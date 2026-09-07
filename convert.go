package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---- 协议转换：Anthropic Messages ↔ OpenAI Chat/Responses ----
// 移植 zcode2api gateway.py 的 _openai_to_anthropic / _responses_to_anthropic /
// _openai_sse / _responses_sse / _collect_anthropic。

// StreamUsage 流式嗅探到的用量
type StreamUsage struct {
	InputTokens  int
	OutputTokens int
	StopReason   string
	ToolCalls    []map[string]interface{}
	StreamError  string // 上游 SSE error 事件（overloaded_error 等）
}

// sseEvent 一个完整的 SSE 事件
type sseEvent struct {
	Event string
	Data  map[string]interface{}
}

// maxSSEBuffer 单个流解析缓冲上限（16MB）
const maxSSEBuffer = 16 << 20

// sseParser 增量 SSE 帧解析器（处理跨 chunk 断帧，兼容 LF/CRLF）
type sseParser struct {
	buf strings.Builder
}

func (p *sseParser) feed(chunk []byte, fn func(sseEvent)) {
	p.buf.Write(chunk)
	// 缓冲上限：上游持续不发空行分隔时防止无界增长（OOM 面）
	if p.buf.Len() > maxSSEBuffer {
		p.buf.Reset()
	}
	p.drain(fn)
}

func (p *sseParser) drain(fn func(sseEvent)) {
	for {
		s := p.buf.String()
		idxLF := strings.Index(s, "\n\n")
		idxCRLF := strings.Index(s, "\r\n\r\n")
		idx, width := -1, 0
		switch {
		case idxLF >= 0 && (idxCRLF < 0 || idxLF <= idxCRLF):
			idx, width = idxLF, 2
		case idxCRLF >= 0:
			idx, width = idxCRLF, 4
		}
		if idx < 0 {
			return
		}
		block := s[:idx]
		p.buf.Reset()
		p.buf.WriteString(s[idx+width:])
		if ev, ok := parseSSEBlock(block); ok {
			fn(ev)
		}
	}
}

func (p *sseParser) flush(fn func(sseEvent)) {
	if ev, ok := parseSSEBlock(p.buf.String()); ok {
		fn(ev)
	}
	p.buf.Reset()
}

func parseSSEBlock(block string) (sseEvent, bool) {
	var ev sseEvent
	var dataLines []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "event:") {
			ev.Event = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(line[5:]))
		}
	}
	dataStr := strings.Join(dataLines, "\n")
	if dataStr == "" || dataStr == "[DONE]" {
		return ev, false
	}
	if err := json.Unmarshal([]byte(dataStr), &ev.Data); err != nil {
		return ev, false
	}
	return ev, ev.Event != "" || ev.Data != nil
}

// parseAnthropicUsageJSON 非流式 Anthropic 响应提取 usage
func parseAnthropicUsageJSON(body []byte) *StreamUsage {
	var v struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	return &StreamUsage{InputTokens: v.Usage.InputTokens, OutputTokens: v.Usage.OutputTokens, StopReason: v.StopReason}
}

// applyEventToUsage 从单个事件累积 usage / stop_reason / tool_calls
func applyEventToUsage(ev sseEvent, usage *StreamUsage, activeTool *map[string]interface{}, textParts, thinkParts *[]string) {
	switch ev.Event {
	case "message_start":
		if msg, ok := ev.Data["message"].(map[string]interface{}); ok {
			if u, ok := msg["usage"].(map[string]interface{}); ok {
				if n := toInt(u["input_tokens"]); n > usage.InputTokens {
					usage.InputTokens = n
				}
				if n := toInt(u["output_tokens"]); n > usage.OutputTokens {
					usage.OutputTokens = n
				}
			}
		}
	case "message_delta":
		if u, ok := ev.Data["usage"].(map[string]interface{}); ok {
			if v, ok := u["output_tokens"]; ok {
				usage.OutputTokens = toInt(v)
			}
			// api.z.ai 通道把最终 input_tokens 放在 message_delta（message_start 为 0）
			if v, ok := u["input_tokens"]; ok {
				if n := toInt(v); n > usage.InputTokens {
					usage.InputTokens = n
				}
			}
		}
		if d, ok := ev.Data["delta"].(map[string]interface{}); ok {
			if sr, ok := d["stop_reason"].(string); ok && sr != "" {
				usage.StopReason = sr
			}
		}
	case "content_block_start":
		if block, ok := ev.Data["content_block"].(map[string]interface{}); ok {
			if block["type"] == "tool_use" {
				tool := map[string]interface{}{
					"id":    block["id"],
					"name":  block["name"],
					"_json": "",
				}
				if input, ok := block["input"].(map[string]interface{}); ok {
					tool["input"] = input
				} else {
					tool["input"] = map[string]interface{}{}
				}
				*activeTool = tool
				usage.ToolCalls = append(usage.ToolCalls, tool)
			}
		}
	case "content_block_delta":
		delta, _ := ev.Data["delta"].(map[string]interface{})
		if delta == nil {
			return
		}
		switch delta["type"] {
		case "text_delta":
			if t, ok := delta["text"].(string); ok {
				*textParts = append(*textParts, t)
			}
		case "thinking_delta":
			if t, ok := delta["thinking"].(string); ok {
				*thinkParts = append(*thinkParts, t)
			}
		case "input_json_delta":
			if *activeTool != nil {
				if pj, ok := delta["partial_json"].(string); ok {
					(*activeTool)["_json"] = ((*activeTool)["_json"]).(string) + pj
				}
			}
		}
	case "content_block_stop":
		*activeTool = nil
	case "error":
		// 上游流内错误事件（overloaded_error / 风控中途拦截等），不得被吞掉
		msg := ""
		if e, ok := ev.Data["error"].(map[string]interface{}); ok {
			msg, _ = e["message"].(string)
			if msg == "" {
				msg, _ = e["type"].(string)
			}
		}
		if msg == "" {
			msg = "upstream stream error"
		}
		usage.StreamError = msg
	}
}

// finalizeToolCalls 把累积的 partial_json 解析为 input
func finalizeToolCalls(usage *StreamUsage) {
	for _, call := range usage.ToolCalls {
		raw, _ := call["_json"].(string)
		delete(call, "_json")
		if raw != "" {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				call["input"] = parsed
			}
		}
		if _, ok := call["input"]; !ok {
			call["input"] = map[string]interface{}{}
		}
	}
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

// ---- 非流式响应写回 ----

// writeProtocolResponse 按客户端协议写回非流式响应
func writeProtocolResponse(w http.ResponseWriter, proto protocol, status int, contentType string,
	body []byte, usage *StreamUsage, clientModel string) {

	if proto == protocolAnthropic {
		w.Header().Set("Content-Type", firstNonEmpty(contentType, "application/json"))
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	// OpenAI / Responses：从 Anthropic JSON 提取文本/思考/工具调用
	var resp struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
			ID       string `json:"id"`
			Name     string `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal(body, &resp)
	var text, thinking string
	var toolCalls []map[string]interface{}
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			text += c.Text
		case "thinking":
			thinking += c.Thinking
		case "tool_use":
			var input map[string]interface{}
			json.Unmarshal(c.Input, &input)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id": c.ID, "name": c.Name, "input": input,
			})
		}
	}
	u := &StreamUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		StopReason:   resp.StopReason,
		ToolCalls:    toolCalls,
	}
	if usage != nil && u.InputTokens == 0 {
		u = usage
	}
	if proto == protocolOpenAI {
		writeJSON(w, status, openaiResponse(clientModel, text, thinking, u))
	} else {
		writeJSON(w, status, responsesResponse(clientModel, newResponseID(), text, thinking, u))
	}
}

// ---- 流式响应写回 ----

// streamProtocolResponse 流式透传/转换 + usage 嗅探 + 用量落库
func streamProtocolResponse(w http.ResponseWriter, rc *relayCtx, resp *http.Response,
	a *Account, r *http.Request, payload []byte, z *ZCodeAPI, start time.Time) {

	proto := rc.proto
	clientStream := rc.clientStream
	clientModel := rc.clientModel
	includeUsage := rc.includeUsage
	defer resp.Body.Close()
	flusher, _ := w.(http.Flusher)

	switch {
	case proto == protocolAnthropic:
		// 原生透传 + 嗅探
		w.Header().Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "text/event-stream"))
		w.Header().Set("Cache-Control", "no-cache")
		for _, k := range []string{"x-request-id", "request-id"} {
			if v := resp.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		var usage StreamUsage
		var activeTool map[string]interface{}
		var texts, thinks []string
		parser := &sseParser{}
		ttft := 0
		buf := make([]byte, 32*1024)
		var readErr error
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if ttft == 0 {
					ttft = int(time.Since(start).Milliseconds())
				}
				w.Write(buf[:n])
				if flusher != nil {
					flusher.Flush()
				}
				parser.feed(buf[:n], func(ev sseEvent) {
					applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks)
				})
			}
			if err != nil {
				readErr = err
				break
			}
		}
		parser.flush(func(ev sseEvent) { applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks) })
		finalizeToolCalls(&usage)
		// 透传路径错误事件已原样转发给客户端；此处仅修正用量记录语义并告警
		recStatus := resp.StatusCode
		if usage.StreamError != "" {
			recStatus = 502
			log.Printf("[relay] upstream stream error event (passthrough): %s", usage.StreamError)
		} else if readErr != nil && readErr != io.EOF {
			recStatus = 502
			log.Printf("[relay] upstream stream interrupted (passthrough): %v", readErr)
		}
		z.recordUsage(a, r, payload, recStatus, start, ttft, &usage, rc.clientStream)

	case proto == protocolOpenAI && clientStream:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		z.streamOpenAI(w, flusher, resp, clientModel, includeUsage, a, r, payload, start)

	case proto == protocolResponses && clientStream:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		z.streamResponses(w, flusher, resp, clientModel, a, r, payload, start)

	default:
		// 客户端要非流式，但上游是流式：聚合后写单个 JSON
		var usage StreamUsage
		var activeTool map[string]interface{}
		var texts, thinks []string
		parser := &sseParser{}
		all, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		parser.feed(all, func(ev sseEvent) { applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks) })
		parser.flush(func(ev sseEvent) { applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks) })
		finalizeToolCalls(&usage)
		// 上游流内错误或中途断流：不得伪装成成功空响应
		if usage.StreamError != "" || (readErr != nil && readErr != io.EOF) {
			msg := usage.StreamError
			if msg == "" {
				msg = fmt.Sprintf("upstream stream interrupted: %v", readErr)
			}
			z.recordUsage(a, r, payload, 502, start, 0, &usage, rc.clientStream)
			writeJSON(w, http.StatusBadGateway, map[string]interface{}{
				"error": map[string]string{"message": msg, "type": "upstream_error"},
			})
			return
		}
		text := strings.Join(texts, "")
		thinking := strings.Join(thinks, "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		if proto == protocolOpenAI {
			json.NewEncoder(w).Encode(openaiResponse(clientModel, text, thinking, &usage))
		} else {
			json.NewEncoder(w).Encode(responsesResponse(clientModel, newResponseID(), text, thinking, &usage))
		}
		z.recordUsage(a, r, payload, resp.StatusCode, start, 0, &usage, rc.clientStream)
	}
}

// ---- Anthropic SSE → OpenAI chat.completion.chunk ----

func (z *ZCodeAPI) streamOpenAI(w http.ResponseWriter, flusher http.Flusher, resp *http.Response,
	model string, includeUsage bool, a *Account, r *http.Request, payload []byte, start time.Time) {

	now := time.Now().Unix()
	cid := "chatcmpl-" + randomHex(12)
	var usage StreamUsage
	var activeTool map[string]interface{}
	var texts, thinks []string
	first := true
	toolIndices := map[int]int{}
	nextToolIndex := 0
	ttft := 0

	writeChunk := func(delta map[string]interface{}, finish interface{}, chunkUsage interface{}) {
		payload := map[string]interface{}{
			"id": cid, "object": "chat.completion.chunk", "created": now, "model": model,
			"choices": []map[string]interface{}{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if includeUsage {
			payload["usage"] = chunkUsage
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	parser := &sseParser{}
	handle := func(ev sseEvent) {
		if ttft == 0 {
			ttft = int(time.Since(start).Milliseconds())
		}
		applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks)
		switch ev.Event {
		case "content_block_start":
			block, _ := ev.Data["content_block"].(map[string]interface{})
			if block != nil && block["type"] == "tool_use" {
				srcIdx := toInt(ev.Data["index"])
				toolIdx := nextToolIndex
				nextToolIndex++
				toolIndices[srcIdx] = toolIdx
				if first {
					first = false
					writeChunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, nil)
				}
				id, _ := block["id"].(string)
				if id == "" {
					id = "call_" + randomHex(8)
				}
				name, _ := block["name"].(string)
				writeChunk(map[string]interface{}{
					"tool_calls": []map[string]interface{}{{
						"index": toolIdx, "id": id, "type": "function",
						"function": map[string]interface{}{"name": name, "arguments": ""},
					}},
				}, nil, nil)
			}
		case "content_block_delta":
			delta, _ := ev.Data["delta"].(map[string]interface{})
			if delta == nil {
				return
			}
			switch delta["type"] {
			case "text_delta":
				if first {
					first = false
					writeChunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, nil)
				}
				t, _ := delta["text"].(string)
				writeChunk(map[string]interface{}{"content": t}, nil, nil)
			case "thinking_delta":
				if first {
					first = false
					writeChunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, nil)
				}
				t, _ := delta["thinking"].(string)
				writeChunk(map[string]interface{}{"reasoning_content": t}, nil, nil)
			case "input_json_delta":
				srcIdx := toInt(ev.Data["index"])
				toolIdx := toolIndices[srcIdx]
				pj, _ := delta["partial_json"].(string)
				writeChunk(map[string]interface{}{
					"tool_calls": []map[string]interface{}{{
						"index": toolIdx,
						"function": map[string]interface{}{"arguments": pj},
					}},
				}, nil, nil)
			}
		}
	}

	buf := make([]byte, 32*1024)
	var readErr error
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			parser.feed(buf[:n], handle)
		}
		if err != nil {
			readErr = err
			break
		}
	}
	parser.flush(handle)
	finalizeToolCalls(&usage)

	// 上游流内错误或中途断流：发 OpenAI 错误 chunk 而非伪装成功
	if usage.StreamError != "" || (readErr != nil && readErr != io.EOF) {
		msg := usage.StreamError
		if msg == "" {
			msg = fmt.Sprintf("upstream stream interrupted: %v", readErr)
		}
		ep, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{"message": msg, "type": "api_error", "code": "stream_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", ep)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		z.recordUsage(a, r, payload, 502, start, ttft, &usage, true)
		return
	}

	if first {
		writeChunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, nil)
	}
	writeChunk(map[string]interface{}{}, openaiFinish(usage.StopReason), nil)
	if includeUsage {
		finalUsage := map[string]interface{}{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.InputTokens + usage.OutputTokens,
		}
		p, _ := json.Marshal(map[string]interface{}{
			"id": cid, "object": "chat.completion.chunk", "created": now, "model": model,
			"choices": []interface{}{}, "usage": finalUsage,
		})
		fmt.Fprintf(w, "data: %s\n\n", p)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	z.recordUsage(a, r, payload, resp.StatusCode, start, ttft, &usage, true)
}

// ---- Anthropic SSE → OpenAI Responses SSE ----

func (z *ZCodeAPI) streamResponses(w http.ResponseWriter, flusher http.Flusher, resp *http.Response,
	model string, a *Account, r *http.Request, payload []byte, start time.Time) {

	responseID := newResponseID()
	createdAt := time.Now().Unix()
	var usage StreamUsage
	var activeTool map[string]interface{}
	var texts, thinks []string
	sequence := 0
	nextOutputIndex := 0
	ttft := 0

	type blockState struct {
		kind        string // text | thinking | tool
		itemID      string
		callID      string
		name        string
		jsonBuf     string
		outputIndex int
		texts       []string
	}
	blocks := map[int]*blockState{}

	writeEvent := func(name string, evPayload map[string]interface{}) {
		evPayload["sequence_number"] = sequence
		sequence++
		if _, ok := evPayload["type"]; !ok {
			evPayload["type"] = name
		}
		b, _ := json.Marshal(evPayload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	openMessageEvents := func(idx int) {
		blk := blocks[idx]
		blk.itemID = "msg_" + randomHex(8)
		writeEvent("response.output_item.added", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item": map[string]interface{}{
				"id": blk.itemID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []interface{}{},
			},
		})
		writeEvent("response.content_part.added", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
		})
	}

	closeMessageEvents := func(idx int) {
		blk, ok := blocks[idx]
		if !ok || blk.itemID == "" || blk.kind != "text" {
			return
		}
		text := strings.Join(blk.texts, "")
		texts = append(texts, blk.texts...)
		part := map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}}
		writeEvent("response.output_text.done", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "content_index": 0, "text": text,
		})
		writeEvent("response.content_part.done", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "content_index": 0, "part": part,
		})
		writeEvent("response.output_item.done", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item": map[string]interface{}{
				"id": blk.itemID, "type": "message", "status": "completed",
				"role": "assistant", "content": []interface{}{part},
			},
		})
		delete(blocks, idx)
	}

	openToolEvents := func(idx int, block map[string]interface{}) {
		blk := blocks[idx]
		callID, _ := block["id"].(string)
		if callID == "" {
			callID = "call_" + randomHex(8)
		}
		blk.itemID = "fc_" + randomHex(8)
		blk.callID = callID
		blk.name, _ = block["name"].(string)
		if blk.name == "" {
			blk.name = "tool"
		}
		blk.outputIndex = nextOutputIndex
		nextOutputIndex++
		writeEvent("response.output_item.added", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item": map[string]interface{}{
				"id": blk.itemID, "type": "function_call", "status": "in_progress",
				"call_id": callID, "name": blk.name, "arguments": "",
			},
		})
	}

	closeToolEvents := func(idx int) {
		blk, ok := blocks[idx]
		if !ok || blk.kind != "tool" {
			return
		}
		arguments := blk.jsonBuf
		if arguments == "" {
			arguments = "{}"
		}
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(arguments), &parsed) != nil {
			parsed = map[string]interface{}{}
		}
		usage.ToolCalls = append(usage.ToolCalls, map[string]interface{}{
			"id": blk.callID, "name": blk.name, "input": parsed,
		})
		writeEvent("response.function_call_arguments.done", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "arguments": arguments,
		})
		writeEvent("response.output_item.done", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item": map[string]interface{}{
				"id": blk.itemID, "type": "function_call", "status": "completed",
				"call_id": blk.callID, "name": blk.name, "arguments": arguments,
			},
		})
		delete(blocks, idx)
	}

	writeEvent("response.created", map[string]interface{}{
		"response": map[string]interface{}{
			"id": responseID, "object": "response", "created_at": createdAt,
			"status": "in_progress", "model": model, "output": []interface{}{},
		},
	})

	parser := &sseParser{}
	handle := func(ev sseEvent) {
		if ttft == 0 {
			ttft = int(time.Since(start).Milliseconds())
		}
		switch ev.Event {
		case "message_start", "message_delta":
			applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks)
			return
		}
		idx := toInt(ev.Data["index"])
		switch ev.Event {
		case "content_block_start":
			block, _ := ev.Data["content_block"].(map[string]interface{})
			kind, _ := block["type"].(string)
			if kind == "tool_use" {
				for i := range blocks {
					if blocks[i].kind == "text" {
						closeMessageEvents(i)
					}
				}
				blocks[idx] = &blockState{kind: "tool"}
				openToolEvents(idx, block)
			} else if kind == "text" {
				blocks[idx] = &blockState{kind: "text"}
			} else {
				blocks[idx] = &blockState{kind: kind}
			}
		case "content_block_stop":
			blk, ok := blocks[idx]
			if !ok {
				return
			}
			if blk.kind == "tool" {
				closeToolEvents(idx)
			} else if blk.kind == "text" {
				closeMessageEvents(idx)
			}
		case "content_block_delta":
			delta, _ := ev.Data["delta"].(map[string]interface{})
			if delta == nil {
				return
			}
			blk, ok := blocks[idx]
			if !ok {
				blk = &blockState{kind: "text"}
				blocks[idx] = blk
			}
			switch delta["type"] {
			case "thinking_delta":
				if t, ok := delta["thinking"].(string); ok {
					thinks = append(thinks, t)
				}
			case "input_json_delta":
				if blk.kind == "tool" {
					pj, _ := delta["partial_json"].(string)
					blk.jsonBuf += pj
					writeEvent("response.function_call_arguments.delta", map[string]interface{}{
						"item_id": blk.itemID, "output_index": blk.outputIndex, "delta": pj,
					})
				}
			case "text_delta":
				t, _ := delta["text"].(string)
				if blk.kind == "" {
					blk.kind = "text"
				}
				if blk.kind != "text" {
					return
				}
				if blk.itemID == "" {
					blk.outputIndex = nextOutputIndex
					nextOutputIndex++
					openMessageEvents(idx)
				}
				blk.texts = append(blk.texts, t)
				writeEvent("response.output_text.delta", map[string]interface{}{
					"item_id": blk.itemID, "output_index": blk.outputIndex, "content_index": 0, "delta": t,
				})
			}
		}
	}

	buf := make([]byte, 32*1024)
	var readErr error
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			parser.feed(buf[:n], handle)
		}
		if err != nil {
			readErr = err
			break
		}
	}
	parser.flush(handle)

	// 关闭未完成的块（上游异常中断兜底）
	for idx := range blocks {
		blk := blocks[idx]
		if blk.kind == "tool" {
			closeToolEvents(idx)
		} else if blk.kind == "text" && blk.itemID != "" {
			closeMessageEvents(idx)
		}
	}

	// 上游流内错误或中途断流：发 response.failed 而非伪装 completed
	if usage.StreamError != "" || (readErr != nil && readErr != io.EOF) {
		msg := usage.StreamError
		if msg == "" {
			msg = fmt.Sprintf("upstream stream interrupted: %v", readErr)
		}
		writeEvent("response.failed", map[string]interface{}{
			"response": map[string]interface{}{
				"id": responseID, "object": "response", "status": "failed", "model": model,
				"error": map[string]string{"message": msg, "code": "stream_error"},
			},
		})
		if flusher != nil {
			flusher.Flush()
		}
		finalizeToolCalls(&usage)
		z.recordUsage(a, r, payload, 502, start, ttft, &usage, true)
		return
	}

	fullText := strings.Join(texts, "")
	fullThinking := strings.Join(thinks, "")
	// 上游无任何输出时补空 message 项保持事件序列完整
	if len(usage.ToolCalls) == 0 && fullThinking == "" && fullText == "" {
		emptyIdx := 0
		blocks[emptyIdx] = &blockState{kind: "text", outputIndex: 0}
		openMessageEvents(emptyIdx)
		closeMessageEvents(emptyIdx)
	}
	writeEvent("response.completed", map[string]interface{}{
		"response": responsesResponse(model, responseID, fullText, fullThinking, &usage),
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	finalizeToolCalls(&usage)
	z.recordUsage(a, r, payload, resp.StatusCode, start, ttft, &usage, true)
}

// ---- OpenAI 响应构造 ----

func openaiFinish(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	return "stop"
}

func openaiResponse(model, text, thinking string, usage *StreamUsage) map[string]interface{} {
	message := map[string]interface{}{"role": "assistant", "content": text}
	if thinking != "" {
		message["reasoning_content"] = thinking
	}
	if usage != nil && len(usage.ToolCalls) > 0 {
		if text == "" {
			message["content"] = nil
		}
		calls := make([]map[string]interface{}, 0, len(usage.ToolCalls))
		for _, c := range usage.ToolCalls {
			id, _ := c["id"].(string)
			if id == "" {
				id = "call_" + randomHex(8)
			}
			name, _ := c["name"].(string)
			args, _ := json.Marshal(c["input"])
			calls = append(calls, map[string]interface{}{
				"id": id, "type": "function",
				"function": map[string]interface{}{"name": name, "arguments": string(args)},
			})
		}
		message["tool_calls"] = calls
	}
	in, out := 0, 0
	stop := ""
	if usage != nil {
		in, out, stop = usage.InputTokens, usage.OutputTokens, usage.StopReason
	}
	return map[string]interface{}{
		"id":      "chatcmpl-" + randomHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index": 0, "message": message, "finish_reason": openaiFinish(stop),
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		},
	}
}

func newResponseID() string { return "resp_" + randomHex(12) }

func responsesResponse(model, responseID, text, thinking string, usage *StreamUsage) map[string]interface{} {
	var output []map[string]interface{}
	if thinking != "" {
		output = append(output, map[string]interface{}{
			"id": "rs_" + randomHex(8), "type": "reasoning",
			"summary": []map[string]interface{}{{"type": "summary_text", "text": thinking}},
		})
	}
	if usage != nil {
		for _, c := range usage.ToolCalls {
			id, _ := c["id"].(string)
			if id == "" {
				id = "call_" + randomHex(8)
			}
			name, _ := c["name"].(string)
			args, _ := json.Marshal(c["input"])
			output = append(output, map[string]interface{}{
				"id": "fc_" + randomHex(8), "type": "function_call",
				"call_id": id, "name": name, "arguments": string(args),
			})
		}
	}
	if text != "" || len(output) == 0 {
		output = append(output, map[string]interface{}{
			"id": "msg_" + randomHex(8), "type": "message", "status": "completed",
			"role": "assistant",
			"content": []map[string]interface{}{
				{"type": "output_text", "text": text, "annotations": []interface{}{}},
			},
		})
	}
	in, out := 0, 0
	if usage != nil {
		in, out = usage.InputTokens, usage.OutputTokens
	}
	return map[string]interface{}{
		"id": responseID, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": model,
		"output":      output,
		"output_text": text,
		"usage": map[string]interface{}{
			"input_tokens": in, "output_tokens": out, "total_tokens": in + out,
		},
	}
}

func randomHex(n int) string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")[:n]
}

var _ = log.Printf
