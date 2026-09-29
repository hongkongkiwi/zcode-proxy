package main

// ---- Off-Peak 异步免费通道（B2，移植 zcode-api src/async）----
//
// 上游在闲时开放免费算力队列：先取票（ticket），排队期间客户端连接以
// SSE 注释帧保活，票面 ready 后携带 X-Off-Peak-Ticket-ID 调用
// /api/v1/off-peak/anthropic/v1/messages，结束后 settle 关票。
//
// 控制面（均为 Bearer JWT + x-coding-plan-api-key 鉴权，路径不签名）：
//   GET  /api/v1/off-peak/ticket/availability   探队 {can_take_number,next_take_at}
//   POST /api/v1/off-peak/ticket                取票 {task_id} → {ticket_id,state,...}
//   POST /api/v1/off-peak/ticket/status         批量查票 {ticket_ids:[..]} (≤100)
//   POST /api/v1/off-peak/ticket/{id}/settle    关票（无 body；4xx 视为已清理成功）
//
// 票状态机：queued → ready/active → settled；expired/not_found 需重新取票。
// 服务端 next_poll_after / active_deadline / next_take_at 均为 unix 秒。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	offPeakOrigin        = "https://zcode.z.ai"
	offPeakControlBase   = offPeakOrigin + "/api/v1/off-peak"
	offPeakMessagesURL   = offPeakOrigin + "/api/v1/off-peak/anthropic/v1/messages"
	offPeakControlTo     = 15 * time.Second
	offPeakBatchMax      = 100
	offPeakDefaultPollMS = 1000
	offPeakKeepaliveMS   = 15000
	offPeakMaxRetries    = 3
	offPeakMaxWaitSec    = 1800
)

// ---- 设置 ----

func (z *ZCodeAPI) asyncEnabled() bool {
	v, _ := z.db.GetSetting("async_enabled")
	return v == "1" || v == "true"
}

func (z *ZCodeAPI) asyncIntSetting(key string, def int) int {
	v, err := z.db.GetSetting(key)
	if err != nil || v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return def
}

// ---- 控制面客户端 ----

type offPeakTickets struct{ z *ZCodeAPI }

// do 控制面请求；settle4xxOK 时 4xx 视为服务端已清理的成功（settle 专用）
func (t offPeakTickets) do(a *Account, method, path string, body interface{}, settle4xxOK bool) (map[string]interface{}, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	urlStr := offPeakControlBase + path
	req, err := http.NewRequest(method, urlStr, rdr)
	if err != nil {
		return nil, err
	}
	id := NewClientIdentity(t.z.appVersion, a.DeviceMid)
	for k, v := range ZaiClientHeaders(id) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+a.ZCodeJWT)
	if a.APIKey != "" {
		req.Header.Set("x-coding-plan-api-key", a.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := ClientForURL(t.z.egress.ProxyURLForAccount(a), urlStr, offPeakControlTo)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		if settle4xxOK {
			return map[string]interface{}{}, nil
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var v map[string]interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("响应非 JSON: %s", truncate(string(raw), 120))
	}
	// 信封解包规则（对齐 zcode-api client.ts request）：code===0 且有 data 才解包；
	// code!==0 报错；无 code 的含 data 对象保守不解包
	if codeVal, has := v["code"]; has {
		n := jsonInt(v, "code")
		_ = codeVal
		if n != 0 {
			return nil, fmt.Errorf("业务码 %d: %s", n, firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message"), truncate(string(raw), 120)))
		}
		if d, ok := v["data"].(map[string]interface{}); ok {
			return d, nil
		}
	}
	return v, nil
}

// Availability 探队：canTake=false 时返回 nextTakeAt（unix 秒，0=未知）
func (t offPeakTickets) Availability(a *Account) (canTake bool, nextTakeAt int64, err error) {
	v, err := t.do(a, "GET", "/ticket/availability", nil, false)
	if err != nil {
		return false, 0, err
	}
	canTake = v["can_take_number"] == true
	if n, ok := v["next_take_at"].(float64); ok {
		nextTakeAt = int64(n)
	}
	return canTake, nextTakeAt, nil
}

// offTicket 控制面响应的归一化票
type offTicket struct {
	ID             string
	State          string
	Position       int
	NextPollSec    int // 服务端建议的下轮轮询间隔（秒）
	ActiveDeadline int64
}

func offTicketFrom(m map[string]interface{}) (*offTicket, error) {
	id, _ := m["ticket_id"].(string)
	state, _ := m["state"].(string)
	if id == "" || state == "" {
		return nil, fmt.Errorf("票响应缺少 ticket_id/state")
	}
	tk := &offTicket{ID: id, State: state}
	if n, ok := m["position"].(float64); ok {
		tk.Position = int(n)
	}
	if n, ok := m["active_deadline"].(float64); ok {
		tk.ActiveDeadline = int64(n)
	}
	return tk, nil
}

func (t offPeakTickets) take(a *Account, taskID string) (*offTicket, error) {
	v, err := t.do(a, "POST", "/ticket", map[string]interface{}{"task_id": taskID}, false)
	if err != nil {
		return nil, err
	}
	tk, err := offTicketFrom(v)
	if err != nil {
		return nil, err
	}
	if n, ok := v["next_poll_after"].(float64); ok && n > 0 {
		tk.NextPollSec = int(n)
	}
	return tk, nil
}

func (t offPeakTickets) status(a *Account, ticketID string) (*offTicket, error) {
	v, err := t.do(a, "POST", "/ticket/status", map[string]interface{}{"ticket_ids": []string{ticketID}}, false)
	if err != nil {
		return nil, err
	}
	tickets, _ := v["tickets"].([]interface{})
	for _, item := range tickets {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if id, _ := m["ticket_id"].(string); id == ticketID {
			tk, err := offTicketFrom(m)
			if err != nil {
				return nil, err
			}
			if n, ok := v["next_poll_after"].(float64); ok && n > 0 {
				tk.NextPollSec = int(n)
			}
			return tk, nil
		}
	}
	return nil, fmt.Errorf("查票响应不含该票")
}

// settle 关票；4xx 已按成功处理；失败仅记日志（关票是 best-effort）
func (t offPeakTickets) settle(a *Account, ticketID string) {
	if _, err := t.do(a, "POST", "/ticket/"+ticketID+"/settle", nil, true); err != nil {
		log.Printf("[async] settle %s… failed: %v", safePrefixLog(ticketID, 8), err)
	}
}

func offPeakStateReady(s string) bool    { return s == "ready" || s == "active" }
func offPeakStateExpired(s string) bool  { return s == "expired" || s == "not_found" }
func offPeakStateTerminal(s string) bool { return s == "settled" || offPeakStateExpired(s) }

// ---- 网关入口 ----

// HandleAsyncMessages POST /async/v1/messages — Anthropic 原生协议经闲时队列
func (z *ZCodeAPI) HandleAsyncMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !z.asyncEnabled() {
		writeAPIError(w, http.StatusServiceUnavailable, "async (off-peak) channel is disabled; enable async_enabled in settings")
		return
	}
	body, errResp := readJSONBody(r)
	if errResp != nil {
		errResp.Write(w)
		return
	}
	if provider := detectProvider(body, r.Header); provider != "zai" {
		writeAPIError(w, http.StatusBadRequest, "async channel only supports zai accounts")
		return
	}
	if err := normalizeBody(body, z); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateMessagesBody(body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, _ := json.Marshal(body)
	clientStream, _ := body["stream"].(bool)

	z.runOffPeak(w, r, offPeakRunOpts{
		payload:      payload,
		group:        r.Header.Get("x-zcode-group"),
		clientStream: clientStream,
		pollMS:       z.asyncIntSetting("async_poll_interval_ms", offPeakDefaultPollMS),
		keepaliveMS:  z.asyncIntSetting("async_keepalive_ms", offPeakKeepaliveMS),
		maxRetries:   z.asyncIntSetting("async_max_retries", offPeakMaxRetries),
		maxWaitSec:   z.asyncIntSetting("async_max_wait_sec", offPeakMaxWaitSec),
	})
}

type offPeakRunOpts struct {
	payload      []byte
	group        string
	clientStream bool
	pollMS       int
	keepaliveMS  int
	maxRetries   int
	maxWaitSec   int
}

// runOffPeak 选号并执行闲时桥接；跨账号重试语义与 relay 一致
func (z *ZCodeAPI) runOffPeak(w http.ResponseWriter, r *http.Request, opts offPeakRunOpts) {
	tickets := offPeakTickets{z: z}
	tried := map[int64]bool{}
	var reasons []string
	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		a := z.pool.Select("zai", opts.group, tried)
		if a == nil {
			break
		}
		tried[a.ID] = true
		if a.ZCodeJWT == "" {
			continue
		}
		if z.offPeakBridge(w, r, a, tickets, opts) == outcomeWritten {
			return
		}
		reasons = append(reasons, a.DisplayNameOrEmail()+": "+firstNonEmpty(a.LastError, a.Status))
	}
	detail := truncate(strings.Join(dedup(reasons), "；"), 400)
	msg := "闲时通道暂不可用（所有账号取票失败），请稍后重试或改用 /v1/messages"
	if detail != "" {
		msg += "（最近失败原因: " + detail + "）"
	}
	log.Printf("[async] no available account: %s", detail)
	if opts.clientStream {
		writeSSEErrorEvent(w, msg)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"error": map[string]string{"message": msg, "type": "no_available_account"},
	})
}

// offPeakBridge 单账号闲时桥接状态机：
// WAIT(排队，保活) → READY(带票调用上游) → EXPIRED/回收(重新取票，≤maxRetries)
// → DONE(流结束，settle) / ABORT(客户端断开，settle)。
// 返回 outcomeWritten 表示响应已按协议写回客户端（成功或终态错误）。
func (z *ZCodeAPI) offPeakBridge(w http.ResponseWriter, r *http.Request, a *Account,
	tickets offPeakTickets, opts offPeakRunOpts) relayOutcome {

	ctx := r.Context()
	pollInterval := time.Duration(opts.pollMS) * time.Millisecond
	if pollInterval < 500*time.Millisecond {
		pollInterval = 500 * time.Millisecond
	}
	keepalive := time.Duration(opts.keepaliveMS) * time.Millisecond
	if keepalive < 3*time.Second {
		keepalive = 3 * time.Second
	}
	maxRetries := opts.maxRetries
	if maxRetries <= 0 {
		maxRetries = offPeakMaxRetries
	}
	var hardDeadline time.Time
	if opts.maxWaitSec > 0 {
		hardDeadline = time.Now().Add(time.Duration(opts.maxWaitSec) * time.Second)
	}

	if opts.clientStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flushWriter(w)
	}

	settled := map[string]bool{}
	settleOnce := func(ticketID string) {
		if ticketID == "" || settled[ticketID] {
			return
		}
		settled[ticketID] = true
		go tickets.settle(a, ticketID)
	}
	fail := func(status int, msg string) relayOutcome {
		a.LastError = truncate(msg, 180)
		log.Printf("[async] account %s bridge failed: %s", a.DisplayNameOrEmail(), msg)
		if opts.clientStream {
			writeSSEErrorEvent(w, msg)
		} else {
			writeJSON(w, status, map[string]interface{}{
				"error": map[string]string{"message": msg, "type": "async_error"},
			})
		}
		return outcomeWritten
	}
	retake := func(attempt int, why string) (*offTicket, bool) {
		if attempt >= maxRetries {
			fail(http.StatusServiceUnavailable, why+"且重试次数用尽")
			return nil, false
		}
		if !hardDeadline.IsZero() && time.Now().After(hardDeadline) {
			fail(http.StatusServiceUnavailable, why+"；闲时排队总时长已超限")
			return nil, false
		}
		next, err := tickets.take(a, uuid.NewString())
		if err != nil {
			fail(http.StatusBadGateway, why+"；重新取票失败: "+err.Error())
			return nil, false
		}
		settleOnce(next.ID)
		log.Printf("[async] account %s retake #%d: %s… (%s)", a.DisplayNameOrEmail(), attempt+1, safePrefixLog(next.ID, 8), next.State)
		return next, true
	}

	ticket, err := tickets.take(a, uuid.NewString())
	if err != nil {
		return fail(http.StatusBadGateway, "取票失败: "+err.Error())
	}
	settleOnce(ticket.ID)
	log.Printf("[async] account %s took ticket %s… state=%s pos=%d", a.DisplayNameOrEmail(), safePrefixLog(ticket.ID, 8), ticket.State, ticket.Position)

	for attempt := 0; ; attempt++ {
		// WAIT：票未就绪时轮询 + 保活
		for !offPeakStateReady(ticket.State) && !offPeakStateTerminal(ticket.State) {
			if ctx.Err() != nil {
				return outcomeUpstreamError // 对端已断开；票由 settleOnce 兜底
			}
			if !hardDeadline.IsZero() && time.Now().After(hardDeadline) {
				return fail(http.StatusServiceUnavailable, "闲时排队超时，请稍后重试或改用 /v1/messages")
			}
			sleep := pollInterval
			if ticket.NextPollSec > 0 {
				sleep = time.Duration(ticket.NextPollSec) * time.Second
			}
			if !offPeakWait(ctx, sleep, keepalive, func() { fmt.Fprint(w, ": keepalive\n\n"); flushWriter(w) }, opts.clientStream) {
				return outcomeUpstreamError
			}
			st, err := tickets.status(a, ticket.ID)
			if err != nil {
				log.Printf("[async] poll ticket %s…: %v", safePrefixLog(ticket.ID, 8), err)
				continue
			}
			ticket = st
		}

		if offPeakStateExpired(ticket.State) || ticket.State == "settled" {
			next, ok := retake(attempt, fmt.Sprintf("闲时票被回收（%s）", ticket.State))
			if !ok {
				return outcomeWritten
			}
			ticket = next
			continue
		}
		if ticket.ActiveDeadline > 0 && time.Now().Unix() > ticket.ActiveDeadline {
			next, ok := retake(attempt, "闲时票已过使用截止时间")
			if !ok {
				return outcomeWritten
			}
			ticket = next
			continue
		}
		return z.offPeakForward(w, r, a, ticket, opts)
	}
}

// offPeakWait 等待 min(sleep) 或保活周期；emitKeepalive 在保活到期时被调用。
// 返回 false 表示客户端已断开。
func offPeakWait(ctx context.Context, sleep, keepalive time.Duration, emitKeepalive func(), stream bool) bool {
	deadline := time.Now().Add(sleep)
	nextKeepalive := time.Now().Add(keepalive)
	if !stream {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		now := time.Now()
		if !now.Before(deadline) {
			return true
		}
		wait := deadline.Sub(now)
		if keepaliveDue := nextKeepalive.Sub(now); keepaliveDue < wait {
			wait = keepaliveDue
		}
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			if !time.Now().Before(nextKeepalive) && time.Now().Before(deadline) {
				emitKeepalive()
				nextKeepalive = time.Now().Add(keepalive)
			}
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}
}

// offPeakForward READY → 带票调用上游闲时消息端点并透传响应（协议两侧同为 Anthropic）
func (z *ZCodeAPI) offPeakForward(w http.ResponseWriter, r *http.Request, a *Account, ticket *offTicket, opts offPeakRunOpts) relayOutcome {
	start := time.Now()
	var reqBody struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(opts.payload, &reqBody)

	req, err := http.NewRequestWithContext(r.Context(), "POST", offPeakMessagesURL, bytes.NewReader(opts.payload))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error": map[string]string{"message": "上游请求构造失败", "type": "async_error"},
		})
		return outcomeWritten
	}
	id := NewClientIdentity(z.appVersion, a.DeviceMid)
	for k, v := range ZaiClientHeaders(id) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+a.ZCodeJWT)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersionH)
	req.Header.Set("X-ZCode-Agent", "glm")
	req.Header.Set("X-Off-Peak-Ticket-ID", ticket.ID)
	if reqBody.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	client := ClientForURL(z.egress.ProxyURLForAccount(a), offPeakMessagesURL, 0)
	resp, err := client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return outcomeUpstreamError
		}
		z.pool.MarkCooling(a, "闲时通道连接失败: "+truncate(err.Error(), 160), 60)
		return outcomeNextAccount
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		z.pool.MarkInvalid(a, fmt.Sprintf("闲时通道鉴权失败 HTTP %d", resp.StatusCode))
		return outcomeNextAccount
	case resp.StatusCode == 429:
		z.pool.MarkCooling(a, "闲时通道限流 429", 30)
		return outcomeNextAccount
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		z.pool.MarkFailed(a, fmt.Sprintf("闲时通道上游错误 HTTP %d", resp.StatusCode))
		z.recordUsage(a, r, opts.payload, resp.StatusCode, start, 0, nil, opts.clientStream)
		msg := "闲时通道上游错误 HTTP " + strconv.Itoa(resp.StatusCode) + ": " + truncate(string(body), 300)
		if opts.clientStream {
			writeSSEErrorEvent(w, msg)
		} else {
			writeJSON(w, resp.StatusCode, map[string]interface{}{
				"error": map[string]string{"message": msg, "type": "api_error"},
			})
		}
		return outcomeWritten
	}

	z.pool.MarkUsed(a)

	if reqBody.Stream {
		sniff := newUsageSniffReader(resp.Body)
		flushWriter(w)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := sniff.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					z.recordUsage(a, r, opts.payload, resp.StatusCode, start, 0, sniff.usage(), opts.clientStream)
					return outcomeWritten
				}
				flushWriter(w)
			}
			if rerr != nil {
				break
			}
		}
		z.recordUsage(a, r, opts.payload, resp.StatusCode, start, 0, sniff.usage(), opts.clientStream)
		return outcomeWritten
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		msg := "闲时通道响应读取失败: " + truncate(err.Error(), 160)
		if opts.clientStream {
			writeSSEErrorEvent(w, msg)
		} else {
			writeJSON(w, http.StatusBadGateway, map[string]interface{}{
				"error": map[string]string{"message": msg, "type": "api_error"},
			})
		}
		return outcomeWritten
	}
	z.recordUsage(a, r, opts.payload, resp.StatusCode, start, 0, parseAnthropicUsageJSON(body), opts.clientStream)
	w.Header().Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "application/json"))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
	return outcomeWritten
}

// ---- SSE 透传期间的 usage 嗅探 ----

// usageSniffReader 透传字节流的同时嗅探 Anthropic SSE usage 帧（message_start/message_delta）
type usageSniffReader struct {
	r      io.Reader
	acc    StreamUsage
	peeked string
	done   bool
}

func newUsageSniffReader(r io.Reader) *usageSniffReader { return &usageSniffReader{r: r} }

func (u *usageSniffReader) usage() *StreamUsage {
	u.flushPeek()
	if u.acc.InputTokens == 0 && u.acc.OutputTokens == 0 {
		return nil
	}
	cp := u.acc
	return &cp
}

func (u *usageSniffReader) flushPeek() {
	for {
		idx := strings.IndexByte(u.peeked, '\n')
		if idx < 0 {
			return
		}
		line := strings.TrimRight(u.peeked[:idx], "\r")
		u.peeked = u.peeked[idx+1:]
		u.sniffLine(line)
	}
}

func (u *usageSniffReader) sniffLine(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(line[5:])
	if data == "" || data == "[DONE]" {
		return
	}
	var v struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if json.Unmarshal([]byte(data), &v) != nil {
		return
	}
	switch v.Type {
	case "message_start":
		if v.Message.Usage.InputTokens > u.acc.InputTokens {
			u.acc.InputTokens = v.Message.Usage.InputTokens
		}
	case "message_delta":
		if v.Usage.OutputTokens > 0 {
			u.acc.OutputTokens = v.Usage.OutputTokens
		}
		if v.Usage.InputTokens > u.acc.InputTokens {
			u.acc.InputTokens = v.Usage.InputTokens
		}
		if v.Delta.StopReason != "" {
			u.acc.StopReason = v.Delta.StopReason
		}
	}
}

func (u *usageSniffReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if n > 0 && !u.done {
		u.peeked += string(p[:n])
		if len(u.peeked) > 1<<20 {
			// 嗅探缓冲超限：放弃解析，避免内存膨胀
			u.done = true
			u.peeked = ""
		} else {
			u.flushPeek()
		}
	}
	return n, err
}

// writeSSEErrorEvent 以 Anthropic 风格 SSE error 帧收尾（不得伪装成功）
func writeSSEErrorEvent(w http.ResponseWriter, msg string) {
	enc, _ := json.Marshal(msg)
	fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%s}}\n\n", enc)
	flushWriter(w)
}

func flushWriter(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}
