package main

// 2026-10 客户端兼容性修复轮（对齐 zai-org/ZCode 3.14.x 线协议）：
//   F1 mid-conversation role:system 放行（原 400 → 客户端不可重试硬失败）
//   F2 全账号限流耗尽 → 529 overloaded_error + Retry-After（原一律 503）
//   F3 /v1/messages 流式透传静默期注入 ping 保活（客户端有空闲超时 abort+重试）
//   F4 image url source 受控抓取内联（SSRF 防护，fail-closed）

import (
	"encoding/base64"
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
	if err := validateMessagesBody(body); err != nil {
		t.Fatalf("mid-conversation system role must pass validation, got: %v", err)
	}
	body["messages"] = []interface{}{
		map[string]interface{}{"role": "tool", "content": "x"},
	}
	err := validateMessagesBody(body)
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

func TestSSEPassthroughInjectsPingDuringSilence(t *testing.T) {
	old := ssePingInterval
	ssePingInterval = 20 * time.Millisecond
	defer func() { ssePingInterval = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(120 * time.Millisecond) // 静默窗口：必须注入 ping
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
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	streamProtocolResponse(w, &relayCtx{proto: protocolAnthropic, clientStream: true, clientModel: "my-model"},
		resp, &Account{}, req, []byte(`{}`), z, time.Now())

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
	if err := validateMessagesBody(body); err != nil {
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
			err := validateMessagesBody(build(sub.URL))
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
	err := validateMessagesBody(body)
	if err == nil || !strings.Contains(err.Error(), "image url fetch failed") || !strings.Contains(err.Error(), "local/private") {
		t.Fatalf("loopback fetch must be refused by SSRF guard, got: %v", err)
	}
}
