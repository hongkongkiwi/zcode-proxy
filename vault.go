package main

// ---- 凭证静态加密（B3，对齐 zcode-api credentials.json 的静态加密实践）----
//
// SQLite accounts 表中的凭证列（access_token / refresh_token / zcode_jwt /
// api_key / user_info / creds_raw）以 vault1: 信封静态加密存储：
//   "vault1:" + b64url(nonce12) + "." + b64url(tag16) + "." + b64url(ct)
// 密钥 = SHA-256(seed)，seed = 环境变量 ZCODE_PROXY_VAULT_SECRET，
// 否则回退 "zcode-proxy-vault:{平台}:{home}:{用户名}"（与客户端 enc:v1 的
// 派生方式同构但前缀不同，两种密文互不通用）。
// 读取透明解密；写入透明加密；启动时一次性迁移存量明文行（幂等）。

import (
	"fmt"
	"log"
	"os"
	"strings"
)

const vaultPrefix = "vault1:"

// vaultCredentialColumns 需要静态加密的凭证列
var vaultCredentialColumns = []string{
	"access_token", "refresh_token", "zcode_jwt", "api_key", "user_info", "creds_raw",
}

// VaultSecret 计算库内凭证加密种子
func VaultSecret() string {
	if s := os.Getenv("ZCODE_PROXY_VAULT_SECRET"); s != "" {
		return s
	}
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

func vaultEncrypt(plain string) string {
	if plain == "" || strings.HasPrefix(plain, vaultPrefix) {
		return plain
	}
	enc, err := EncryptCredential(plain, VaultSecret())
	if err != nil {
		log.Printf("[vault] encrypt failed: %v", err)
		return plain
	}
	return vaultPrefix + strings.TrimPrefix(enc, encPrefix)
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
