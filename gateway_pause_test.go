package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- 网关全局暂停（panic stop）----

// TestGatewayPausedFlag gatewayPaused 只认 "1"；读失败按未暂停
func TestGatewayPausedFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pause-test.db")
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	z := &ZCodeAPI{cfg: &FileConfig{}, db: db, pool: NewAccountPool(db, &FileConfig{}, "test")}
	if z.gatewayPaused() {
		t.Fatal("default must be unpaused")
	}
	if err := db.SetSetting("gateway_paused", "1"); err != nil {
		t.Fatal(err)
	}
	if !z.gatewayPaused() {
		t.Fatal("paused=1 must read back true")
	}
	if err := db.SetSetting("gateway_paused", "0"); err != nil {
		t.Fatal(err)
	}
	if z.gatewayPaused() {
		t.Fatal("paused=0 must read back false")
	}
}

// TestRelayPausedGate503 暂停打开后 relay() 在任何账号选择/网关 Key 限额之前
// 直接 503；恢复后不再拦截
func TestRelayPausedGate503(t *testing.T) {
	p, db := newPoolTestPool(t)
	z := &ZCodeAPI{cfg: &FileConfig{}, db: db, pool: p}
	rc := &relayCtx{proto: protocolAnthropic}
	r := httptest.NewRequest("POST", "/v1/messages", nil)

	if err := db.SetSetting("gateway_paused", "1"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	z.relay(w, r, rc)
	if w.Code != 503 {
		t.Fatalf("paused gateway should 503, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "panic stop") {
		t.Fatalf("503 body should name the pause state, got %s", w.Body.String())
	}

	if err := db.SetSetting("gateway_paused", "0"); err != nil {
		t.Fatal(err)
	}
	// 恢复后走正常链路（无账号 → 503 无账号可用文案，而非暂停文案）
	w2 := httptest.NewRecorder()
	z.relay(w2, r, rc)
	if strings.Contains(w2.Body.String(), "panic stop") {
		t.Fatalf("resumed gateway must not return pause message, got %s", w2.Body.String())
	}
}

// TestPutSettingsPauseValidation 暂停开关只收 0/1
func TestPutSettingsPauseValidation(t *testing.T) {
	_, db := newPoolTestPool(t)
	s := &APIServer{db: db}
	for _, v := range []string{"true", "yes", "2", "-1"} {
		req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"gateway_paused":"`+v+`"}`))
		w := httptest.NewRecorder()
		s.handlePutSettings(w, req)
		if w.Code != 400 {
			t.Fatalf("gateway_paused=%q should 400, got %d", v, w.Code)
		}
	}
	// 非白名单键继续被忽略：别的值不受影响
	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"gateway_paused":"1","bogus_key":"x"}`))
	w := httptest.NewRecorder()
	s.handlePutSettings(w, req)
	if w.Code != 200 {
		t.Fatalf("valid pause write should 200, got %d body=%s", w.Code, w.Body.String())
	}
	got, err := db.GetSetting("gateway_paused")
	if err != nil || got != "1" {
		t.Fatalf("gateway_paused should persist as 1, got %q err=%v", got, err)
	}
}

// ---- 长窗口聚合（热力图 + 24h 小时趋势）----

func TestUsageHeatmapSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heatmap-test.db")
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// 空库：两段均为空数组而非 nil/error
	out, err := db.UsageHeatmap()
	if err != nil {
		t.Fatalf("empty heatmap: %v", err)
	}
	if days, ok := out["daily_365"].([]dayCell); ok && len(days) != 0 {
		t.Fatalf("empty daily_365 should be len 0")
	}

	// 两条记录（一个当前小时、一个 8 小时前）：小时段 2 桶、日段 1 天。
	// InsertUsageRecord 不接 created_at（建表默认 now），显式时间戳走直插
	now := time.Now()
	for _, rec := range []struct {
		at    time.Time
		total int
	}{
		{now, 15},
		{now.Add(-8 * time.Hour), 10},
	} {
		if _, err := db.conn.Exec(`INSERT INTO usage_records
			(created_at, model, prompt_tokens, completion_tokens, total_tokens, status_code)
			VALUES (?,?,?,?,?,?)`,
			rec.at.Format("2006-01-02 15:04:05"), "glm-5.3", 10, 5, rec.total, 200); err != nil {
			t.Fatal(err)
		}
	}
	out2, err := db.UsageHeatmap()
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	hours, _ := out2["hourly_24"].([]hourCell)
	if len(hours) != 2 {
		t.Fatalf("hourly_24 should have 2 buckets, got %d", len(hours))
	}
	// 小时桶升序：第一条是 8 小时前的记录
	if hours[0].Requests != 1 || hours[1].Requests != 1 {
		t.Fatalf("unexpected hourly buckets: %+v", hours)
	}
	days, _ := out2["daily_365"].([]dayCell)
	if len(days) == 0 {
		t.Fatal("daily_365 should have at least 1 day after inserts")
	}
	total := int64(0)
	for _, d := range days {
		total += d.Tokens
	}
	if total != 25 {
		t.Fatalf("daily_365 tokens should sum to 25, got %d", total)
	}
}
