package main

// 2026-10 客户端兼容性修复轮（对齐 zai-org/ZCode 3.14.x 线协议）：
//   F1 mid-conversation role:system 放行（原 400 → 客户端不可重试硬失败）
//   F2 全账号限流耗尽 → 529 overloaded_error + Retry-After（原一律 503）
//   F3 /v1/messages 流式透传静默期注入 ping 保活（客户端有空闲超时 abort+重试）
//   F4 image url source 受控抓取内联（SSRF 防护，fail-closed）

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- F1 ----

func TestMidConversationSystemRoleAccepted(t *testing.T) {
	body := map[string]interface{}{
		"model": "GLM-5.3",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "system", "content": "mid-conversation note"},
			map[string]interface{}{"role": "assistant", "content": "ok"},
			map[string]interface{}{"role": "system", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "block-form note"},
			}},
			map[string]interface{}{"role": "user", "content": "go on"},
		},
	}
	if err := validateMessagesBody(context.Background(), body); err != nil {
		t.Fatalf("mid-conversation system role must pass validation, got: %v", err)
	}
	body["messages"] = []interface{}{
		map[string]interface{}{"role": "tool", "content": "x"},
	}
	err := validateMessagesBody(context.Background(), body)
	if err == nil || !strings.Contains(err.Error(), "must be user, assistant or system") {
		t.Fatalf("unknown role must still be rejected, got: %v", err)
	}
}

// ---- F2 ----

func TestNoAccountResponseShapes(t *testing.T) {
	cases := []struct {
		name       string
		rc         *relayCtx
		paidSkip   bool
		coolSecs   int64
		wantStatus int
		wantType   string
		wantRA     bool
	}{
		{"anthropic rate-limited", &relayCtx{proto: protocolAnthropic, sawRateLimit: true}, false, 45, 529, "overloaded_error", true},
		{"anthropic rate-limited default RA", &relayCtx{proto: protocolAnthropic, sawRateLimit: true}, false, 0, 529, "overloaded_error", true},
		{"no rate-limit seen stays 503", &relayCtx{proto: protocolAnthropic}, false, 0, 503, "no_available_account", false},
		{"openai protocol stays 503", &relayCtx{proto: protocolOpenAI, sawRateLimit: true}, false, 45, 503, "no_available_account", false},
		{"paid skipped stays 503", &relayCtx{proto: protocolAnthropic, sawRateLimit: true}, true, 45, 503, "no_available_account", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeAllAccountsUnavailable(w, c.rc, "all accounts unavailable", c.paidSkip, c.coolSecs)
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, c.wantStatus, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); (got != "") != c.wantRA {
				t.Fatalf("Retry-After = %q, wantRA=%v", got, c.wantRA)
			}
			if c.wantRA {
				if n, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || n < 1 || n > 300 {
					t.Fatalf("Retry-After %q out of clamp range", w.Header().Get("Retry-After"))
				}
			}
			if !strings.Contains(w.Body.String(), `"`+c.wantType+`"`) {
				t.Fatalf("body missing error type %q: %s", c.wantType, w.Body.String())
			}
			if c.wantStatus == 529 && !strings.Contains(w.Body.String(), `"type":"error"`) {
				t.Fatalf("529 body must use Anthropic error envelope: %s", w.Body.String())
			}
		})
	}
}

func TestRelayRateLimitExhaustionReturns529(t *testing.T) {
	p, db := newPaidTestPool(t)
	if err := db.SetSetting("captcha_mode", "off"); err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertAccount(mkDualAccount("overload", StatusActive))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer server.Close()
	cfg := &FileConfig{Upstream: UpstreamURLs{Zai: server.URL, ZaiFallback: server.URL, Bigmodel: server.URL}}
	z := &ZCodeAPI{cfg: cfg, db: db, pool: p, egress: NewEgressProxy(db),
		captcha: NewCaptchaService(cfg, db, "3.14.4"),
		routing: &EndpointRouter{snapshot: &routingSnapshot{expiresAt: time.Now().Add(time.Hour)}},
	}
	w := httptest.NewRecorder()
	z.relay(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), &relayCtx{
		provider: "zai", proto: protocolAnthropic,
		body: map[string]interface{}{"model": "GLM-5.3", "messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}}},
	})
	if w.Code != 529 {
		t.Fatalf("status = %d, want 529; body=%s", w.Code, w.Body.String())
	}
	n, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || n < 1 || n > 300 {
		t.Fatalf("Retry-After = %q, want clamped integer", w.Header().Get("Retry-After"))
	}
	out := w.Body.String()
	if !strings.Contains(out, `"overloaded_error"`) || !strings.Contains(out, `"type":"error"`) {
		t.Fatalf("body must be Anthropic overloaded_error envelope: %s", out)
	}
	if hits.Load() == 0 {
		t.Fatal("upstream must have been reached")
	}
	_ = id
}

// ---- F3 ----

// flushSignalRecorder 在代理 Flush 时发信号：测试据此得知 chunk1 已被消费，
// 静默窗口从这一刻起算——否则上游的 sleep 会与客户端建立/读取的耗时赛跑
// （-race 下首读可慢 100ms+，两个 chunk 在 socket 里合拢，代理根本没有
// 静默窗口可注入 ping，测试纯属掷硬币）
type flushSignalRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (r *flushSignalRecorder) Flush() {
	r.ResponseRecorder.Flush()
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

func TestSSEPassthroughInjectsPingDuringSilence(t *testing.T) {
	old := ssePingInterval
	ssePingInterval = 20 * time.Millisecond
	defer func() { ssePingInterval = old }()

	writeSecond := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		<-writeSecond // 静默窗口由测试精确控制（见上），不与读取耗时赛跑
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()

	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db}
	w := &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 8)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamProtocolResponse(w, &relayCtx{proto: protocolAnthropic, clientStream: true, clientModel: "my-model"},
			resp, &Account{}, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), []byte(`{}`), z, time.Now())
	}()

	<-w.flushed                     // chunk1 已消费：代理此刻必然空闲在 select 上
	time.Sleep(4 * ssePingInterval) // 真实静默 ≥4 个 tick，ping 必须注入
	close(writeSecond)
	<-done

	out := w.Body.String()
	if !strings.Contains(out, "event: ping") || !strings.Contains(out, `{"type":"ping"}`) {
		t.Fatalf("expected injected ping frames during upstream silence, got: %q", out)
	}
	if !strings.Contains(out, "message_start") || !strings.Contains(out, "message_stop") {
		t.Fatalf("upstream events must still be relayed verbatim, got: %q", out)
	}
}

// ---- F4 ----

func TestForbiddenImageHostIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true}, {"10.1.2.3", true}, {"192.168.1.1", true}, {"172.16.0.1", true},
		{"100.64.0.1", true}, {"169.254.1.1", true}, {"0.0.0.0", true}, {"::1", true},
		{"fc00::1", true}, {"fe80::1", true}, {"8.8.8.8", false}, {"2001:db8::1", false},
	}
	for _, c := range cases {
		if got := isForbiddenImageHostIP(net.ParseIP(c.ip)); got != c.blocked {
			t.Errorf("isForbiddenImageHostIP(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
}

func TestImageURLSourceInlined(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte("fakepngdata")...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	}))
	defer srv.Close()
	old := imageFetchHTTPClient
	imageFetchHTTPClient = &http.Client{} // 无防护 client：允许测试触达环回 httptest
	defer func() { imageFetchHTTPClient = old }()

	build := func(url string) map[string]interface{} {
		return map[string]interface{}{
			"model": "GLM-4.6",
			"messages": []interface{}{map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hi"},
					map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": url}},
				},
			}},
		}
	}
	body := build(srv.URL)
	if err := validateMessagesBody(context.Background(), body); err != nil {
		t.Fatalf("url image must be inlined and accepted, got: %v", err)
	}
	blocks := body["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	src := blocks[1].(map[string]interface{})["source"].(map[string]interface{})
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Fatalf("source not rewritten to base64/png: %v", src)
	}
	if want := base64.StdEncoding.EncodeToString(png); src["data"] != want {
		t.Fatal("inlined data mismatch")
	}

	for _, c := range []struct {
		name    string
		serve   func(w http.ResponseWriter, r *http.Request)
		wantErr string
	}{
		{"404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }, "HTTP 404"},
		{"bad content-type", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html>")
		}, "unsupported content-type"},
		{"oversize", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(make([]byte, maxImageFetchBytes+1))
		}, "exceeds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sub := httptest.NewServer(http.HandlerFunc(c.serve))
			defer sub.Close()
			err := validateMessagesBody(context.Background(), build(sub.URL))
			if err == nil || !strings.Contains(err.Error(), "image url fetch failed") || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("expected fetch failure (%s), got: %v", c.wantErr, err)
			}
		})
	}
}

// 默认带防护的 client 必须拒绝环回地址（httptest 即环回）：SSRF 防护生效
func TestImageURLPrivateAddressBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer srv.Close()
	body := map[string]interface{}{
		"model": "GLM-4.6",
		"messages": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": srv.URL + "/x.png"}},
			},
		}},
	}
	err := validateMessagesBody(context.Background(), body)
	if err == nil || !strings.Contains(err.Error(), "image url fetch failed") || !strings.Contains(err.Error(), "local/private") {
		t.Fatalf("loopback fetch must be refused by SSRF guard, got: %v", err)
	}
}

// ---- R2 轮（openai 兼容模式 + 非流式 TTFB + request-id 遥测）----

// 非流式转发的 transport 首字节上限必须放宽到 10min；流式保持 60s 黑洞防护
func TestSlowTTFBClientsUseLongerHeaderTimeout(t *testing.T) {
	slow := ClientForURLSlowTTFB("", "https://api.z.ai/v1/messages")
	tr, ok := slow.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("slow client transport type %T", slow.Transport)
	}
	if tr.ResponseHeaderTimeout != slowTTFBHeaderTimeout {
		t.Fatalf("slow std ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, slowTTFBHeaderTimeout)
	}
	if slow.Timeout != 0 {
		t.Fatalf("slow client Timeout = %v, want 0", slow.Timeout)
	}
	slowFP := ClientForURLSlowTTFB("", "https://zcode.z.ai/api/v1/x")
	trFP, ok := slowFP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("slow fingerprint transport type %T", slowFP.Transport)
	}
	if trFP.ResponseHeaderTimeout != slowTTFBHeaderTimeout {
		t.Fatalf("slow fp ResponseHeaderTimeout = %v", trFP.ResponseHeaderTimeout)
	}
	fast := ClientForURL("", "https://api.z.ai/v1/messages", 0)
	trFast := fast.Transport.(*http.Transport)
	if trFast.ResponseHeaderTimeout != upstreamHeaderTimeout {
		t.Fatalf("fast ResponseHeaderTimeout = %v, want %v", trFast.ResponseHeaderTimeout, upstreamHeaderTimeout)
	}
}

// ZCode option-map 给 openai-compat 请求注入四种推理控制形状：全部归一
func TestOpenAIThinkingControlPassthrough(t *testing.T) {
	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db, cfg: &FileConfig{}}
	conv := func(extra map[string]interface{}) map[string]interface{} {
		body := map[string]interface{}{
			"model":    "GLM-5.3",
			"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		}
		for k, v := range extra {
			body[k] = v
		}
		out, err := openaiToAnthropic(body)
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if err := normalizeBody(out, z); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		return out
	}
	assertEffort := func(t *testing.T, out map[string]interface{}, want string) {
		t.Helper()
		th, _ := out["thinking"].(map[string]interface{})
		if th["type"] != "adaptive" {
			t.Fatalf("thinking = %v, want adaptive", out["thinking"])
		}
		oc, _ := out["output_config"].(map[string]interface{})
		if oc["effort"] != want {
			t.Fatalf("effort = %v, want %q", oc["effort"], want)
		}
	}
	t.Run("reasoning.effort max", func(t *testing.T) {
		out := conv(map[string]interface{}{"reasoning": map[string]interface{}{"effort": "max"}})
		assertEffort(t, out, "max")
	})
	t.Run("reasoning_effort low", func(t *testing.T) {
		out := conv(map[string]interface{}{"reasoning_effort": "low"})
		assertEffort(t, out, "low")
	})
	t.Run("enable_thinking false", func(t *testing.T) {
		out := conv(map[string]interface{}{"enable_thinking": false})
		if th, _ := out["thinking"].(map[string]interface{}); th["type"] != "disabled" {
			t.Fatalf("thinking = %v, want disabled", out["thinking"])
		}
	})
	t.Run("thinking budget 1024", func(t *testing.T) {
		out := conv(map[string]interface{}{"thinking": map[string]interface{}{"type": "enabled", "budget_tokens": float64(1024)}})
		assertEffort(t, out, "low")
	})
	t.Run("reasoning.effort yields to reasoning_effort", func(t *testing.T) {
		out := conv(map[string]interface{}{
			"reasoning":        map[string]interface{}{"effort": "low"},
			"reasoning_effort": "high",
		})
		assertEffort(t, out, "high")
	})
}

// 非流式成功响应也要转发上游 request-id（客户端遥测按 x-request-id → request-id 读取）
func TestRelayNonStreamForwardsRequestID(t *testing.T) {
	p, db := newPaidTestPool(t)
	if err := db.SetSetting("captcha_mode", "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAccount(mkDualAccount("reqid", StatusActive)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-abc123")
		io.WriteString(w, `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"GLM-5.3","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()
	cfg := &FileConfig{Upstream: UpstreamURLs{Zai: server.URL, ZaiFallback: server.URL, Bigmodel: server.URL}}
	z := &ZCodeAPI{cfg: cfg, db: db, pool: p, egress: NewEgressProxy(db),
		captcha: NewCaptchaService(cfg, db, "3.14.4"),
		routing: &EndpointRouter{snapshot: &routingSnapshot{expiresAt: time.Now().Add(time.Hour)}},
	}
	w := httptest.NewRecorder()
	z.relay(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), &relayCtx{
		provider: "zai", proto: protocolAnthropic, clientStream: false,
		body: map[string]interface{}{"model": "GLM-5.3", "messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Request-Id"); got != "req-abc123" {
		t.Fatalf("X-Request-Id = %q, want req-abc123", got)
	}
}

// ---- 轮 3（SDK 闭环清单 + 红队）----

// fixThinking 5.3 重写必须保留 output_config 的其余键（format.json_schema / task_budget）
func TestFixThinkingPreservesOutputConfigExtras(t *testing.T) {
	body := map[string]interface{}{
		"model":    "GLM-5.3",
		"thinking": map[string]interface{}{"type": "enabled"},
		"output_config": map[string]interface{}{
			"effort": "max",
			"format": map[string]interface{}{"type": "json_schema", "schema": map[string]interface{}{"type": "object"}},
			"task_budget": map[string]interface{}{
				"type": "tokens", "total": float64(100000), "remaining": float64(90000),
			},
		},
	}
	fixThinking(body)
	oc, _ := body["output_config"].(map[string]interface{})
	if oc["effort"] != "max" {
		t.Fatalf("effort = %v, want max", oc["effort"])
	}
	if oc["format"] == nil || oc["task_budget"] == nil {
		t.Fatalf("output_config extras dropped: %v", oc)
	}
	if th, _ := body["thinking"].(map[string]interface{}); th["type"] != "adaptive" {
		t.Fatalf("thinking = %v, want adaptive", body["thinking"])
	}
}

// URL 图片内联必须有单请求额度：数量上限、字节上限、同 URL 去重（红队 F1/F2）
func TestImageInlineBudget(t *testing.T) {
	old := imageFetchHTTPClient
	imageFetchHTTPClient = &http.Client{}
	defer func() { imageFetchHTTPClient = old }()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4})
	}))
	defer srv.Close()

	mkBlock := func(url string) map[string]interface{} {
		return map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": url}}
	}
	mkBody := func(blocks ...map[string]interface{}) map[string]interface{} {
		content := []interface{}{map[string]interface{}{"type": "text", "text": "hi"}}
		for _, b := range blocks {
			content = append(content, b)
		}
		return map[string]interface{}{"model": "GLM-4.6", "messages": []interface{}{
			map[string]interface{}{"role": "user", "content": content},
		}}
	}

	t.Run("count cap", func(t *testing.T) {
		blocks := make([]map[string]interface{}, 0, maxInlineImagesPerRequest+1)
		for i := 0; i <= maxInlineImagesPerRequest; i++ {
			blocks = append(blocks, mkBlock(fmt.Sprintf("%s/%d.png", srv.URL, i)))
		}
		err := validateMessagesBody(context.Background(), mkBody(blocks...))
		if err == nil || !strings.Contains(err.Error(), "too many url images") {
			t.Fatalf("expected count-cap error, got: %v", err)
		}
	})
	t.Run("url dedup fetches once", func(t *testing.T) {
		before := hits.Load()
		body := mkBody(mkBlock(srv.URL+"/dup.png"), mkBlock(srv.URL+"/dup.png"), mkBlock(srv.URL+"/dup.png"))
		if err := validateMessagesBody(context.Background(), body); err != nil {
			t.Fatalf("dedup validate failed: %v", err)
		}
		if got := hits.Load() - before; got != 1 {
			t.Fatalf("same url fetched %d times, want 1", got)
		}
	})
	t.Run("bytes cap", func(t *testing.T) {
		big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(make([]byte, maxImageFetchBytes)) // 5MB × 7 = 35MB base64 > 32MB 上限
		}))
		defer big.Close()
		blocks := make([]map[string]interface{}, 0, 7)
		for i := 0; i < 7; i++ {
			blocks = append(blocks, mkBlock(fmt.Sprintf("%s/%d.png", big.URL, i)))
		}
		err := validateMessagesBody(context.Background(), mkBody(blocks...))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("expected bytes-cap error, got: %v", err)
		}
	})
}

// 抓取必须绑定请求 ctx：客户端断开时中止（红队 F2）
func TestImageFetchAbortsOnClientDisconnect(t *testing.T) {
	old := imageFetchHTTPClient
	imageFetchHTTPClient = &http.Client{}
	defer func() { imageFetchHTTPClient = old }()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G'})
		w.(http.Flusher).Flush()
		<-release // 挂住响应体
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	body := map[string]interface{}{"model": "GLM-4.6", "messages": []interface{}{
		map[string]interface{}{"role": "user", "content": []interface{}{
			map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": srv.URL + "/x.png"}},
		}},
	}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := validateMessagesBody(ctx, body)
	if err == nil || !strings.Contains(err.Error(), "image url fetch failed") {
		t.Fatalf("expected ctx-aborted fetch error, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("fetch did not abort on cancel: %v", elapsed)
	}
}
