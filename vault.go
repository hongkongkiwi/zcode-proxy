package main

// ---- 凭证静态加密（B3，对齐 zcode-api credentials.json 的静态加密实践）----
//
// SQLite accounts 表中的凭证列（access_token / refresh_token / zcode_jwt /
// api_key / user_info / creds_raw）以 vault1: 信封静态加密存储：
//   "vault1:" + b64url(nonce12) + "." + b64url(tag16) + "." + b64url(ct)
// 密钥 = SHA-256(seed)。seed 解析优先级（ResolveVaultSeed，启动时执行一次）：
//   1. 环境变量 ZCODE_PROXY_VAULT_SECRET
//   2. DB 同目录 vault.key（首次自动生成随机密钥，0600）
//   3. 旧版平台派生种子仅作迁移源：检测到旧密文且可解时重加密至 vault.key
// 读取透明解密；写入透明加密（失败中止写入）；启动时一次性迁移存量明文行（幂等），
// 并做解密健康检查（密文解不开时大声告警）。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const vaultPrefix = "vault1:"

// vaultCredentialColumns 需要静态加密的凭证列
var vaultCredentialColumns = []string{
	"access_token", "refresh_token", "zcode_jwt", "api_key", "user_info", "creds_raw",
}

// vaultSeedOverride 启动时解析出的当前种子（keyfile 或环境变量）；
// 在任何账号读写发生前的 main 启动序列中设置，此后只读。
var (
	vaultSeedMu       sync.RWMutex
	vaultSeedOverride string
)

func setVaultSeedOverride(s string) {
	vaultSeedMu.Lock()
	vaultSeedOverride = s
	vaultSeedMu.Unlock()
}

func currentVaultSeed() string {
	vaultSeedMu.RLock()
	defer vaultSeedMu.RUnlock()
	if vaultSeedOverride != "" {
		return vaultSeedOverride
	}
	return legacyVaultSeed()
}

// vaultKeyFile DB 同目录的密钥文件
func vaultKeyFile(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), "vault.key")
}

// randomVaultSeed 生成 32 字节随机种子的 hex 表示
func randomVaultSeed() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// loadVaultKeyFile 读取并校验已存在的密钥文件，返回 (seed, ok)
func loadVaultKeyFile(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(raw))
	if len(s) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", false
	}
	return s, true
}

// writeVaultKeyFile 新建密钥文件（0600）。仅应在确认文件不存在时调用，
// 绝不覆盖已有密钥——覆盖等于销毁存量密文唯一的钥匙。
// 注：Windows 上 Go 的 Chmod 只映射只读位，0600 限制在 win32 不生效（ACL 限制需另做）。
func writeVaultKeyFile(path, seed string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("vault key file %s 已存在，拒绝覆盖", path)
	}
	if err := os.WriteFile(path, []byte(seed+"\n"), 0600); err != nil {
		return err
	}
	// umask 可能放宽权限，显式收紧
	os.Chmod(path, 0600)
	return nil
}

// ResolveVaultSeed 启动时决定凭证加密种子，优先级：
//  1. ZCODE_PROXY_VAULT_SECRET 环境变量（显式指定，不读不写密钥文件）
//  2. DB 同目录 vault.key（存在则直接采用；不存在且库中无密文时生成随机密钥）
//  3. 旧版平台派生种子（可从平台/用户名推算，仅作为一次性迁移源：
//     库中有密文、派生种子能解开全部密文且无 keyfile 时，生成 keyfile 并重加密）
//
// 库中存在密文但当前候选钥匙解不开时，保持派生种子并大声告警，绝不换钥匙——
// 换钥匙会让存量密文永久不可解。重加密失败时回滚种子并删除刚建的 keyfile，
// 避免下次启动采用一把和库中数据不匹配的钥匙。
func ResolveVaultSeed(db *DB, dbPath string) {
	if s := os.Getenv("ZCODE_PROXY_VAULT_SECRET"); s != "" {
		setVaultSeedOverride(s)
		return
	}
	keyFile := vaultKeyFile(dbPath)
	legacy := legacyVaultSeed()

	// 1. 已有 keyfile：直接采用（这是重启后的常规路径）
	if keyFile != "" {
		if seed, ok := loadVaultKeyFile(keyFile); ok {
			setVaultSeedOverride(seed)
			return
		}
		if _, statErr := os.Stat(keyFile); statErr == nil {
			log.Printf("[vault] WARNING: key file %s 存在但格式非法，忽略（如需重置请手动删除）", keyFile)
		}
	}

	// 2. 全量盘点库中密文在派生种子下的可解性
	total, broken := db.scanVaultCiphertext(legacy)
	if total == 0 {
		// 无存量密文：生成随机 keyfile 并启用
		if keyFile == "" {
			return // 无法定位 keyfile（dbPath 为空）：留在派生种子
		}
		seed, err := randomVaultSeed()
		if err == nil {
			err = writeVaultKeyFile(keyFile, seed)
			if err == nil {
				log.Printf("[vault] generated random vault key: %s", keyFile)
				setVaultSeedOverride(seed)
				return
			}
		}
		log.Printf("[vault] key file unavailable (%v); falling back to derived key", errOr(err))
		return
	}
	if broken > 0 {
		// 有密文但派生种子解不开：曾用 env 加密 / DB 换机。绝不盲目换新钥匙。
		log.Printf("[vault] WARNING: %d of %d encrypted credential values cannot be decrypted with the derived key. "+
			"If ZCODE_PROXY_VAULT_SECRET was previously set, run with it again; "+
			"if the database was moved from another machine/user, restore its vault.key. "+
			"Affected accounts will appear credential-less until the key is restored.", broken, total)
		return
	}
	// 3. 派生种子可解开全部密文：一次性轮换到随机 keyfile
	if keyFile == "" {
		return
	}
	newSeed, err := randomVaultSeed()
	if err != nil {
		log.Printf("[vault] generate new key: %v; staying on derived key", err)
		return
	}
	if err := writeVaultKeyFile(keyFile, newSeed); err != nil {
		log.Printf("[vault] write key file: %v; staying on derived key", err)
		return
	}
	if _, err := db.reencryptVaultColumns(legacy, newSeed); err != nil {
		// 回滚：恢复派生种子并删掉刚建的 keyfile，否则下次启动会采用
		// 一把和库中数据不匹配的钥匙
		os.Remove(keyFile)
		log.Printf("[vault] key rotation failed (%v); rolled back to derived key and removed %s", err, keyFile)
		return
	}
	setVaultSeedOverride(newSeed)
	log.Printf("[vault] rotated credential values from derived key to random key file %s", keyFile)
}

func errOr(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// scanVaultCiphertext 全量扫描库中 vault1 密文，返回 (总数, 在给定种子下解密失败的条数)。
// 轮换决策必须全量：抽样会漏掉个别损坏/异源行，导致重加密中途失败。
func (db *DB) scanVaultCiphertext(seed string) (total, broken int) {
	for _, col := range vaultCredentialColumns {
		rows, err := db.conn.Query(`SELECT ` + col + ` FROM accounts WHERE ` + col + ` LIKE '` + vaultPrefix + `%'`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var val string
			if rows.Scan(&val) != nil || !strings.HasPrefix(val, vaultPrefix) {
				continue
			}
			total++
			if _, err := DecryptCredential(encPrefix+strings.TrimPrefix(val, vaultPrefix), seed); err != nil {
				broken++
			}
		}
		rows.Close()
	}
	return total, broken
}

// reencryptVaultColumns 用 oldSeed 解密、newSeed 重加密所有 vault1 凭证列。
// 单事务执行：任一行失败整体回滚，库里不会出现两种钥匙混存的半迁移状态。
func (db *DB) reencryptVaultColumns(oldSeed, newSeed string) (int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	moved := 0
	for _, col := range vaultCredentialColumns {
		rows, err := tx.Query(`SELECT id, ` + col + ` FROM accounts WHERE ` + col + ` LIKE '` + vaultPrefix + `%'`)
		if err != nil {
			return 0, fmt.Errorf("reencrypt query %s: %w", col, err)
		}
		type pending struct {
			id  int64
			enc string
		}
		var updates []pending
		for rows.Next() {
			var id int64
			var val string
			if err := rows.Scan(&id, &val); err != nil {
				rows.Close()
				return 0, err
			}
			if !strings.HasPrefix(val, vaultPrefix) {
				continue
			}
			plain, err := DecryptCredential(encPrefix+strings.TrimPrefix(val, vaultPrefix), oldSeed)
			if err != nil {
				rows.Close()
				return 0, fmt.Errorf("reencrypt decrypt %s id=%d: %w", col, id, err)
			}
			enc, err := EncryptCredential(plain, newSeed)
			if err != nil {
				rows.Close()
				return 0, fmt.Errorf("reencrypt encrypt %s id=%d: %w", col, id, err)
			}
			updates = append(updates, pending{id: id, enc: vaultPrefix + strings.TrimPrefix(enc, encPrefix)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		for _, u := range updates {
			if _, err := tx.Exec(`UPDATE accounts SET `+col+` = ? WHERE id = ?`, u.enc, u.id); err != nil {
				return 0, fmt.Errorf("reencrypt update %s id=%d: %w", col, u.id, err)
			}
		}
		moved += len(updates)
	}
	return moved, tx.Commit()
}

// ProbeVaultHealth 启动收尾的健康检查：统计当前种子解不开的密文数量并大声告警
func (db *DB) ProbeVaultHealth() {
	_, broken := db.scanVaultCiphertext(currentVaultSeed())
	if broken > 0 {
		log.Printf("[vault] WARNING: %d credential values cannot be decrypted with the active key "+
			"(set ZCODE_PROXY_VAULT_SECRET or restore data/vault.key); affected accounts will fail auth until fixed", broken)
	}
}

// VaultSecret 返回当前凭证加密种子
func VaultSecret() string {
	return currentVaultSeed()
}

// legacyVaultSeed 旧版派生种子（可由平台/home/用户名推算，仅作迁移源）
func legacyVaultSeed() string {
	home, _ := os.UserHomeDir()
	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}
	if username == "" {
		username = os.Getenv("LOGNAME")
	}
	if username == "" {
		username = "unknown"
	}
	return fmt.Sprintf("zcode-proxy-vault:%s:%s:%s", NodePlatform(), home, username)
}

// vaultEncrypt 加密单值；失败必须由调用方中止写入——静默落明文会让
// "加密存储"在加密层故障时变成明文存储且无人察觉
func vaultEncrypt(plain string) (string, error) {
	if plain == "" || strings.HasPrefix(plain, vaultPrefix) {
		return plain, nil
	}
	enc, err := EncryptCredential(plain, VaultSecret())
	if err != nil {
		return "", fmt.Errorf("vault encrypt: %w", err)
	}
	return vaultPrefix + strings.TrimPrefix(enc, encPrefix), nil
}

func vaultDecrypt(value string) string {
	if value == "" || !strings.HasPrefix(value, vaultPrefix) {
		return value
	}
	// vault1: 与 enc:v1 信封布局一致，仅前缀与密钥不同
	out, err := DecryptCredential(encPrefix+strings.TrimPrefix(value, vaultPrefix), VaultSecret())
	if err != nil {
		log.Printf("[vault] decrypt failed: %v", err)
		return ""
	}
	return out
}

// MigrateVault 把存量明文凭证列加密（幂等：已加密值跳过）。启动时调用一次。
func (db *DB) MigrateVault() error {
	secret := VaultSecret()
	for _, col := range vaultCredentialColumns {
		rows, err := db.conn.Query(`SELECT id, ` + col + ` FROM accounts WHERE ` + col + ` != ''`)
		if err != nil {
			return fmt.Errorf("vault migrate query %s: %w", col, err)
		}
		type pending struct {
			id  int64
			enc string
		}
		var updates []pending
		for rows.Next() {
			var id int64
			var val string
			if err := rows.Scan(&id, &val); err != nil {
				rows.Close()
				return err
			}
			if val == "" || strings.HasPrefix(val, vaultPrefix) {
				continue
			}
			enc, err := EncryptCredential(val, secret)
			if err != nil {
				rows.Close()
				return fmt.Errorf("vault encrypt %s id=%d: %w", col, id, err)
			}
			updates = append(updates, pending{id: id, enc: vaultPrefix + strings.TrimPrefix(enc, encPrefix)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, u := range updates {
			if _, err := db.conn.Exec(`UPDATE accounts SET `+col+` = ? WHERE id = ?`, u.enc, u.id); err != nil {
				return fmt.Errorf("vault update %s id=%d: %w", col, u.id, err)
			}
		}
		if len(updates) > 0 {
			log.Printf("[vault] encrypted %d plaintext values in column %s", len(updates), col)
		}
	}
	return nil
}
