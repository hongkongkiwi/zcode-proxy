package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- 账号池：状态机 + 选择策略 + 额度刷新循环 ----
//
// 状态（与 zcode2api models.py 一致，另加 inactive=套餐未激活）：
//   active    正常可轮询
//   exhausted 额度用完（402 / remaining=0），不可选，等额度刷新恢复
//   cooling   限流冷却（429），cooling_until 到期后自动可选
//   invalid   凭证失效（401/403），需人工处理
//   disabled  手动禁用
//   inactive  Coding Plan / Start Plan 未激活（可尝试激活流程恢复）

const (
	StatusActive    = "active"
	StatusExhausted = "exhausted"
	StatusCooling   = "cooling"
	StatusInvalid   = "invalid"
	StatusDisabled  = "disabled"
	StatusInactive  = "inactive"
)

// 选择策略
const (
	StrategyRandom     = "random"
	StrategyRoundRobin = "round_robin"
	StrategyBestQuota  = "best_quota"
	StrategyPriority   = "priority"
)

// AccountPool 账号池
type AccountPool struct {
	db         *DB
	cfg        *FileConfig
	appVersion string

	mu       sync.Mutex
	rotation map[string]int // "group|provider" -> round-robin 游标

	invalidRetry map[int64]time.Time // invalid 账号上次重试时间（mu 保护，内存退避）

	sticky       map[string]stickyEntry // 会话粘滞：sessionKey -> 账号（mu 保护，TTL 淘汰）
	stickyPruned time.Time              // 上次粘滞表清理时间

	refreshFn func(a *Account) error // 由 ZCodeAPI 注入的额度刷新函数
	stopCh    chan struct{}
	stopOnce  sync.Once
}

// stickyEntry 会话粘滞表项
type stickyEntry struct {
	accountID int64
	seenAt    int64
}

const (
	stickyTTL  = time.Hour
	stickyMax  = 10000
	pruneEvery = 10 * time.Minute
)

// NewAccountPool 创建账号池
func NewAccountPool(db *DB, cfg *FileConfig, appVersion string) *AccountPool {
	return &AccountPool{
		db:           db,
		cfg:          cfg,
		appVersion:   appVersion,
		rotation:     make(map[string]int),
		invalidRetry: make(map[int64]time.Time),
		sticky:       make(map[string]stickyEntry),
		stopCh:       make(chan struct{}),
	}
}

// SetQuotaFetcher 注入额度刷新实现（解耦 ZCodeAPI 循环依赖）
func (p *AccountPool) SetQuotaFetcher(fn func(a *Account) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshFn = fn
}

// Start 启动后台额度刷新循环
func (p *AccountPool) Start() {
	go p.refreshLoop()
}

// Stop 停止后台循环
func (p *AccountPool) Stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
}

func (p *AccountPool) refreshLoop() {
	// 启动后先等 5 秒（让 HTTP 服务先起来）
	select {
	case <-p.stopCh:
		return
	case <-time.After(5 * time.Second):
	}
	for {
		interval := p.refreshInterval()
		if interval > 0 {
			p.refreshAll()
		} else {
			// 显式 0 = 关闭刷新：固定 30s 后重读配置（设置支持 UI 热更新，避免空转打满 GetSetting）
			interval = 30
		}
		select {
		case <-p.stopCh:
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}

func (p *AccountPool) refreshInterval() int {
	v, err := p.db.GetSetting("quota_refresh_interval")
	if err != nil || v == "" {
		return 60 // 未配置：默认 60s
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 60
	}
	if n < 0 {
		return 60
	}
	return n // 显式 0 = 关闭后台刷新
}

// invalidBackoff invalid 账号重试退避间隔：max(refreshInterval*10, 5 分钟)
func (p *AccountPool) invalidBackoff() time.Duration {
	d := time.Duration(p.refreshInterval()) * 10 * time.Second
	if d < 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// refreshAll 刷新所有启用账号的额度（并发 4，账号间 1-3s 随机延迟防风控）
func (p *AccountPool) refreshAll() {
	p.mu.Lock()
	fn := p.refreshFn
	p.mu.Unlock()
	if fn == nil {
		return
	}
	accounts, err := p.db.ListAccounts("")
	if err != nil {
		log.Printf("[pool] list accounts: %v", err)
		return
	}
	backoff := p.invalidBackoff()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, a := range accounts {
		if !a.Enabled || a.Status == StatusDisabled {
			continue
		}
		if a.ZCodeJWT == "" && a.APIKey == "" {
			continue
		}
		// 冷却中的账号跳过刷新（到期后自然恢复）
		if a.Status == StatusCooling && a.CoolingUntil > time.Now().Unix() {
			continue
		}
		// invalid 账号按退避参与刷新（额度刷新是唯一自动恢复路径）
		if a.Status == StatusInvalid {
			p.mu.Lock()
			last := p.invalidRetry[a.ID]
			p.mu.Unlock()
			if time.Since(last) < backoff {
				continue
			}
			p.mu.Lock()
			p.invalidRetry[a.ID] = time.Now()
			p.mu.Unlock()
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				// 后台长驻 worker：单账号刷新 panic 不得带走整个进程
				if r := recover(); r != nil {
					log.Printf("[pool] refresh quota %s panicked: %v", acc.Email, r)
				}
			}()
			// 每账号随机延迟 0-2s，模拟人工行为
			time.Sleep(time.Duration(rand.Intn(2000)) * time.Millisecond)
			if err := fn(acc); err != nil {
				log.Printf("[pool] refresh quota %s: %v", acc.Email, err)
			}
		}(a)
	}
	wg.Wait()
}

// RefreshOne 手动刷新单个账号额度（API 触发）
func (p *AccountPool) RefreshOne(a *Account) error {
	p.mu.Lock()
	fn := p.refreshFn
	p.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(a)
}

// ---- 可选性判定（models.py is_selectable 移植）----

func accountSelectable(a *Account, now int64) bool {
	if !a.Enabled || a.Status == StatusDisabled || a.Status == StatusInvalid ||
		a.Status == StatusExhausted || a.Status == StatusInactive {
		return false
	}
	if a.Status == StatusCooling {
		// cooling_until<=0（历史/手工数据）视为已到期，避免永久不可选
		return a.CoolingUntil <= 0 || now >= a.CoolingUntil
	}
	return true
}

// EffectiveStatus 考虑冷却到期的实时状态（cooling_until<=0 视为已到期，
// 与 accountSelectable 同一规则，避免"仪表盘显示冷却、转发却照选"的分裂）
func EffectiveStatus(a *Account) string {
	if a.Status == StatusCooling && (a.CoolingUntil <= 0 || time.Now().Unix() >= a.CoolingUntil) {
		return StatusActive
	}
	return a.Status
}

// matchAccountGroup 账号组匹配：account_group 允许逗号分隔多组（ListGroups 同约定），
// 精确匹配某一组名
func matchAccountGroup(a *Account, group string) bool {
	for _, part := range strings.Split(a.AccountGroup, ",") {
		if strings.TrimSpace(part) == group {
			return true
		}
	}
	return false
}

// ---- 账号选择 ----

// Select 按策略选择账号。group 为空 = 不限组；skip 为已尝试过的账号 ID。
func (p *AccountPool) Select(provider, group string, skip map[int64]bool) *Account {
	accounts, err := p.db.ListAccounts("")
	if err != nil {
		log.Printf("[pool] select list: %v", err)
		return nil
	}
	now := time.Now().Unix()
	var pool []*Account
	for _, a := range accounts {
		if a.Provider != provider {
			continue
		}
		if group != "" && !matchAccountGroup(a, group) {
			continue
		}
		if skip[a.ID] {
			continue
		}
		if !accountSelectable(a, now) {
			continue
		}
		// 必须有可用凭证
		if a.ZCodeJWT == "" && a.APIKey == "" {
			continue
		}
		pool = append(pool, a)
	}
	if len(pool) == 0 {
		return nil
	}

	strategy, _ := p.db.GetSetting("selection_strategy")
	if strategy == StrategyPriority {
		// 级联：只保留最高优先级（数值最小）层，层内 round_robin 公平轮转。
		// 被状态机淘汰的账号本就不在候选里——promo 层耗尽时自然让位给下一层
		minPri := int64(1 << 62)
		for _, a := range pool {
			pri := accountPriority(a)
			if pri < minPri {
				minPri = pri
			}
		}
		var tier []*Account
		for _, a := range pool {
			if accountPriority(a) == minPri {
				tier = append(tier, a)
			}
		}
		pool = tier
	}
	switch strategy {
	case StrategyRandom:
		return pool[rand.Intn(len(pool))]
	case StrategyBestQuota:
		// 剩余额度最大者优先；额度未知(0)排后
		sort.SliceStable(pool, func(i, j int) bool {
			return pool[i].Remaining > pool[j].Remaining
		})
		return pool[0]
	default: // round_robin
		p.mu.Lock()
		defer p.mu.Unlock()
		key := group + "|" + provider
		idx := p.rotation[key] % len(pool)
		a := pool[idx]
		p.rotation[key] = (idx + 1) % len(pool)
		return a
	}
}

// ---- 会话粘滞（F2）----
// 同一会话尽量固定到同一账号，保住上游 prompt 缓存（缓存命中比 fresh 便宜数倍）。
// 粘滞账号进入不可选状态时 selectable 校验自动让位，恢复后回到粘滞账号。

func (p *AccountPool) stickyEnabled() bool {
	v, _ := p.db.GetSetting("sticky_sessions")
	return v != "0" && v != "false"
}

// SelectSticky 粘滞优先选择：sessionKey 命中且账号仍可选 → 复用；
// 否则按策略选择并记录。skip（已尝试失败）的账号不粘滞。
func (p *AccountPool) SelectSticky(provider, group, sessionKey string, skip map[int64]bool) *Account {
	if sessionKey != "" && p.stickyEnabled() {
		now := time.Now().Unix()
		p.mu.Lock()
		e, ok := p.sticky[sessionKey]
		p.mu.Unlock()
		if ok && now-e.seenAt < int64(stickyTTL.Seconds()) && !skip[e.accountID] {
			if a := p.selectableByID(e.accountID, provider, group, skip); a != nil {
				p.rememberSticky(sessionKey, a.ID)
				return a
			}
		}
	}
	a := p.Select(provider, group, skip)
	if a != nil && sessionKey != "" && p.stickyEnabled() {
		p.rememberSticky(sessionKey, a.ID)
	}
	return a
}

func (p *AccountPool) selectableByID(id int64, provider, group string, skip map[int64]bool) *Account {
	a, err := p.db.GetAccount(id)
	if err != nil || a == nil {
		return nil
	}
	if skip[id] || a.Provider != provider || !a.Enabled {
		return nil
	}
	if group != "" && !matchAccountGroup(a, group) {
		return nil
	}
	if !accountSelectable(a, time.Now().Unix()) {
		return nil
	}
	if a.ZCodeJWT == "" && a.APIKey == "" {
		return nil
	}
	return a
}

func (p *AccountPool) rememberSticky(sessionKey string, accountID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().Unix()
	if len(p.sticky) >= stickyMax && time.Since(p.stickyPruned) < pruneEvery {
		// 表满且刚清理过：覆盖式写入，接受随机挤掉
		p.sticky[sessionKey] = stickyEntry{accountID: accountID, seenAt: now}
		return
	}
	p.stickyPruned = time.Now()
	for k, e := range p.sticky {
		if now-e.seenAt >= int64(stickyTTL.Seconds()) {
			delete(p.sticky, k)
		}
	}
	if len(p.sticky) >= stickyMax {
		p.sticky = make(map[string]stickyEntry)
	}
	p.sticky[sessionKey] = stickyEntry{accountID: accountID, seenAt: now}
}

// ---- 耗尽重置提示（F3）----

// ExhaustedResetInfo 返回 provider/group 下耗尽账号的最早重置时间（unix 秒）
// 与账号名；读各自 quota_json 的 next_reset 字段（monitor 通道提供）。
func (p *AccountPool) ExhaustedResetInfo(provider, group string) (int64, string) {
	accounts, err := p.db.ListAccounts("")
	if err != nil {
		return 0, ""
	}
	var until int64
	email := ""
	for _, a := range accounts {
		if provider != "" && a.Provider != provider {
			continue
		}
		if group != "" && !matchAccountGroup(a, group) {
			continue
		}
		if a.Status != StatusExhausted || a.QuotaJSON == "" {
			continue
		}
		if r := nextResetFromQuota(a.QuotaJSON); r > 0 && (until == 0 || r < until) {
			until = r
			email = a.DisplayNameOrEmail()
		}
	}
	return until, email
}

func nextResetFromQuota(quotaJSON string) int64 {
	var ov struct {
		NextReset int64 `json:"next_reset"`
	}
	if json.Unmarshal([]byte(quotaJSON), &ov) != nil {
		return 0
	}
	return ov.NextReset
}

// ---- 状态迁移 ----

// MarkExhausted 额度用完
func (p *AccountPool) MarkExhausted(a *Account, reason string) {
	a.setRuntime(StatusExhausted, reason, 0)
	p.db.SetAccountStatus(a.ID, StatusExhausted, reason, 0)
	log.Printf("[pool] account %s -> exhausted: %s", a.Email, reason)
}

// MarkCooling 限流冷却（默认 60s，429 多为瞬时 RPM 峰值）
func (p *AccountPool) MarkCooling(a *Account, reason string, seconds int) {
	if seconds <= 0 {
		seconds = 60
	}
	until := time.Now().Unix() + int64(seconds)
	a.setRuntime(StatusCooling, reason, until)
	p.db.SetAccountStatus(a.ID, StatusCooling, reason, until)
	log.Printf("[pool] account %s -> cooling %ds: %s", a.Email, seconds, reason)
}

// MarkInvalid 凭证失效
func (p *AccountPool) MarkInvalid(a *Account, reason string) {
	a.setRuntime(StatusInvalid, reason, 0)
	p.db.SetAccountStatus(a.ID, StatusInvalid, reason, 0)
	log.Printf("[pool] account %s -> invalid: %s", a.Email, reason)
}

// MarkInactive 套餐未激活
func (p *AccountPool) MarkInactive(a *Account, reason string) {
	a.setRuntime(StatusInactive, reason, 0)
	p.db.SetAccountStatus(a.ID, StatusInactive, reason, 0)
	log.Printf("[pool] account %s -> inactive: %s", a.Email, reason)
}

// MarkUsed 成功使用一次（仅 cooling 恢复 active；exhausted 只能由额度刷新恢复，避免并发复活抖动）
func (p *AccountPool) MarkUsed(a *Account) {
	a.bumpUse()
	p.db.TouchAccountUse(a.ID)
}

// MarkFailed 失败计数
func (p *AccountPool) MarkFailed(a *Account, reason string) {
	a.bumpFail(reason)
	p.db.BumpAccountFail(a.ID, reason)
}

// CoolingInfo 返回该 provider/group 下最近一个冷却中账号的恢复时间与原因（用于 503 提示）
func (p *AccountPool) CoolingInfo(provider, group string) (int64, string) {
	accounts, err := p.db.ListAccounts("")
	if err != nil {
		return 0, ""
	}
	now := time.Now().Unix()
	var until int64
	reason := ""
	for _, a := range accounts {
		if provider != "" && a.Provider != provider {
			continue
		}
		if group != "" && !matchAccountGroup(a, group) {
			continue
		}
		if a.Status == StatusCooling && a.CoolingUntil > now {
			if until == 0 || a.CoolingUntil < until {
				until = a.CoolingUntil
				reason = a.LastError
			}
		}
	}
	return until, reason
}

// SelectableCount 返回当前可选账号数（仪表盘）
func (p *AccountPool) SelectableCount(provider string) int {
	accounts, err := p.db.ListAccounts("")
	if err != nil {
		return 0
	}
	now := time.Now().Unix()
	n := 0
	for _, a := range accounts {
		if provider != "" && a.Provider != provider {
			continue
		}
		if accountSelectable(a, now) && (a.ZCodeJWT != "" || a.APIKey != "") {
			n++
		}
	}
	return n
}
