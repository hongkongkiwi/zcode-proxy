package main

import (
	"encoding/json"
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
}

// NewZCodeAPI 创建上游 API 封装，并把额度刷新函数注入账号池
func NewZCodeAPI(cfg *FileConfig, db *DB, pool *AccountPool, captcha *CaptchaService, appVersion string) *ZCodeAPI {
	z := &ZCodeAPI{
		cfg:        cfg,
		db:         db,
		pool:       pool,
		captcha:    captcha,
		egress:     NewEgressProxy(db),
		routing:    NewEndpointRouter(""),
		appVersion: appVersion,
		claimLocks: make(map[int64]*sync.Mutex),
	}
	pool.SetQuotaFetcher(z.RefreshAccountQuota)
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
	json.NewDecoder(r.Body).Decode(&body)
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
