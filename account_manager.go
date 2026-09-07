package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---- 账号管理：本地客户端导入 / 粘贴导入 / 一键切回 ----

// AccountManager 账号来源管理
type AccountManager struct {
	db    *DB
	zapi  *ZCodeAPI
	oauth *OAuthManager
}

// NewAccountManager 创建账号管理器
func NewAccountManager(db *DB, zapi *ZCodeAPI, oauth *OAuthManager) *AccountManager {
	return &AccountManager{db: db, zapi: zapi, oauth: oauth}
}

// ---- 本地客户端导入 ----

// localClientFiles 本地 ZCode 客户端相关文件路径
type localClientFiles struct {
	home        string
	credentials string // ~/.zcode/v2/credentials.json
	config      string // ~/.zcode/v2/config.json
	telemetry   string // ~/.zcode/v2/telemetry-state.json
	cache       string // ~/.zcode/v2/coding-plan-cache.json
}

func resolveLocalClientFiles() localClientFiles {
	home, _ := os.UserHomeDir()
	v2 := filepath.Join(home, ".zcode", "v2")
	return localClientFiles{
		home:        home,
		credentials: filepath.Join(v2, "credentials.json"),
		config:      filepath.Join(v2, "config.json"),
		telemetry:   filepath.Join(v2, "telemetry-state.json"),
		cache:       filepath.Join(v2, "coding-plan-cache.json"),
	}
}

// ImportFromLocalClient 从本机 ZCode 客户端导入当前登录账号。
// 解密 credentials.json 提取 JWT/access_token/user_info；
// 从 config.json 提取 coding-plan API Key；保留原始文件内容供一键切回。
func (m *AccountManager) ImportFromLocalClient(group string) (*Account, error) {
	f := resolveLocalClientFiles()
	credData, err := os.ReadFile(f.credentials)
	if err != nil {
		return nil, fmt.Errorf("读取本地凭证失败（ZCode 客户端可能未安装/未登录）: %w", err)
	}
	var creds map[string]string
	if err := json.Unmarshal(credData, &creds); err != nil {
		return nil, fmt.Errorf("凭证文件解析失败: %w", err)
	}

	secret := DefaultCredentialSecret(f.home)
	dec := func(key string) string {
		v, ok := creds[key]
		if !ok {
			return ""
		}
		plain, err := DecryptCredential(v, secret)
		if err != nil {
			log.Printf("[import] decrypt %s: %v", key, err)
			return ""
		}
		return plain
	}

	provider := dec("oauth:active_provider")
	if provider == "" {
		provider = "zai"
	}
	zcodeJWT := dec("zcodejwttoken")
	accessToken := dec(fmt.Sprintf("oauth:%s:access_token", provider))
	userInfo := dec(fmt.Sprintf("oauth:%s:user_info", provider))
	if zcodeJWT == "" && accessToken == "" {
		return nil, fmt.Errorf("本地凭证中没有可用的 ZCode 登录态（请先在 ZCode 客户端登录）")
	}

	// user_info → email/name/user_id
	var ui map[string]interface{}
	if userInfo != "" {
		json.Unmarshal([]byte(userInfo), &ui)
	}
	email := jsonStr(ui, "email")
	displayName := firstNonEmpty(jsonStr(ui, "name"), jsonStr(ui, "username"), jsonStr(ui, "displayName"))
	userID := firstNonEmpty(jsonStr(ui, "user_id"), jsonStr(ui, "id"))
	if userID == "" && zcodeJWT != "" {
		if claims, err := DecodeJWTPayload(zcodeJWT); err == nil {
			userID = firstNonEmpty(jsonStr(claims, "user_id"), jsonStr(claims, "sub"))
		}
	}
	if userID == "" && accessToken != "" {
		if claims, err := DecodeJWTPayload(accessToken); err == nil {
			userID = firstNonEmpty(jsonStr(claims, "user_id"), jsonStr(claims, "sub"))
		}
	}
	if userID == "" {
		return nil, fmt.Errorf("无法确定账号 user_id")
	}

	// device_mid
	deviceMid := ""
	if tData, err := os.ReadFile(f.telemetry); err == nil {
		var t struct {
			DeviceMid string `json:"deviceMid"`
		}
		if json.Unmarshal(tData, &t) == nil {
			deviceMid = t.DeviceMid
		}
	}

	// config.json → coding-plan API Key（已是 {id}.{secret} 完整格式）
	apiKey := ""
	if cData, err := os.ReadFile(f.config); err == nil {
		var cfg struct {
			Provider map[string]struct {
				Enabled *bool `json:"enabled"`
				Options struct {
					APIKey string `json:"apiKey"`
				} `json:"options"`
			} `json:"provider"`
		}
		if json.Unmarshal(cData, &cfg) == nil {
			for id, p := range cfg.Provider {
				if strings.Contains(id, "coding-plan") && strings.Contains(id, provider) &&
					p.Options.APIKey != "" && !strings.HasPrefix(p.Options.APIKey, "enc:") {
					apiKey = p.Options.APIKey
					break
				}
			}
		}
	}

	// 原始凭证快照（供一键切回还原）
	snapshot := map[string]string{
		"credentials.json": string(credData),
	}
	if cData, err := os.ReadFile(f.config); err == nil {
		snapshot["config.json"] = string(cData)
	}
	snapshotJSON, _ := json.Marshal(snapshot)

	a := &Account{
		UserID:      userID,
		Email:       email,
		DisplayName: displayName,
		Provider:    provider,
		AuthType:    "jwt",
		AccessToken: accessToken,
		ZCodeJWT:    zcodeJWT,
		APIKey:      apiKey,
		UserInfo:    userInfo,
		DeviceMid:   deviceMid,
		CredsRaw:    string(snapshotJSON),
		Status:      StatusActive,
		Enabled:     true,
		AccountGroup: group,
		Remark:      "本地客户端导入",
	}
	if zcodeJWT == "" {
		a.AuthType = "apikey"
	}
	id, err := m.db.UpsertAccount(a)
	if err != nil {
		return nil, fmt.Errorf("账号入库失败: %w", err)
	}
	a.ID = id
	log.Printf("[import] local client account imported: %s (id=%d, jwt=%v, apikey=%v)",
		email, id, zcodeJWT != "", apiKey != "")

	// 异步刷新额度
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := m.zapi.RefreshAccountQuota(a); err != nil {
			log.Printf("[import] quota refresh %s: %v", email, err)
		}
	}()
	return a, nil
}

// ---- 粘贴导入 ----

// LooksLikeJWT 判断凭证是否为 JWT 形状（3 段 base64url）
func LooksLikeJWT(secret string) bool {
	parts := strings.Split(strings.TrimSpace(secret), ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", c) {
				return false
			}
		}
	}
	return true
}

// ImportPasted 粘贴 JWT 或 API Key 导入
func (m *AccountManager) ImportPasted(provider, name, secret, group string) (*Account, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, fmt.Errorf("凭证不能为空")
	}
	if provider == "" {
		provider = "zai"
	}
	isJWT := LooksLikeJWT(secret) && provider == "zai"

	// user_id：JWT 解 payload；API Key 用哈希做自然键
	userID := ""
	if isJWT {
		if claims, err := DecodeJWTPayload(secret); err == nil {
			userID = firstNonEmpty(jsonStr(claims, "user_id"), jsonStr(claims, "sub"))
		}
	}
	if userID == "" {
		h := sha256.Sum256([]byte(provider + ":" + secret))
		userID = provider + "-" + hex.EncodeToString(h[:8])
	}

	a := &Account{
		UserID:       userID,
		DisplayName:  strings.TrimSpace(name),
		Email:        "",
		Provider:     provider,
		Status:       StatusActive,
		Enabled:      true,
		AccountGroup: group,
		Remark:       "粘贴导入",
	}
	if isJWT {
		a.AuthType = "jwt"
		a.ZCodeJWT = secret
	} else {
		a.AuthType = "apikey"
		a.APIKey = secret
	}
	id, err := m.db.UpsertAccount(a)
	if err != nil {
		return nil, fmt.Errorf("账号入库失败: %w", err)
	}
	a.ID = id

	go func() {
		time.Sleep(500 * time.Millisecond)
		m.zapi.RefreshAccountQuota(a)
	}()
	return a, nil
}

// ---- 一键切回本地客户端 ----

// SwitchBackToLocal 把所选账号凭证写回本地 ZCode 客户端：
//  1. 快照现有 credentials.json / config.json 到 data/backups/
//  2. 重新 enc:v1 加密写回 credentials.json（原子替换）
//  3. 更新 config.json 的 start-plan / coding-plan apiKey 与 enabled
//  4. 删除 coding-plan-cache.json 强制客户端重新探测套餐
func (m *AccountManager) SwitchBackToLocal(accountID int64, killClient bool) error {
	a, err := m.db.GetAccount(accountID)
	if err != nil {
		return err
	}
	if a.ZCodeJWT == "" {
		return fmt.Errorf("该账号没有 ZCode JWT，无法切回本地客户端")
	}
	f := resolveLocalClientFiles()
	secret := DefaultCredentialSecret(f.home)

	// 1. 备份（基于可执行文件目录，避免受工作目录影响）
	backupDir := filepath.Join(exeDir(), "data", "backups")
	os.MkdirAll(backupDir, 0755)
	stamp := time.Now().Format("20060102-150405")
	for _, p := range []string{f.credentials, f.config} {
		if data, err := os.ReadFile(p); err == nil {
			os.WriteFile(filepath.Join(backupDir, filepath.Base(p)+"."+stamp+".bak"), data, 0644)
		}
	}

	// 2. 重建 credentials.json：保留无关键（bot/web-remote-control 等），只替换登录态
	var creds map[string]string
	if data, err := os.ReadFile(f.credentials); err == nil {
		json.Unmarshal(data, &creds)
	}
	if creds == nil {
		creds = map[string]string{}
	}
	provider := a.Provider
	if provider == "" {
		provider = "zai"
	}
	enc := func(plain string) (string, error) { return EncryptCredential(plain, secret) }

	if v, err := enc(a.ZCodeJWT); err == nil {
		creds["zcodejwttoken"] = v
	}
	if a.AccessToken != "" {
		if v, err := enc(a.AccessToken); err == nil {
			creds["oauth:"+provider+":access_token"] = v
		}
	}
	if a.UserInfo != "" {
		if v, err := enc(a.UserInfo); err == nil {
			creds["oauth:"+provider+":user_info"] = v
		}
	}
	if v, err := enc(provider); err == nil {
		creds["oauth:active_provider"] = v
	}
	if err := atomicWriteJSON(f.credentials, creds); err != nil {
		return fmt.Errorf("写回 credentials.json 失败: %w", err)
	}

	// 3. 更新 config.json provider
	var cfg map[string]interface{}
	if data, err := os.ReadFile(f.config); err == nil {
		if json.Unmarshal(data, &cfg) != nil {
			cfg = nil
		}
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	providers, _ := cfg["provider"].(map[string]interface{})
	if providers == nil {
		providers = map[string]interface{}{}
		cfg["provider"] = providers
	}
	setProviderKey := func(id, key string, enable bool) {
		p, _ := providers[id].(map[string]interface{})
		if p == nil {
			p = map[string]interface{}{}
			providers[id] = p
		}
		opts, _ := p["options"].(map[string]interface{})
		if opts == nil {
			opts = map[string]interface{}{}
			p["options"] = opts
		}
		opts["apiKey"] = key
		p["enabled"] = enable
	}
	setProviderKey("builtin:"+provider+"-start-plan", a.ZCodeJWT, true)
	if a.APIKey != "" {
		setProviderKey("builtin:"+provider+"-coding-plan", a.APIKey, true)
	}
	if err := atomicWriteJSON(f.config, cfg); err != nil {
		return fmt.Errorf("写回 config.json 失败: %w", err)
	}

	// 4. 清缓存强制重新探测
	os.Remove(f.cache)

	log.Printf("[switch-back] account %s written to local client", a.Email)

	// 5. 可选：结束 ZCode 进程让改动生效
	if killClient {
		killZCodeProcess()
	}
	return nil
}

// atomicWriteJSON 临时文件 + rename 原子写
func atomicWriteJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp-zproxy"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// killZCodeProcess 结束本机 ZCode 客户端进程（平台实现见 proc_windows.go / proc_other.go）
func killZCodeProcess() {
	out, err := killProcessByName("ZCode.exe")
	if err != nil {
		log.Printf("[switch-back] kill ZCode.exe: %v (%s)", err, strings.TrimSpace(out))
		return
	}
	log.Printf("[switch-back] ZCode.exe terminated")
}

// exeDir 返回可执行文件所在目录（备份/数据路径基准）
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// RestoreLocalFromSnapshot 用导入时的快照还原本地客户端（撤销切回）
func (m *AccountManager) RestoreLocalFromSnapshot(accountID int64) error {
	a, err := m.db.GetAccount(accountID)
	if err != nil {
		return err
	}
	if a.CredsRaw == "" {
		return fmt.Errorf("该账号没有本地凭证快照，无法还原")
	}
	var snapshot map[string]string
	if err := json.Unmarshal([]byte(a.CredsRaw), &snapshot); err != nil {
		return fmt.Errorf("快照解析失败: %w", err)
	}
	f := resolveLocalClientFiles()
	targets := map[string]string{
		"credentials.json": f.credentials,
		"config.json":      f.config,
	}
	for name, content := range snapshot {
		path, ok := targets[name]
		if !ok || content == "" {
			continue
		}
		tmp := path + ".tmp-zproxy"
		if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	os.Remove(f.cache)
	log.Printf("[switch-back] local client restored from snapshot of account %s", a.Email)
	return nil
}
