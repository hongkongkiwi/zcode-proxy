package main

import (
	"database/sql"
	"strings"
)

// ---- 设置 KV ----

// vaultSecretSettingsSet 机密设置键集合：落库前 vault 加密，读取后透明解密
var vaultSecretSettingsSet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range vaultSecretSettings {
		m[k] = true
	}
	return m
}()

// GetSetting 读取设置项（机密键透明解密）
func (db *DB) GetSetting(key string) (string, error) {
	var v string
	err := db.conn.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if v != "" && vaultSecretSettingsSet[key] {
		return vaultDecrypt(v), nil
	}
	return v, nil
}

// SetSetting 写入设置项（机密键落库前加密；加密失败中止写入）
func (db *DB) SetSetting(key, value string) error {
	if value != "" && vaultSecretSettingsSet[key] && !strings.HasPrefix(value, vaultPrefix) {
		enc, err := vaultEncrypt(value)
		if err != nil {
			return err
		}
		value = enc
	}
	_, err := db.conn.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = datetime('now','localtime')`,
		key, value)
	return err
}

// AllSettings 返回全部设置
func (db *DB) AllSettings() (map[string]string, error) {
	rows, err := db.conn.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// ---- 认证相关设置 ----

func (db *DB) GetPasswordHash() (string, error)  { return db.GetSetting("password_hash") }
func (db *DB) SetPasswordHash(hash string) error { return db.SetSetting("password_hash", hash) }

// IsDefaultPassword 是否仍使用引导期生成的初始口令。
// 标记缺失（老库/未写）一律视为 false——缺省告警只信任显式写入的标记，
// 避免 env 口令部署与升级库被误报警。
func (db *DB) IsDefaultPassword() (bool, error) {
	v, err := db.GetSetting("is_default_password")
	if err != nil || v == "" {
		return false, err
	}
	return v == "1", nil
}

func (db *DB) SetDefaultPasswordFlag(isDefault bool) error {
	v := "0"
	if isDefault {
		v = "1"
	}
	return db.SetSetting("is_default_password", v)
}

func (db *DB) GetAPIKey() (string, error) { return db.GetSetting("api_key") }
func (db *DB) SetAPIKey(key string) error { return db.SetSetting("api_key", key) }

func (db *DB) GetAdminUser() (string, error) {
	u, err := db.GetSetting("admin_user")
	if err != nil || u == "" {
		return "admin", err
	}
	return u, nil
}
