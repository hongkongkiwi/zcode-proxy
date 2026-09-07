package main

import (
	"log"
	"math/rand"
	"sort"
	"strconv"
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
)

// AccountPool 账号池
type AccountPool struct {
	db         *DB
	cfg        *FileConfig
	appVersion string

	mu       sync.Mutex
	rotation map[string]int // "group|provider" -> round-robin 游标

	refreshFn func(a *Account) error // 由 ZCodeAPI 注入的额度刷新函数
	stopCh    chan struct{}
	stopOnce  sync.Once
}

// NewAccountPool 创建账号池
func NewAccountPool(db *DB, cfg *FileConfig, appVersion string) *AccountPool {
	return &AccountPool{
		db:         db,
		cfg:        cfg,
		appVersion: appVersion,
		rotation:   make(map[string]int),
		stopCh:     make(chan struct{}),
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
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, a := range accounts {
		if !a.Enabled || a.Status == StatusDisabled || a.Status == StatusInvalid {
			continue
		}
		if a.ZCodeJWT == "" && a.APIKey == "" {
			continue
		}
		// 冷却中的账号跳过刷新（到期后自然恢复）
		if a.Status == StatusCooling && a.CoolingUntil > time.Now().Unix() {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *Account) {
			defer wg.Done()
			defer func() { <-sem }()
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

// EffectiveStatus 考虑冷却到期的实时状态
func EffectiveStatus(a *Account) string {
	if a.Status == StatusCooling && a.CoolingUntil > 0 && time.Now().Unix() >= a.CoolingUntil {
		return StatusActive
	}
	return a.Status
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
		if group != "" && a.AccountGroup != group {
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

// ---- 状态迁移 ----

// MarkExhausted 额度用完
func (p *AccountPool) MarkExhausted(a *Account, reason string) {
	a.Status = StatusExhausted
	a.LastError = reason
	p.db.SetAccountStatus(a.ID, StatusExhausted, reason, 0)
	log.Printf("[pool] account %s -> exhausted: %s", a.Email, reason)
}

// MarkCooling 限流冷却（默认 60s，429 多为瞬时 RPM 峰值）
func (p *AccountPool) MarkCooling(a *Account, reason string, seconds int) {
	if seconds <= 0 {
		seconds = 60
	}
	until := time.Now().Unix() + int64(seconds)
	a.Status = StatusCooling
	a.CoolingUntil = until
	a.LastError = reason
	p.db.SetAccountStatus(a.ID, StatusCooling, reason, until)
	log.Printf("[pool] account %s -> cooling %ds: %s", a.Email, seconds, reason)
}

// MarkInvalid 凭证失效
func (p *AccountPool) MarkInvalid(a *Account, reason string) {
	a.Status = StatusInvalid
	a.LastError = reason
	p.db.SetAccountStatus(a.ID, StatusInvalid, reason, 0)
	log.Printf("[pool] account %s -> invalid: %s", a.Email, reason)
}

// MarkInactive 套餐未激活
func (p *AccountPool) MarkInactive(a *Account, reason string) {
	a.Status = StatusInactive
	a.LastError = reason
	p.db.SetAccountStatus(a.ID, StatusInactive, reason, 0)
	log.Printf("[pool] account %s -> inactive: %s", a.Email, reason)
}

// MarkUsed 成功使用一次（cooling/exhausted 恢复 active）
func (p *AccountPool) MarkUsed(a *Account) {
	a.UseCount++
	a.LastUsedAt = time.Now().Unix()
	if a.Status == StatusCooling || a.Status == StatusExhausted {
		a.Status = StatusActive
		a.CoolingUntil = 0
	}
	p.db.TouchAccountUse(a.ID)
}

// MarkFailed 失败计数
func (p *AccountPool) MarkFailed(a *Account, reason string) {
	a.FailCount++
	a.LastError = reason
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
		if group != "" && a.AccountGroup != group {
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
