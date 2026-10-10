package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ---- 网关 Key 运行时（R1）----
// 认证（auth.Middleware）解析请求 Key → 命中命名 Key 时做 RPM/启停检查并注入
// context；转发层（relay）用 context 里的 Key 做模型白名单/配额拦截，
// usage 落库时回写消耗。根 Key（旧 api_key）不注入 context，不受任何限制。

type gwKeyCtxType int

const gwKeyCtxKey gwKeyCtxType = 1

func contextWithGatewayKey(ctx context.Context, k *GatewayKey) context.Context {
	return context.WithValue(ctx, gwKeyCtxKey, k)
}

func gatewayKeyFromCtx(ctx context.Context) *GatewayKey {
	k, _ := ctx.Value(gwKeyCtxKey).(*GatewayKey)
	return k
}

// HashGatewayKey sha256 hex 摘要（明文不落库、不参与比对）
func HashGatewayKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// gatewayKeyQuotaCharge 网关 Key 配额计费口径：input+output 全价，
// cache_creation 全价（上游按 ≥1x 计费），cache_read 按 1/10 折算（上游缓存
// 命中约 0.1x 计费，整数除法近似）。usage_records.total_tokens 维持纯
// input+output 口径（报表语义不变），仅配额拦截走此口径——忽略缓存 token
// 会让重缓存客户端在 quota_total 之外烧掉大量真实计费 token（评审 F9）
func gatewayKeyQuotaCharge(u *StreamUsage) int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.OutputTokens + u.CacheCreationTokens + u.CacheReadTokens/10
}

// resolveGatewayKey 中间件解析：返回 nil,nil = 根 Key（或未配置命名 Key 场景直接放行）；
// 非 nil errResp = 认证/限流失败，已构造好响应。
// RPM 命中时 errResp.status = 429，调用方应带 Retry-After 回写。
func (am *AuthManager) resolveGatewayKey(key string) (*GatewayKey, *errorResponse) {
	if am.db == nil || key == "" {
		return nil, &errorResponse{status: http.StatusUnauthorized, msg: "invalid API key"}
	}
	// 根 Key 优先（常数时间比较，规避计时侧信道）
	if am.ValidateAPIKey(key) {
		return nil, nil
	}
	k, err := am.db.GetGatewayKeyByHash(HashGatewayKey(key))
	if err != nil {
		// 认证前置路径，未认证方可触达：库错误文本（引擎/状态细节）不外泄
		log.Printf("[auth] gateway key lookup: %v", err)
		return nil, &errorResponse{status: http.StatusInternalServerError, msg: "internal error"}
	}
	if k == nil {
		return nil, &errorResponse{status: http.StatusUnauthorized, msg: "invalid API key"}
	}
	if !k.Enabled {
		return nil, &errorResponse{status: http.StatusForbidden, msg: "gateway key is disabled: " + k.Name}
	}
	if k.RPMLimit > 0 && !am.gwRPM.allow(k.ID, k.RPMLimit) {
		return nil, &errorResponse{status: http.StatusTooManyRequests,
			msg: "gateway key rate limited (" + strconv.Itoa(k.RPMLimit) + " rpm): " + k.Name}
	}
	// 滚动窗口限额（5h/1d/7d）：与 RPM 同闸口（认证时点计数），跨窗口一次
	// 命中只记一笔；首个触限窗口决定 429 文案与 Retry-After（小时边界粒度）
	if k.Rate5h > 0 || k.Rate1d > 0 || k.Rate7d > 0 {
		if rej := am.gwWindows.admit(k.ID, []gwWindowSpec{
			{hours: 5, limit: k.Rate5h, label: "5h"},
			{hours: 24, limit: k.Rate1d, label: "24h"},
			{hours: gwWindowRingHours, limit: k.Rate7d, label: "7d"},
		}); rej != nil {
			return nil, &errorResponse{status: http.StatusTooManyRequests,
				msg:               "gateway key rate limited (" + rej.window + " rolling window): " + k.Name,
				retryAfterSeconds: int(rej.retryAfter.Seconds()) + 1}
		}
	}
	return k, nil
}

// gatewayKeyModelAllowed 仅白名单判定（无配额副作用）：nil Key（根 Key）放行。
// 请求校验前先行拦截用——避免注定 403 的请求先触发 URL 图片抓取等准备工作（轮 8）
func gatewayKeyModelAllowed(k *GatewayKey, model string) bool {
	return k == nil || k.modelAllowed(model)
}

// checkGatewayKeyRequest 转发前拦截：配额耗尽 / 模型不在白名单。
// model 须已去掉 provider 前缀并小写（与白名单同规范）。
func checkGatewayKeyRequest(k *GatewayKey, model string) *errorResponse {
	if k == nil {
		return nil
	}
	if k.QuotaTotal > 0 && k.QuotaUsed >= k.QuotaTotal {
		return &errorResponse{status: http.StatusTooManyRequests,
			msg: "gateway key token quota exhausted (" + strconv.FormatInt(k.QuotaUsed, 10) + "/" +
				strconv.FormatInt(k.QuotaTotal, 10) + "): " + k.Name}
	}
	if !k.modelAllowed(model) {
		return &errorResponse{status: http.StatusForbidden,
			msg: "model not allowed for this gateway key: " + model}
	}
	return nil
}

// gwRPM 命名 Key 的进程内滑动窗口限速（60s 窗口，与转发无关的独立锁）
type gwRPMTracker struct {
	mu    sync.Mutex
	hits  map[int64][]time.Time
	prune time.Time
}

const gwRPMWindow = time.Minute

func (t *gwRPMTracker) allow(id int64, limit int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hits == nil {
		t.hits = map[int64][]time.Time{}
	}
	now := time.Now()
	// 容量兜底：Key 数量无界增长不现实，但陈旧窗口必须淘汰
	if len(t.hits) >= 4096 && now.Sub(t.prune) > gwRPMWindow {
		for kid, ts := range t.hits {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > gwRPMWindow {
				delete(t.hits, kid)
			}
		}
		t.prune = now
	}
	cutoff := now.Add(-gwRPMWindow)
	live := t.hits[id][:0]
	for _, ts := range t.hits[id] {
		if ts.After(cutoff) {
			live = append(live, ts)
		}
	}
	if len(live) >= limit {
		t.hits[id] = live
		return false
	}
	t.hits[id] = append(live, now)
	return true
}

// ---- 滚动窗口限额（5h/1d/7d）----
// 按"自然小时"分桶近似滑动窗口：每把 Key 一个 168 桶环形计数器（桶 0 = 当前
// 自然小时，含未满部分），窗口命中数 = 最近 N 个桶之和。请求时间戳存满 7 天
// 在长窗口下不可行（RPM 的逐条 []time.Time 到 7d 量级是内存泄漏形状），分桶
// 把每 Key 状态压到常量 1.3KB。粒度取舍：窗口起点对齐小时边界——命中在桶内
// 的位置被抹平，实际放行只会比严格滑动窗口更早（最多提前约 1 小时），绝不会
// 更严；对"别把共享 Key 的 5h 套餐窗口烧穿"的用途足够（面板同文案披露）。
// 进程内状态，重启清零（与 RPM 同口径，不持久化）。

const gwWindowRingHours = 168 // 7 天

// gwWindowSpec 单个窗口：hours 桶之和对比 limit（0 = 该窗口不限）
type gwWindowSpec struct {
	hours int
	limit int
	label string
}

type gwWindowRejection struct {
	window     string
	retryAfter time.Duration
}

// gwWindowTracker 命名 Key 的滚动窗口计数器（与转发无关的独立锁）。
// 锁内操作 gwHourRing；Key 集合以 DB 行为界（管理员手工创建），删除时由
// forget 显式清理，admit 内另有陈旧环兜底
type gwWindowTracker struct {
	mu   sync.Mutex
	keys map[int64]*gwHourRing
}

// forget 删除 Key 时同步清窗口状态：create→use→delete 循环不在内存里累积
// 陈旧环（每环 1.3KB，泄漏是慢性的但方向明确不对）
func (t *gwWindowTracker) forget(id int64) {
	t.mu.Lock()
	delete(t.keys, id)
	t.mu.Unlock()
}

// gwHourRing 小时桶环形：counts[i] = anchor 往前第 i 个自然小时的命中数。
// 所有方法要求调用方持有 tracker 锁。
type gwHourRing struct {
	anchor int64 // counts[0] 对应的自然小时（unix 秒 / 3600）
	counts [gwWindowRingHours]int64
}

// advance 把环形推进到 nowHour；时钟回拨按重置处理（宁可多放不可卡死）
func (r *gwHourRing) advance(nowHour int64) {
	if r.anchor == nowHour {
		return
	}
	if r.anchor == 0 || nowHour < r.anchor || nowHour-r.anchor >= gwWindowRingHours {
		r.anchor = nowHour
		r.counts = [gwWindowRingHours]int64{}
		return
	}
	d := int(nowHour - r.anchor)
	// 右移 d 桶：counts[d:] = counts[:168-d]，老桶自然移出窗口尾（copy 即 memmove）
	copy(r.counts[d:], r.counts[:])
	for i := 0; i < d; i++ {
		r.counts[i] = 0
	}
	r.anchor = nowHour
}

func (r *gwHourRing) sum(hours int) int64 {
	var s int64
	for i := 0; i < hours && i < gwWindowRingHours; i++ {
		s += r.counts[i]
	}
	return s
}

// unlockAfter 估算触限窗口的最早解除时点。触限后 admit 不再记录命中（窗口
// 冻结），把窗口内最老侧逐桶移出，求最小 k 使剩余计数 < limit——推进 k 个
// 整点后（anchor+k）第 k 批最老桶恰好滑出，即解除边界。误差方向：若管理员
// 中途上调限额，实际恢复早于估算（保守可接受）；环内其他窗口有余量时新请求
// 会放行并记录，也会让实际恢复晚于估算（报短不报长——此后请求仍被拒，客户
// 端按 429 重退避）。下限 1s 防止边界抖动报 0
func (r *gwHourRing) unlockAfter(hours, limit int, nowHour int64, now time.Time) time.Duration {
	total := r.sum(hours)
	removed := int64(0)
	k := 1
	for ; k <= hours; k++ {
		if idx := hours - k; idx >= 0 && idx < gwWindowRingHours {
			removed += r.counts[idx]
		}
		if total-removed < int64(limit) {
			break
		}
	}
	if k > hours {
		k = hours
	}
	d := time.Unix((nowHour+int64(k))*3600, 0).Sub(now)
	if d < time.Second {
		d = time.Second
	}
	return d
}

// admit 依次检查各窗口；全部放行时记录一次命中并返回 nil，否则返回首个触限
// 窗口（不记录命中——被拒请求不计入窗口）。计数在认证时点发生：后续请求校验
// 失败（400 等）不退还，与 RPM 同口径——只数"校验成功"会让畸形请求洪泛免费
// 消耗上游算力
func (t *gwWindowTracker) admit(id int64, specs []gwWindowSpec) *gwWindowRejection {
	now := time.Now()
	nowHour := now.Unix() / 3600
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.keys == nil {
		t.keys = map[int64]*gwHourRing{}
	}
	// 陈旧环兜底（forget 漏网/直接改库删 Key 的场景）：anchor 落后整环时长的
	// 环不可能再有窗口内命中，下次该 Key 回来时 advance 也会整环重置
	if len(t.keys) >= 4096 {
		for kid, kr := range t.keys {
			if nowHour-kr.anchor >= gwWindowRingHours {
				delete(t.keys, kid)
			}
		}
	}
	r := t.keys[id]
	if r == nil {
		r = &gwHourRing{}
		t.keys[id] = r
	}
	r.advance(nowHour)
	for _, sp := range specs {
		if sp.limit > 0 && r.sum(sp.hours) >= int64(sp.limit) {
			return &gwWindowRejection{window: sp.label, retryAfter: r.unlockAfter(sp.hours, sp.limit, nowHour, now)}
		}
	}
	r.counts[0]++
	return nil
}
