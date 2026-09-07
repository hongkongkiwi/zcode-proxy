package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---- 管理 REST API ----

// APIServer 管理接口服务
type APIServer struct {
	db        *DB
	cfg       *FileConfig
	pool      *AccountPool
	zapi      *ZCodeAPI
	oauth     *OAuthManager
	acctMgr   *AccountManager
	scheduler *CronScheduler
	auth      *AuthManager
	captcha   *CaptchaService
}

// NewAPIServer 创建 API 服务
func NewAPIServer(db *DB, cfg *FileConfig, pool *AccountPool, zapi *ZCodeAPI,
	oauth *OAuthManager, acctMgr *AccountManager, scheduler *CronScheduler,
	auth *AuthManager, captcha *CaptchaService) *APIServer {
	return &APIServer{db: db, cfg: cfg, pool: pool, zapi: zapi, oauth: oauth,
		acctMgr: acctMgr, scheduler: scheduler, auth: auth, captcha: captcha}
}

// RegisterRoutes 注册路由（Go 1.22+ 方法模式）
func (s *APIServer) RegisterRoutes(mux *http.ServeMux) {
	// 认证
	mux.HandleFunc("POST /api/login", s.auth.HandleLogin)
	mux.HandleFunc("POST /api/logout", s.auth.HandleLogout)
	mux.HandleFunc("GET /api/auth/check", s.auth.HandleCheckAuth)
	mux.HandleFunc("POST /api/auth/password", s.auth.HandleChangePassword)
	mux.HandleFunc("GET /api/settings/api-key", s.auth.HandleGetAPIKey)
	mux.HandleFunc("POST /api/settings/api-key/generate", s.auth.HandleGenerateAPIKey)

	// 仪表盘
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)

	// 账号
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts/import/local", s.handleImportLocal)
	mux.HandleFunc("POST /api/accounts/import/paste", s.handleImportPaste)
	mux.HandleFunc("POST /api/accounts/import/bundle", s.handleImportBundle)
	mux.HandleFunc("POST /api/accounts/export", s.handleExportBundle)
	mux.HandleFunc("POST /api/accounts/oauth/start", s.handleOAuthStart)
	mux.HandleFunc("POST /api/accounts/oauth/manual", s.handleOAuthManual)
	mux.HandleFunc("GET /api/accounts/oauth/status", s.handleOAuthStatus)
	mux.HandleFunc("POST /api/accounts/{id}/refresh", s.handleAccountRefresh)
	mux.HandleFunc("POST /api/accounts/{id}/claim", s.handleAccountClaim)
	mux.HandleFunc("POST /api/accounts/{id}/detect", s.handleAccountDetect)
	mux.HandleFunc("POST /api/accounts/{id}/activate", s.handleAccountActivate)
	mux.HandleFunc("POST /api/accounts/{id}/reset", s.handleAccountReset)
	mux.HandleFunc("GET /api/accounts/{id}/reset-status", s.handleResetStatus)
	mux.HandleFunc("POST /api/accounts/{id}/switch-back", s.handleSwitchBack)
	mux.HandleFunc("POST /api/accounts/{id}/restore-local", s.handleRestoreLocal)
	mux.HandleFunc("PUT /api/accounts/{id}", s.handleUpdateAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("GET /api/groups", s.handleListGroups)

	// 活动计划
	mux.HandleFunc("GET /api/plans", s.handleListPlans)
	mux.HandleFunc("POST /api/plans", s.handleSavePlan)
	mux.HandleFunc("PUT /api/plans/{id}", s.handleSavePlan)
	mux.HandleFunc("DELETE /api/plans/{id}", s.handleDeletePlan)
	mux.HandleFunc("POST /api/plans/{id}/run", s.handleRunPlan)
	mux.HandleFunc("GET /api/plans/running", s.handleRunningPlans)
	mux.HandleFunc("GET /api/plan-runs", s.handlePlanRuns)

	// 记录
	mux.HandleFunc("GET /api/claim-records", s.handleClaimRecords)
	mux.HandleFunc("GET /api/usage-records", s.handleUsageRecords)
	mux.HandleFunc("GET /api/stats", s.handleStats)

	// 设置
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)

	// 代理
	mux.HandleFunc("GET /api/proxies", s.handleListProxies)
	mux.HandleFunc("POST /api/proxies", s.handleSaveProxy)
	mux.HandleFunc("PUT /api/proxies/{id}", s.handleSaveProxy)
	mux.HandleFunc("DELETE /api/proxies/{id}", s.handleDeleteProxy)
	mux.HandleFunc("POST /api/proxies/{id}/test", s.handleTestProxy)
	mux.HandleFunc("POST /api/proxies/test-url", s.handleTestProxyURL)
	mux.HandleFunc("GET /api/proxies/system", s.handleSystemProxy)
	mux.HandleFunc("GET /api/proxies/probe-ports", s.handleProbePorts)

	// 验证码
	mux.HandleFunc("GET /api/captcha/status", s.handleCaptchaStatus)
	mux.HandleFunc("POST /api/captcha/invalidate", s.handleCaptchaInvalidate)
	mux.HandleFunc("POST /api/captcha/solve", s.handleCaptchaSolve)

	// 指纹
	mux.HandleFunc("GET /api/fingerprints", s.handleFingerprints)

	// 模型
	mux.HandleFunc("GET /api/models", s.handleModelList)
	mux.HandleFunc("POST /api/models/sync", s.handleModelSync)
	mux.HandleFunc("GET /api/models/catalog", s.handleModelCatalog)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// ---- 仪表盘 ----

func (s *APIServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	accounts, _ := s.db.ListAccounts("")
	statusCount := map[string]int{}
	groupCount := map[string]int{}
	totalRemaining := 0.0
	for _, a := range accounts {
		st := EffectiveStatus(a)
		statusCount[st]++
		g := a.AccountGroup
		if g == "" {
			g = "未分组"
		}
		groupCount[g]++
		totalRemaining += a.Remaining
	}
	stats, _ := s.db.UsageStats(7)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_total":      len(accounts),
		"account_selectable": s.pool.SelectableCount(""),
		"status_count":       statusCount,
		"group_count":        groupCount,
		"total_remaining":    totalRemaining,
		"usage_7d":           stats,
		"app_version":        s.zapi.appVersion,
		"captcha":            s.captcha.Status(),
	})
}

// ---- 账号 ----

// accountPublicView 脱敏账号视图
func accountPublicView(a *Account) map[string]interface{} {
	mask := func(tok string) string {
		if tok == "" {
			return ""
		}
		// 任何长度都脱敏，短 token 也不全量返回
		if len(tok) <= 8 {
			return "****"
		}
		return tok[:4] + "…" + tok[len(tok)-4:]
	}
	var quota *QuotaOverview
	if a.QuotaJSON != "" {
		var ov QuotaOverview
		if json.Unmarshal([]byte(a.QuotaJSON), &ov) == nil {
			quota = &ov
		}
	}
	return map[string]interface{}{
		"id": a.ID, "user_id": a.UserID, "email": a.Email, "display_name": a.DisplayName,
		"provider": a.Provider, "auth_type": a.AuthType,
		"jwt_masked": mask(a.ZCodeJWT), "api_key_masked": mask(a.APIKey),
		"has_jwt": a.ZCodeJWT != "", "has_api_key": a.APIKey != "",
		"has_access_token": a.AccessToken != "", "has_creds_snapshot": a.CredsRaw != "",
		"device_mid": a.DeviceMid,
		"status": EffectiveStatus(a), "raw_status": a.Status, "enabled": a.Enabled,
		"group": a.AccountGroup, "remark": a.Remark,
		"plan_tier": a.PlanTier, "plan_expire": a.PlanExpire,
		"total_units": a.TotalUnits, "used_units": a.UsedUnits, "remaining": a.Remaining,
		"quota":          quota,
		"use_count":      a.UseCount,
		"fail_count":     a.FailCount,
		"last_used_at":   a.LastUsedAt,
		"last_checked_at": a.LastCheckedAt,
		"cooling_until":  a.CoolingUntil,
		"last_error":     a.LastError,
		"last_claim_at":  a.LastClaimAt, "last_claim_plan": a.LastClaimPlan, "last_claim_msg": a.LastClaimMsg,
		"created_at": a.CreatedAt, "updated_at": a.UpdatedAt,
	}
}

func (s *APIServer) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	group := r.URL.Query().Get("group")
	accounts, err := s.db.ListAccounts(group)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, accountPublicView(a))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"accounts": out, "total": len(out)})
}

func (s *APIServer) handleImportLocal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Group string `json:"group"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	a, err := s.acctMgr.ImportFromLocalClient(body.Group)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "account": accountPublicView(a)})
}

func (s *APIServer) handleImportPaste(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		Secret   string `json:"secret"`
		Group    string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	a, err := s.acctMgr.ImportPasted(body.Provider, body.Name, body.Secret, body.Group)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "account": accountPublicView(a)})
}

// handleExportBundle 导出加密账号包
func (s *APIServer) handleExportBundle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string  `json:"password"`
		IDs      []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	bundle, err := s.acctMgr.ExportBundle(body.Password, body.IDs)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"bundle": bundle})
}

// handleImportBundle 导入加密账号包
func (s *APIServer) handleImportBundle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
		Bundle   string `json:"bundle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	count, err := s.acctMgr.ImportBundle(body.Password, body.Bundle)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "imported": count})
}

func (s *APIServer) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Manual bool   `json:"manual"`
		Group  string `json:"group"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	flow, authURL := s.oauth.StartLogin(body.Manual, body.Group)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"state":         flow.State,
		"authorize_url": authURL,
		"manual":        body.Manual,
	})
}

func (s *APIServer) handleOAuthManual(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
		Input string `json:"input"` // 回跳 URL 或 code
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.oauth.SubmitManual(body.State, body.Input); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "已提交，正在后台兑换"})
}

func (s *APIServer) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	flow := s.oauth.FlowStatus(state)
	if flow == nil {
		writeAPIError(w, http.StatusNotFound, "流程不存在或已过期")
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

func (s *APIServer) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.zapi.RefreshAccountQuota(a); err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	a2, _ := s.db.GetAccount(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "account": accountPublicView(a2)})
}

func (s *APIServer) handleAccountClaim(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	result := s.zapi.ClaimForAccount(a)
	writeJSON(w, http.StatusOK, result)
}

func (s *APIServer) handleAccountDetect(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	plans, err := s.zapi.PreviewPlans(a)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"plans": plans, "total": len(plans)})
}

func (s *APIServer) handleAccountActivate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	result := s.zapi.ActivateForAccount(a)
	writeJSON(w, http.StatusOK, result)
}

// handleAccountReset 执行 Coding Plan 配额重置（审计发现接口）
func (s *APIServer) handleAccountReset(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	result := s.zapi.ResetForAccount(a)
	writeJSON(w, http.StatusOK, result)
}

// handleResetStatus 查询重置机会（five_hour / week）
func (s *APIServer) handleResetStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	st, httpStatus, bizMsg, err := s.zapi.FetchResetStatus(a)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "message": firstNonEmpty(bizMsg, err.Error()), "http_status": httpStatus})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "status": st})
}

// handleModelSync 同步官方模型目录
func (s *APIServer) handleModelSync(w http.ResponseWriter, r *http.Request) {
	models, err := s.zapi.SyncModelCatalog()
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "models": models, "total": len(models)})
}

// handleModelCatalog 读取缓存的模型目录
func (s *APIServer) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": s.zapi.GetModelCatalog()})
}

func (s *APIServer) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Group   *string `json:"group"`
		Remark  *string `json:"remark"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	group, remark, enabled := a.AccountGroup, a.Remark, a.Enabled
	if body.Group != nil {
		group = *body.Group
	}
	if body.Remark != nil {
		remark = *body.Remark
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if err := s.db.UpdateAccountFields(id, group, remark, enabled); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *APIServer) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.db.DeleteAccount(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *APIServer) handleSwitchBack(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		KillClient bool `json:"kill_client"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if err := s.acctMgr.SwitchBackToLocal(id, body.KillClient); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "已写回本地 ZCode 客户端，重启客户端生效",
	})
}

func (s *APIServer) handleRestoreLocal(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.acctMgr.RestoreLocalFromSnapshot(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "已从快照还原本地客户端"})
}

func (s *APIServer) handleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.db.ListGroups()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if groups == nil {
		groups = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"groups": groups})
}

// ---- 活动计划 ----

func (s *APIServer) handleListPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.db.ListClaimPlans()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(plans))
	now := time.Now()
	for _, p := range plans {
		item := map[string]interface{}{
			"id": p.ID, "plan_name": p.PlanName, "cron_expr": p.CronExpr,
			"is_active": p.IsActive, "target_type": p.TargetType, "account_id": p.AccountID,
			"account_group": p.AccountGroup, "task_type": p.TaskType, "auto_pick": p.AutoPick,
			"delay_seconds": p.DelaySeconds, "last_run_at": p.LastRunAt,
			"last_run_status": p.LastRunStatus, "last_run_msg": p.LastRunMsg,
			"created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
		}
		if next := NextRunTime(p.CronExpr, now); !next.IsZero() {
			item["next_run_at"] = next.Format("2006-01-02 15:04")
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"plans": out})
}

func (s *APIServer) handleSavePlan(w http.ResponseWriter, r *http.Request) {
	var p ClaimPlan
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if idStr := r.PathValue("id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid id")
			return
		}
		p.ID = id
	}
	if err := ValidateCronExpr(p.CronExpr); err != nil {
		writeAPIError(w, http.StatusBadRequest, "cron 表达式无效: "+err.Error())
		return
	}
	if p.TaskType == "" {
		p.TaskType = "claim"
	}
	if p.TargetType == "" {
		p.TargetType = "all_accounts"
	}
	id, err := s.db.SaveClaimPlan(&p)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "id": id})
}

func (s *APIServer) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.db.DeleteClaimPlan(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *APIServer) handleRunPlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.scheduler.RunPlanNow(id); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "计划已开始执行"})
}

func (s *APIServer) handleRunningPlans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"running": s.scheduler.GetRunning()})
}

func (s *APIServer) handlePlanRuns(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	records, err := s.db.ListPlanRunRecords(limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"records": records})
}

// ---- 记录与统计 ----

func (s *APIServer) handleClaimRecords(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	accountID := int64(queryInt(r, "account_id", 0))
	records, err := s.db.ListClaimRecords(limit, accountID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"records": records})
}

func (s *APIServer) handleUsageRecords(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	records, err := s.db.ListUsageRecords(limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"records": records})
}

func (s *APIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 7)
	stats, err := s.db.UsageStats(days)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---- 设置 ----

// settingsWhitelist 允许前端修改的设置键
var settingsWhitelist = map[string]bool{
	"selection_strategy": true, "quota_refresh_interval": true,
	"upstream_proxy": true, "fingerprint": true, "custom_ja3": true,
	"captcha_mode": true, "gateway_models": true,
}

func (s *APIServer) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := s.db.AllSettings()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 敏感项脱敏
	delete(all, "password_hash")
	if k, ok := all["api_key"]; ok && k != "" {
		all["has_api_key"] = "1"
		delete(all, "api_key")
	}
	all["app_version"] = s.zapi.appVersion
	all["listen_addr"] = s.cfg.GetListenAddr()
	writeJSON(w, http.StatusOK, all)
}

func (s *APIServer) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated := []string{}
	for k, v := range body {
		if !settingsWhitelist[k] {
			continue
		}
		if k == "selection_strategy" {
			switch v {
			case StrategyRandom, StrategyRoundRobin, StrategyBestQuota:
			default:
				writeAPIError(w, http.StatusBadRequest, "无效策略: "+v)
				return
			}
		}
		if k == "quota_refresh_interval" {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 86400 {
				writeAPIError(w, http.StatusBadRequest, "无效刷新间隔")
				return
			}
		}
		// 指纹设置服务端校验：非法值会导致所有上游连接失败
		if k == "fingerprint" && !isValidFingerprint(v) {
			writeAPIError(w, http.StatusBadRequest, "无效指纹预设: "+v)
			return
		}
		if k == "custom_ja3" && strings.TrimSpace(v) != "" {
			if _, err := ja3ToClientHelloSpec(v); err != nil {
				writeAPIError(w, http.StatusBadRequest, "无效 JA3: "+err.Error())
				return
			}
		}
		if err := s.db.SetSetting(k, v); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		updated = append(updated, k)
	}
	// 指纹/代理变更：失效并关闭缓存的上游客户端连接池
	for _, k := range updated {
		if k == "fingerprint" || k == "custom_ja3" || k == "upstream_proxy" {
			CloseIdleClients()
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "updated": updated})
}

// ---- 代理 ----

func (s *APIServer) handleListProxies(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.db.ListProxyNodes()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		item := map[string]interface{}{
			"id": n.ID, "name": n.Name, "type": n.Type, "host": n.Host, "port": n.Port,
			"username": n.Username, "has_password": n.Password != "",
			"is_default": n.IsDefault, "group_name": n.GroupName, "enabled": n.Enabled,
			"check_status": n.CheckStatus, "check_latency": n.CheckLatency,
			"check_ip": n.CheckIP, "check_msg": n.CheckMsg, "check_at": n.CheckAt,
			"created_at": n.CreatedAt, "updated_at": n.UpdatedAt,
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"proxies": out})
}

func (s *APIServer) handleSaveProxy(w http.ResponseWriter, r *http.Request) {
	var n ProxyNode
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if idStr := r.PathValue("id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid id")
			return
		}
		n.ID = id
	}
	if n.Host == "" || n.Port <= 0 {
		writeAPIError(w, http.StatusBadRequest, "host/port 必填")
		return
	}
	// 编辑时密码留空 = 保持原密码
	if n.ID > 0 && n.Password == "" {
		if old, err := s.db.ListProxyNodes(); err == nil {
			for _, o := range old {
				if o.ID == n.ID {
					n.Password = o.Password
					break
				}
			}
		}
	}
	id, err := s.db.SaveProxyNode(&n)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "id": id})
}

func (s *APIServer) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.db.DeleteProxyNode(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *APIServer) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	nodes, _ := s.db.ListProxyNodes()
	var target *ProxyNode
	for _, n := range nodes {
		if n.ID == id {
			target = n
			break
		}
	}
	if target == nil {
		writeAPIError(w, http.StatusNotFound, "代理节点不存在")
		return
	}
	proxyURL := ProxyURLForNode(target)
	ip, elapsed, err := TestProxyExitIP(proxyURL)
	status, msg := "ok", ""
	if err != nil {
		status, msg = "fail", err.Error()
	}
	s.db.UpdateProxyNodeCheck(id, status, int(elapsed.Milliseconds()), ip, msg)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": err == nil, "exit_ip": ip, "elapsed_ms": elapsed.Milliseconds(),
		"message": msg, "proxy": MaskProxyURL(proxyURL),
	})
}

func (s *APIServer) handleTestProxyURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	ip, elapsed, err := TestProxyExitIP(strings.TrimSpace(body.URL))
	resp := map[string]interface{}{
		"ok": err == nil, "exit_ip": ip, "elapsed_ms": elapsed.Milliseconds(),
	}
	if err != nil {
		resp["message"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *APIServer) handleSystemProxy(w http.ResponseWriter, r *http.Request) {
	enabled, proxyURL := DetectSystemProxy()
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": enabled, "url": proxyURL})
}

func (s *APIServer) handleProbePorts(w http.ResponseWriter, r *http.Request) {
	ports := ProbeLocalProxyPorts()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ports": ports})
}

// ---- 验证码 ----

func (s *APIServer) handleCaptchaStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.captcha.Status())
}

func (s *APIServer) handleCaptchaInvalidate(w http.ResponseWriter, r *http.Request) {
	s.captcha.Invalidate()
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// handleCaptchaSolve 手动触发一次求解（调试用；有头模式下会弹浏览器窗口）
func (s *APIServer) handleCaptchaSolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID int64 `json:"account_id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	var a *Account
	if body.AccountID > 0 {
		a, _ = s.db.GetAccount(body.AccountID)
	}
	param, region, err := s.captcha.GetVerifyParam(a)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": param != "", "param_len": len(param), "region": region,
	})
}

// ---- 指纹 ----

func (s *APIServer) handleFingerprints(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]string, 0, len(tlsFingerprintPresets))
	for _, p := range tlsFingerprintPresets {
		out = append(out, map[string]string{"id": p.ID, "label": p.Label, "group": p.Group})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"fingerprints": out})
}

// ---- 模型 ----

func (s *APIServer) handleModelList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": s.cfg.GetModels()})
}

// ---- 工具 ----

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	// 夹取范围，防止 LIMIT -1 / 超大值拉全表
	if n < 1 {
		return 1
	}
	if n > 500 {
		return 500
	}
	return n
}

var _ = log.Printf
var _ = fmt.Sprintf
