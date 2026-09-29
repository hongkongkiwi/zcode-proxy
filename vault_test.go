package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func newVaultTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault-test.db")
	t.Cleanup(resetVaultSeed) // 进程内包级种子不得跨测试泄漏
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// resetVaultSeed 模拟进程重启：清掉内存中的种子覆盖，让下一次 NewDB
// 走完整的 ResolveVaultSeed（含 keyfile 读取）
func resetVaultSeed() { setVaultSeedOverride("") }

func TestVaultEncryptOnWriteDecryptOnRead(t *testing.T) {
	db, _ := newVaultTestDB(t)
	a := &Account{
		UserID: "u-vault-1", Email: "vault@test", Provider: "zai", AuthType: "jwt",
		AccessToken: "at-secret", RefreshToken: "rt-secret", ZCodeJWT: "jwt-secret",
		APIKey: "key.secret", UserInfo: `{"email":"vault@test"}`, CredsRaw: `{"zcodejwttoken":"x"}`,
		Status: StatusActive, Enabled: true,
	}
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 原始行必须是密文
	var raw string
	if err := db.conn.QueryRow(`SELECT zcode_jwt FROM accounts WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !strings.HasPrefix(raw, vaultPrefix) {
		t.Fatalf("stored value not vault-encrypted: %q", raw)
	}
	if strings.Contains(raw, "jwt-secret") {
		t.Fatal("plaintext leaked into stored value")
	}
	// 读回必须透明解密
	got, err := db.GetAccount(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ZCodeJWT != "jwt-secret" || got.AccessToken != "at-secret" || got.CredsRaw != `{"zcodejwttoken":"x"}` {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	// UpdateAccountTokens 同样加密；空值保留旧值
	if err := db.UpdateAccountTokens(id, "at-new", "", "", "", ""); err != nil {
		t.Fatalf("update tokens: %v", err)
	}
	got, err = db.GetAccount(id)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.AccessToken != "at-new" || got.RefreshToken != "rt-secret" {
		t.Fatalf("token update mismatch: at=%q rt=%q", got.AccessToken, got.RefreshToken)
	}
}

func TestVaultMigratesPlaintextRows(t *testing.T) {
	db, path := newVaultTestDB(t)
	// 绕过加密直接插入明文（模拟旧库）
	res, err := db.conn.Exec(`INSERT INTO accounts (user_id, email, provider, auth_type, zcode_jwt, status, enabled)
		VALUES ('u-legacy', 'legacy@test', 'zai', 'jwt', 'legacy-plain-jwt', 'active', 1)`)
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	id, _ := res.LastInsertId()
	db.Close()
	resetVaultSeed() // 模拟进程重启

	// 重新打开触发迁移
	db2, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { db2.Close(); resetVaultSeed() }()
	var raw string
	if err := db2.conn.QueryRow(`SELECT zcode_jwt FROM accounts WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !strings.HasPrefix(raw, vaultPrefix) {
		t.Fatalf("migration did not encrypt: %q", raw)
	}
	got, err := db2.GetAccount(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ZCodeJWT != "legacy-plain-jwt" {
		t.Fatalf("decrypt after migration mismatch: %q", got.ZCodeJWT)
	}
	// 再开一次：已加密值不重复处理（幂等），且 keyfile 种子被正确采用
	db2.Close()
	resetVaultSeed()
	db3, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	defer func() { db3.Close(); resetVaultSeed() }()
	got, err = db3.GetAccount(id)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.ZCodeJWT != "legacy-plain-jwt" {
		t.Fatalf("idempotency broken: %q", got.ZCodeJWT)
	}
}

// TestVaultKeyPersistsAcrossRestart 回归测试：vault.key 必须在重启后被读取采用。
// （历史缺陷：ResolveVaultSeed 只写不读，第二次重启后全部凭证解密失败）
func TestVaultKeyPersistsAcrossRestart(t *testing.T) {
	db, path := newVaultTestDB(t)
	a := &Account{
		UserID: "u-restart", Email: "restart@test", Provider: "zai", AuthType: "jwt",
		ZCodeJWT: "jwt-across-restart", Status: StatusActive, Enabled: true,
	}
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	db.Close()

	// 两次重启：种子都必须从 vault.key 恢复，而非回落到派生种子
	for round := 1; round <= 2; round++ {
		resetVaultSeed()
		db2, err := NewDB(path)
		if err != nil {
			t.Fatalf("reopen round %d: %v", round, err)
		}
		got, err := db2.GetAccount(id)
		db2.Close()
		resetVaultSeed()
		if err != nil {
			t.Fatalf("get round %d: %v", round, err)
		}
		if got.ZCodeJWT != "jwt-across-restart" {
			t.Fatalf("round %d: credential not decryptable after restart (keyfile not adopted?): %q", round, got.ZCodeJWT)
		}
	}
}
