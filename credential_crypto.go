package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ---- ZCode 本地凭证 enc:v1 加解密 ----
// 格式: "enc:v1:" + b64url(nonce12) + "." + b64url(tag16) + "." + b64url(ciphertext)
// 密钥: SHA-256(secret)，secret = 环境变量 ZCODE_CREDENTIAL_SECRET
//       或回退 "zcode-credential-fallback:{node平台}:{home}:{用户名}"
// 与 zcode-switch/src-tauri/src/zcrypto.rs 逐字节兼容。

const encPrefix = "enc:v1:"

var b64URLNoPad = base64.RawURLEncoding

// NodePlatform 返回 Node.js 语义的平台名（与 ZCode 客户端一致）
func NodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return runtime.GOOS
	}
}

// DefaultCredentialSecret 计算默认凭证密钥。
// home 为空时自动取 USERPROFILE / HOME。
func DefaultCredentialSecret(home string) string {
	if s := os.Getenv("ZCODE_CREDENTIAL_SECRET"); s != "" {
		return s
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	username := os.Getenv("USERNAME") // Windows
	if username == "" {
		username = os.Getenv("USER")
	}
	if username == "" {
		username = os.Getenv("LOGNAME")
	}
	if username == "" {
		username = "unknown"
	}
	return fmt.Sprintf("zcode-credential-fallback:%s:%s:%s", NodePlatform(), home, username)
}

func deriveKey(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// IsEncryptedValue 判断值是否为 enc:v1 密文
func IsEncryptedValue(v string) bool {
	return strings.HasPrefix(v, encPrefix)
}

// DecryptCredential 解密 enc:v1 值；明文原样返回
func DecryptCredential(value, secret string) (string, error) {
	if !IsEncryptedValue(value) {
		return value, nil
	}
	body := strings.TrimPrefix(value, encPrefix)
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("enc:v1 格式不正确")
	}
	nonce, err := b64URLNoPad.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("nonce 解码失败: %w", err)
	}
	tag, err := b64URLNoPad.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("tag 解码失败: %w", err)
	}
	ct, err := b64URLNoPad.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("密文解码失败: %w", err)
	}
	if len(nonce) != 12 {
		return "", fmt.Errorf("nonce 长度异常: %d", len(nonce))
	}
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	// Rust aes-gcm 输出 ct||tag，Go GCM Open 同样期望 ct||tag
	buf := append(append([]byte{}, ct...), tag...)
	pt, err := gcm.Open(nil, nonce, buf, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不匹配或数据损坏）")
	}
	return string(pt), nil
}

// EncryptCredential 加密为 enc:v1 格式（一键切回本地客户端时写回用）
func EncryptCredential(plain, secret string) (string, error) {
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plain), nil) // ct||tag
	ct, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	return fmt.Sprintf("%s%s.%s.%s", encPrefix,
		b64URLNoPad.EncodeToString(nonce),
		b64URLNoPad.EncodeToString(tag),
		b64URLNoPad.EncodeToString(ct)), nil
}

// DecodeJWTPayload 解出 JWT payload 的 JSON（不验签，仅读取声明）
func DecodeJWTPayload(jwt string) (map[string]interface{}, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("不是 JWT 格式")
	}
	payload, err := b64URLNoPad.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ZCodeHome 返回本机 ZCode 数据目录（~/.zcode）
func ZCodeHome() string {
	if h := os.Getenv("ZCODE_SWITCH_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".zcode")
}

// LocalCredentialsPath 本地客户端凭证文件路径
func LocalCredentialsPath() string {
	h := ZCodeHome()
	if h == "" {
		return ""
	}
	return filepath.Join(h, "v2", "credentials.json")
}

// LocalTelemetryPath 本地 telemetry-state.json（deviceMid 来源）
func LocalTelemetryPath() string {
	h := ZCodeHome()
	if h == "" {
		return ""
	}
	return filepath.Join(h, "v2", "telemetry-state.json")
}
