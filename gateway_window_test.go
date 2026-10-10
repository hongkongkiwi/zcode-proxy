package main

import (
	"path/filepath"
	"testing"
	"time"
)

// gwWindowSpecsFor 构造与生产 resolveGatewayKey 相同的三窗口规格
func gwWindowSpecsFor(k *GatewayKey) []gwWindowSpec {
	return []gwWindowSpec{
		{hours: 5, limit: k.Rate5h, label: "5h"},
		{hours: 24, limit: k.Rate1d, label: "24h"},
		{hours: gwWindowRingHours, limit: k.Rate7d, label: "7d"},
	}
}

// TestGWWindowAdmitLimits 逐窗口触限：5h 先满时拒绝带 5h 标签，其余窗口不受影响
func TestGWWindowAdmitLimits(t *testing.T) {
	tr := &gwWindowTracker{}
	key := &GatewayKey{ID: 1, Rate5h: 3, Rate1d: 10, Rate7d: 100}
	specs := gwWindowSpecsFor(key)
	for i := 0; i < 3; i++ {
		if rej := tr.admit(1, specs); rej != nil {
			t.Fatalf("admit %d: unexpected rejection %+v", i, rej)
		}
	}
	rej := tr.admit(1, specs)
	if rej == nil {
		t.Fatal("expected 5h window rejection after 3 hits")
	}
	if rej.window != "5h" {
		t.Fatalf("expected rejection window 5h, got %s", rej.window)
	}
	// 3 次命中都在当前小时 → 解除点 = anchor+5 整点（5h 窗口最老侧移出）
	if rej.retryAfter <= 0 || rej.retryAfter > 5*time.Hour+2*time.Second {
		t.Fatalf("retryAfter should be (0,5h], got %v", rej.retryAfter)
	}
	// 5h 触限后 1d/7d 限额更宽，拒绝仍应来自 5h（首个触限窗口）
	for i := 0; i < 5; i++ {
		if r := tr.admit(1, specs); r == nil || r.window != "5h" {
			t.Fatalf("expected continued 5h rejection, got %+v", r)
		}
	}
}

// TestGWWindowUnlimitedPasses 全 0 = 不限：大量请求不拒绝
func TestGWWindowUnlimitedPasses(t *testing.T) {
	tr := &gwWindowTracker{}
	specs := gwWindowSpecsFor(&GatewayKey{ID: 2})
	for i := 0; i < 1000; i++ {
		if rej := tr.admit(2, specs); rej != nil {
			t.Fatalf("admit %d: unexpected rejection %+v", i, rej)
		}
	}
}

// TestGWWindowRingAdvance 环形推进：模拟时间流逝，老桶移出窗口后计数回落。
// 直接操纵 anchor 模拟"下一个小时"（advance 由 admit 内部调用，这里验证
// sum 对桶滑出的响应，以及 168 小时整环重置与时钟回拨处理）
func TestGWWindowRingAdvance(t *testing.T) {
	r := &gwHourRing{}
	nowHour := time.Now().Unix() / 3600
	r.advance(nowHour)
	r.counts[0] = 5
	if got := r.sum(5); got != 5 {
		t.Fatalf("sum(5) after fill = %d, want 5", got)
	}
	// 推进 1 小时：命中进入窗口历史，sum(5) 仍含它
	r.advance(nowHour + 1)
	if got := r.sum(5); got != 5 {
		t.Fatalf("sum(5) after 1h advance = %d, want 5", got)
	}
	// 再推进 4 小时：命中落在 5 桶窗口外
	r.advance(nowHour + 5)
	if got := r.sum(5); got != 0 {
		t.Fatalf("sum(5) after 5h advance = %d, want 0 (bucket slid out)", got)
	}
	// 推进 200 小时（> 168）：整环重置不 panic、计数清零
	r.advance(nowHour + 200)
	if got := r.sum(168); got != 0 {
		t.Fatalf("sum(168) after full-ring advance = %d, want 0", got)
	}
	// 时钟回拨：按重置处理，不得 panic 或保留未来桶
	r.counts[0] = 9
	r.advance(nowHour - 10)
	if got := r.sum(24); got != 0 {
		t.Fatalf("sum(24) after clock rollback = %d, want 0", got)
	}
}

// TestGWWindowIndependentKeys 两把 Key 各自计数，互不影响
func TestGWWindowIndependentKeys(t *testing.T) {
	tr := &gwWindowTracker{}
	specs := gwWindowSpecsFor(&GatewayKey{Rate5h: 1})
	if rej := tr.admit(10, specs); rej != nil {
		t.Fatalf("key10 first admit rejected: %+v", rej)
	}
	if rej := tr.admit(10, specs); rej == nil {
		t.Fatal("key10 second admit should hit 5h limit")
	}
	if rej := tr.admit(11, specs); rej != nil {
		t.Fatalf("key11 should be independent, got %+v", rej)
	}
}

// TestGWWindowUnlockAfter Rollback-After 诚实性（codex 评审 F3）：解除时点
// 指向"足够最老桶滑出"的整点，而不是无脑下一小时
func TestGWWindowUnlockAfter(t *testing.T) {
	nowHour := time.Now().Unix() / 3600
	now := time.Now()

	// 场景 1：命中在当前小时，5h 窗口 limit=1 → 解除 = anchor+5（约 4-5h 后）
	r := &gwHourRing{}
	r.advance(nowHour)
	r.counts[0] = 1
	d := r.unlockAfter(5, 1, nowHour, now)
	want := time.Unix((nowHour+5)*3600, 0).Sub(now)
	if d < want-2*time.Second || d > want+2*time.Second {
		t.Fatalf("hit-in-current-hour: retryAfter %v want ~%v", d, want)
	}

	// 场景 2：命中在 4 小时前（index 4），5h 窗口 limit=1 → 解除 = 下一整点
	// （再推 1 小时它就滑出窗口），而不是 5 小时后
	r2 := &gwHourRing{}
	r2.advance(nowHour)
	r2.counts[4] = 1
	d2 := r2.unlockAfter(5, 1, nowHour, now)
	want2 := time.Unix((nowHour+1)*3600, 0).Sub(now)
	if d2 < want2-2*time.Second || d2 > want2+2*time.Second {
		t.Fatalf("old-bucket hit: retryAfter %v want ~%v", d2, want2)
	}

	// 场景 3：混合（老桶 3 + 新桶 3，limit=5）→ 移出老桶即解除 = 下一整点
	r3 := &gwHourRing{}
	r3.advance(nowHour)
	r3.counts[4] = 3
	r3.counts[0] = 3
	d3 := r3.unlockAfter(5, 5, nowHour, now)
	if d3 < want2-2*time.Second || d3 > want2+2*time.Second {
		t.Fatalf("mixed buckets: retryAfter %v want ~%v", d3, want2)
	}

	// 场景 4：24h 窗口 + 命中在当前小时 → 解除 = anchor+24
	d4 := r.unlockAfter(24, 1, nowHour, now)
	want4 := time.Unix((nowHour+24)*3600, 0).Sub(now)
	if d4 < want4-2*time.Second || d4 > want4+2*time.Second {
		t.Fatalf("24h window: retryAfter %v want ~%v", d4, want4)
	}
}

// TestGWWindowForget 删除 Key 清窗口状态（codex 评审 F4）：create→use→delete
// 不在内存留环
func TestGWWindowForget(t *testing.T) {
	tr := &gwWindowTracker{}
	specs := gwWindowSpecsFor(&GatewayKey{Rate5h: 100})
	if rej := tr.admit(7, specs); rej != nil {
		t.Fatalf("admit rejected: %+v", rej)
	}
	tr.mu.Lock()
	n := len(tr.keys)
	tr.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 ring after admit, got %d", n)
	}
	tr.forget(7)
	tr.mu.Lock()
	n = len(tr.keys)
	tr.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected 0 rings after forget, got %d", n)
	}
}

// TestGWWindowStaleRingBackstop admit 的陈旧环兜底：anchor 落后整环时长的环
// 在 map 达到 4096 时被清理，新 Key 正常计数
func TestGWWindowStaleRingBackstop(t *testing.T) {
	tr := &gwWindowTracker{}
	staleHour := time.Now().Unix()/3600 - gwWindowRingHours - 1
	tr.mu.Lock()
	tr.keys = map[int64]*gwHourRing{}
	for i := int64(1); i <= 4096; i++ {
		tr.keys[i] = &gwHourRing{anchor: staleHour}
	}
	tr.mu.Unlock()
	specs := gwWindowSpecsFor(&GatewayKey{Rate5h: 1})
	if rej := tr.admit(999999, specs); rej != nil {
		t.Fatalf("fresh key admit rejected: %+v", rej)
	}
	tr.mu.Lock()
	n := len(tr.keys)
	tr.mu.Unlock()
	if n != 1 {
		t.Fatalf("stale rings should be pruned to just the fresh key, got %d", n)
	}
}

// TestResolveGatewayKeyWindowLimit 端到端：resolveGatewayKey 对滚动窗口触限的
// Key 返回 429 且 Retry-After 写入 errResp（小时边界粒度）
func TestResolveGatewayKeyWindowLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gw-window-test.db")
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	am := &AuthManager{db: db}
	plain := GenerateAPIKey()
	_, err = db.CreateGatewayKey(&GatewayKey{
		Name: "win", KeyHash: HashGatewayKey(plain), KeyPrefix: plain[:10] + "…",
		Enabled: true, Rate5h: 1,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	for i := 0; i < 2; i++ {
		_, errResp := am.resolveGatewayKey(plain)
		if i == 0 {
			if errResp != nil {
				t.Fatalf("first resolve rejected: %+v", errResp)
			}
			continue
		}
		if errResp == nil {
			t.Fatal("second resolve should be rate limited (5h window=1)")
		}
		if errResp.status != 429 {
			t.Fatalf("expected 429, got %d", errResp.status)
		}
		if errResp.retryAfterSeconds <= 0 || errResp.retryAfterSeconds > 5*3600+60 {
			t.Fatalf("retryAfterSeconds should be (0,5h+slack], got %d", errResp.retryAfterSeconds)
		}
	}
}
