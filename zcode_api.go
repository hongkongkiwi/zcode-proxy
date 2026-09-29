package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---- ZCode 上游 API 封装 ----
// 持有账号池 / 验证码服务 / 出口代理，提供额度刷新、活动领取、聊天转发。

type ZCodeAPI struct {
	cfg        *FileConfig
	db         *DB
	pool       *AccountPool
	captcha    *CaptchaService
	egress     *EgressProxy
	routing    *EndpointRouter
	appVersion string

	claimMu    sync.Mutex
	claimLocks map[int64]*sync.Mutex // 账号级领取互斥（防 UI+cron 并发双领）

	resetSyncMu sync.Mutex
	resetSyncAt map[int64]time.Time // 上游重置历史同步节流（每账号）

	quotaRefreshInflight sync.Map // 账号ID → 刷新中（单飞，防并发请求对 billing 形成风暴）

	tokenRefreshInflight sync.Map // 账号ID → refresh_token 兑换中（单飞）
}

// NewZCodeAPI 创建上游 API 封装，并把额度刷新函数注入账号池
func NewZCodeAPI(cfg *FileConfig, db *DB, pool *AccountPool, captcha *CaptchaService, appVersion string) *ZCodeAPI {
	z := &ZCodeAPI{
		cfg:         cfg,
		db:          db,
		pool:        pool,
		captcha:     captcha,
		egress:      NewEgressProxy(db),
		routing:     NewEndpointRouter(""),
		appVersion:  appVersion,
		claimLocks:  make(map[int64]*sync.Mutex),
		resetSyncAt: make(map[int64]time.Time),
	}
	pool.SetQuotaFetcher(func(a *Account) error {
		err := z.RefreshAccountQuota(a)
		if n, serr := z.SyncResetHistoryFromUpstream(a); serr != nil {
			log.Printf("[reset] 上游重置历史同步失败 account=%s: %v", a.Email, serr)
		} else if n > 0 {
			log.Printf("[reset] 同步到 %d 条上游重置记录 account=%s", n, a.Email)
		}
		return err
	})
	return z
}

// RefreshAccountQuota 拉取额度 → 应用状态迁移 → 落库
func (z *ZCodeAPI) RefreshAccountQuota(a *Account) error {
	ov, err := z.FetchQuotaRaw(a)
	if err != nil {
		return err
	}
	z.applyQuotaResult(a, ov)
	return nil
}

// applyQuotaResult 根据额度结果驱动状态机（quota.py 状态迁移移植）
func (z *ZCodeAPI) applyQuotaResult(a *Account, ov *QuotaOverview) {
	switch {
	case ov.AuthFailed:
		// 凭证失效：先尝试 refresh_token 兑换新 JWT，成功则恢复而非判死
		if z.tryRefreshAccount(a) {
			return
		}
		z.pool.MarkInvalid(a, "额度接口鉴权失败（401/403），凭证可能已过期")
		return
	case ov.NotEntitled:
		z.pool.MarkInactive(a, "Coding Plan 未激活（不存在订阅资格）")
		return
	case ov.AllExhausted():
		z.pool.MarkExhausted(a, "额度已用完")
	default:
		// 有剩余额度：cooling 到期 / exhausted / inactive / invalid（凭证其实有效）恢复 active
		if a.tryRecoverActive() {
			z.db.SetAccountStatus(a.ID, StatusActive, "", 0)
			log.Printf("[quota] account %s recovered -> active", a.Email)
		}
	}

	// 持久化额度快照
	quotaJSON, _ := json.Marshal(ov)
	total, used, remaining := 0.0, 0.0, 0.0
	if ov.Total != nil {
		total = *ov.Total
	}
	if ov.Used != nil {
		used = *ov.Used
	}
	if ov.Remaining != nil {
		remaining = *ov.Remaining
	}
	a.setQuota(string(quotaJSON), ov.PlanTier, ov.PlanExpire, total, used, remaining)
	if err := z.db.SetAccountQuota(a.ID, string(quotaJSON), ov.PlanTier, ov.PlanExpire, total, used, remaining); err != nil {
		log.Printf("[quota] persist %s: %v", a.Email, err)
	}
}

// tryRefreshAccount 用库中 refresh_token 兑换新 JWT（每账号单飞）。
// 成功时持久化新凭证并把账号恢复 active；任何失败返回 false，
// 调用方沿用原 401 处理（MarkInvalid），不会更糟。
// 说明：上游刷新请求形状无仓库内样例，按 token 端点的 JSON 信封风格构造；
// 若形状不符，兑换失败并退回手动重登路径。
func (z *ZCodeAPI) tryRefreshAccount(a *Account) bool {
	if a.Provider != "zai" || a.RefreshToken == "" {
		return false
	}
	if _, busy := z.tokenRefreshInflight.LoadOrStore(a.ID, struct{}{}); busy {
		return false
	}
	defer z.tokenRefreshInflight.Delete(a.ID)

	data, err := z.refreshTokenRequest(a.RefreshToken)
	if err != nil {
		log.Printf("[oauth] refresh %s failed: %v", a.Email, err)
		return false
	}
	jwt := jsonStr(data, "token")
	if jwt == "" {
		log.Printf("[oauth] refresh %s: 响应不含新 JWT", a.Email)
		return false
	}
	zai, _ := data["zai"].(map[string]interface{})
	newRefresh := firstNonEmpty(jsonStr(zai, "refresh_token"), a.RefreshToken) // 上游可能不轮换
	newAccess := jsonStr(zai, "access_token")
	if err := z.db.UpdateAccountTokens(a.ID, newAccess, newRefresh, jwt, "", ""); err != nil {
		log.Printf("[oauth] refresh %s persist: %v", a.Email, err)
		return false
	}
	a.setRuntime(StatusActive, "", 0)
	log.Printf("[oauth] refreshed JWT for %s via refresh_token", a.Email)
	return true
}

// refreshTokenRequest POST /api/v1/oauth/token（refresh_token 兑换）
func (z *ZCodeAPI) refreshTokenRequest(refreshToken string) (map[string]interface{}, error) {
	payload, _ := json.Marshal(map[string]string{
		"provider":      "zai",
		"refresh_token": refreshToken,
	})
	client := ClientForURL(z.egress.GlobalProxyURL(), OAuthTokenURL, 30*time.Second)
	resp, err := client.Post(OAuthTokenURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v map[string]interface{}
	json.Unmarshal(body, &v)
	if len(v) == 0 && resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if codeVal, has := v["code"]; has {
		if n := jsonInt(v, "code"); n != 0 && n != 200 {
			msg := firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message"), truncate(string(body), 200))
			return nil, fmt.Errorf("业务码 %d: %s", n, msg)
		}
		_ = codeVal
	}
	data, _ := v["data"].(map[string]interface{})
	if data == nil {
		return nil, fmt.Errorf("响应缺少 data")
	}
	return data, nil
}

// HandleCountTokens POST /v1/messages/count_tokens — Anthropic SDK 会探测该端点。
// 网关不做精确 tokenize，返回保守估计（字符数/4 + 消息开销），避免 SDK 报 405。
func (z *ZCodeAPI) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Messages json.RawMessage `json:"messages"`
		System   json.RawMessage `json:"system"`
		Tools    json.RawMessage `json:"tools"`
	}
	// body 可选（全空按 0 估算），但必须是合法 JSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	chars := len(body.Messages) + len(body.System) + len(body.Tools)
	est := chars/4 + 8
	writeJSON(w, http.StatusOK, map[string]interface{}{"input_tokens": est})
}

// HandleModels GET /v1/models — 同时兼容 OpenAI 与 Anthropic 字段
func (z *ZCodeAPI) HandleModels(w http.ResponseWriter, r *http.Request) {
	models := z.cfg.GetModels()
	// DB 设置可覆盖模型清单
	if extra, _ := z.db.GetSetting("gateway_models"); strings.TrimSpace(extra) != "" {
		var list []string
		for _, m := range strings.Split(extra, ",") {
			if m = strings.TrimSpace(m); m != "" {
				list = append(list, m)
			}
		}
		if len(list) > 0 {
			models = list
		}
	}
	now := time.Now().Unix()
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]interface{}{
			"id":           m,
			"object":       "model", // OpenAI 客户端校验字段
			"type":         "model", // Anthropic 客户端校验字段
			"display_name": m,
			"created":      now,
			"created_at":   "2025-01-01T00:00:00Z",
			"owned_by":     "zcode-proxy",
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"object": "list", "data": data})
}
