package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
)

// ---- OpenAI 兼容端点 ----
// POST /v1/chat/completions 与 POST /v1/responses：
// 请求体转 Anthropic Messages 格式，内部一律流式请求上游，按客户端需求聚合或转 SSE。

// HandleChatCompletions POST /v1/chat/completions
func (z *ZCodeAPI) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, errResp := readJSONBody(r)
	if errResp != nil {
		errResp.Write(w)
		return
	}
	if s, ok := body["stream"]; ok {
		if _, isBool := s.(bool); !isBool {
			writeAPIError(w, http.StatusBadRequest, "stream must be a boolean")
			return
		}
	}
	provider := detectProvider(body, r.Header)
	anth, err := openaiToAnthropic(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	anth["stream"] = true // 内部一律流式，按需聚合
	if err := normalizeBody(anth, z); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateMessagesBody(anth); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	clientStream, _ := body["stream"].(bool)
	clientModel, _ := body["model"].(string)
	if clientModel == "" {
		clientModel = "gpt-4o"
	}
	includeUsage := false
	if so, ok := body["stream_options"].(map[string]interface{}); ok {
		includeUsage, _ = so["include_usage"].(bool)
	}
	rc := &relayCtx{
		body: anth, provider: provider, group: r.Header.Get("x-zcode-group"),
		proto: protocolOpenAI, clientStream: clientStream,
		clientModel: clientModel, includeUsage: includeUsage,
	}
	z.relay(w, r, rc)
}

// HandleResponses POST /v1/responses（Codex / 新版 OpenAI SDK）
func (z *ZCodeAPI) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, errResp := readJSONBody(r)
	if errResp != nil {
		errResp.Write(w)
		return
	}
	if s, ok := body["stream"]; ok {
		if _, isBool := s.(bool); !isBool {
			writeAPIError(w, http.StatusBadRequest, "stream must be a boolean")
			return
		}
	}
	provider := detectProvider(body, r.Header)
	anth, err := responsesToAnthropic(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	anth["stream"] = true
	if err := normalizeBody(anth, z); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateMessagesBody(anth); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	clientStream, _ := body["stream"].(bool)
	clientModel, _ := body["model"].(string)
	if clientModel == "" {
		clientModel = "GLM-5.3"
	}
	rc := &relayCtx{
		body: anth, provider: provider, group: r.Header.Get("x-zcode-group"),
		proto: protocolResponses, clientStream: clientStream, clientModel: clientModel,
	}
	z.relay(w, r, rc)
}

// asIfaceSlice 将 []interface{} 或 []map[string]interface{} 等切片统一为 []interface{}
func asIfaceSlice(v interface{}) []interface{} {
	switch s := v.(type) {
	case []interface{}:
		return s
	case nil:
		return nil
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice {
			return nil
		}
		out := make([]interface{}, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out
	}
}

// ---- 请求体转换：OpenAI Chat → Anthropic Messages ----

func openaiToAnthropic(body map[string]interface{}) (map[string]interface{}, error) {
	model, _ := body["model"].(string)
	if model == "" {
		model = "GLM-5.3"
	}
	var messages []map[string]interface{}
	var systemParts []string

	rawMsgs, _ := body["messages"].([]interface{})
	for _, m := range rawMsgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		if role == "system" || role == "developer" {
			switch c := content.(type) {
			case string:
				systemParts = append(systemParts, c)
			case []interface{}:
				for _, part := range c {
					if pm, ok := part.(map[string]interface{}); ok && pm["type"] == "text" {
						if t, ok := pm["text"].(string); ok {
							systemParts = append(systemParts, t)
						}
					}
				}
			}
			continue
		}
		if role == "tool" {
			toolID, _ := msg["tool_call_id"].(string)
			if toolID == "" {
				continue
			}
			var resultContent interface{}
			switch c := content.(type) {
			case string, []interface{}:
				resultContent = c
			default:
				resultContent = ""
			}
			messages = append(messages, map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{{
					"type": "tool_result", "tool_use_id": toolID, "content": resultContent,
				}},
			})
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}

		var blocks []map[string]interface{}
		switch c := content.(type) {
		case string:
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": c})
		case []interface{}:
			for _, part := range c {
				pm, ok := part.(map[string]interface{})
				if !ok {
					continue
				}
				switch pm["type"] {
				case "text":
					t, _ := pm["text"].(string)
					blocks = append(blocks, map[string]interface{}{"type": "text", "text": t})
				case "image_url":
					iu, _ := pm["image_url"].(map[string]interface{})
					u, _ := iu["url"].(string)
					if strings.HasPrefix(u, "data:") && strings.Contains(u, ";base64,") {
						idx := strings.Index(u, ";base64,")
						mime := u[5:idx]
						if mime == "" {
							mime = "image/png"
						}
						data := u[idx+8:]
						blocks = append(blocks, map[string]interface{}{
							"type": "image",
							"source": map[string]interface{}{
								"type": "base64", "media_type": mime, "data": data,
							},
						})
					}
				}
			}
		}
		if role == "assistant" {
			for _, c := range asIfaceSlice(msg["tool_calls"]) {
				cm, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				fn, _ := cm["function"].(map[string]interface{})
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				var toolInput map[string]interface{}
				switch args := fn["arguments"].(type) {
				case string:
					if json.Unmarshal([]byte(args), &toolInput) != nil {
						toolInput = map[string]interface{}{"_raw_arguments": args}
					}
				case map[string]interface{}:
					toolInput = args
				default:
					toolInput = map[string]interface{}{}
				}
				id, _ := cm["id"].(string)
				if id == "" {
					id = "call_" + randomHex(8)
				}
				blocks = append(blocks, map[string]interface{}{
					"type": "tool_use", "id": id, "name": name, "input": toolInput,
				})
			}
		}
		if len(blocks) > 0 {
			item := map[string]interface{}{"role": role, "content": blocks}
			if role == "assistant" {
				if name, ok := msg["name"].(string); ok && name != "" {
					item["name"] = name
				}
			}
			messages = append(messages, item)
		}
	}

	out := map[string]interface{}{"model": model, "messages": toIfaceSlice(messages)}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if mt, ok := body["max_tokens"]; ok {
		out["max_tokens"] = mt
	} else if mct, ok := body["max_completion_tokens"]; ok {
		out["max_tokens"] = mct
	} else {
		out["max_tokens"] = float64(4096)
	}
	if t, ok := body["temperature"]; ok && t != nil {
		out["temperature"] = t
	}
	if tp, ok := body["top_p"]; ok && tp != nil {
		out["top_p"] = tp
	}
	if stop, ok := body["stop"]; ok && stop != nil {
		switch s := stop.(type) {
		case []interface{}:
			out["stop_sequences"] = s
		case string:
			out["stop_sequences"] = []interface{}{s}
		}
	}

	// tools 转换
	if rawTools, ok := body["tools"].([]interface{}); ok {
		var tools []map[string]interface{}
		for _, t := range rawTools {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			fn := tm
			if tm["type"] == "function" {
				if f, ok := tm["function"].(map[string]interface{}); ok {
					fn = f
				}
			}
			name, _ := fn["name"].(string)
			if name == "" {
				continue
			}
			desc, _ := fn["description"].(string)
			var schema map[string]interface{}
			for _, k := range []string{"parameters", "input_schema", "schema"} {
				if s, ok := fn[k].(map[string]interface{}); ok {
					schema = s
					break
				}
			}
			if schema == nil {
				schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			tools = append(tools, map[string]interface{}{
				"name": name, "description": desc, "input_schema": schema,
			})
		}
		if len(tools) > 0 {
			out["tools"] = toIfaceSlice(tools)
		}
	}

	// tool_choice 转换；Anthropic 无 "none"：连同 tools 一起从上游请求中省略
	switch tc := body["tool_choice"].(type) {
	case string:
		if tc == "none" {
			delete(out, "tools")
		} else if tc == "auto" {
			out["tool_choice"] = map[string]interface{}{"type": "auto"}
		} else if tc == "required" {
			out["tool_choice"] = map[string]interface{}{"type": "any"}
		}
	case map[string]interface{}:
		if fn, ok := tc["function"].(map[string]interface{}); ok {
			if name, _ := fn["name"].(string); name != "" {
				out["tool_choice"] = map[string]interface{}{"type": "tool", "name": name}
			}
		}
	}
	return out, nil
}

// ---- 请求体转换：OpenAI Responses → Anthropic Messages ----

func responsesContentToText(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, p := range c {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			for _, k := range []string{"text", "input_text", "output_text"} {
				if s, ok := pm[k].(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func responsesToAnthropic(body map[string]interface{}) (map[string]interface{}, error) {
	model, _ := body["model"].(string)
	if model == "" {
		model = "GLM-5.3"
	}
	var messages []map[string]interface{}
	var systemParts []string

	if instructions, ok := body["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		systemParts = append(systemParts, strings.TrimSpace(instructions))
	}

	switch input := body["input"].(type) {
	case string:
		messages = append(messages, map[string]interface{}{"role": "user", "content": input})
	case []interface{}:
		for _, item := range input {
			im, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			itemType, _ := im["type"].(string)
			role, _ := im["role"].(string)
			switch {
			case itemType == "message" || role == "user" || role == "assistant" || role == "system" || role == "developer":
				text := responsesContentToText(im["content"])
				if text == "" {
					if s, ok := im["content"].(string); ok {
						text = s
					}
				}
				if role == "system" || role == "developer" {
					if text != "" {
						systemParts = append(systemParts, text)
					}
				} else if (role == "user" || role == "assistant") && text != "" {
					messages = append(messages, map[string]interface{}{"role": role, "content": text})
				}
			case itemType == "function_call_output":
				callID := firstNonEmpty(jsonStr(im, "call_id"), jsonStr(im, "tool_call_id"))
				output := responsesContentToText(im["output"])
				if callID != "" {
					messages = append(messages, map[string]interface{}{
						"role": "tool", "tool_call_id": callID, "content": output,
					})
				}
			case itemType == "function_call":
				name := firstNonEmpty(jsonStr(im, "name"), "tool")
				callID := firstNonEmpty(jsonStr(im, "call_id"), jsonStr(im, "id"), "call_"+randomHex(8))
				arguments := jsonStr(im, "arguments")
				if arguments == "" {
					arguments = "{}"
				}
				messages = append(messages, map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []interface{}{map[string]interface{}{
						"id": callID, "type": "function",
						"function": map[string]interface{}{"name": name, "arguments": arguments},
					}},
				})
			}
		}
	default:
		return nil, errString("input must be a string or array")
	}
	if len(messages) == 0 {
		return nil, errString("input must contain at least one message")
	}

	chatBody := map[string]interface{}{
		"model":    model,
		"messages": toIfaceSlice(messages),
		"stream":   boolOf(body["stream"]),
	}
	if len(systemParts) > 0 {
		sysMsg := map[string]interface{}{"role": "system", "content": strings.Join(systemParts, "\n\n")}
		chatBody["messages"] = append([]interface{}{sysMsg}, chatBody["messages"].([]interface{})...)
	}
	if mot, ok := body["max_output_tokens"]; ok {
		chatBody["max_tokens"] = mot
	} else if mt, ok := body["max_tokens"]; ok {
		chatBody["max_tokens"] = mt
	}
	if t, ok := body["temperature"]; ok && t != nil {
		chatBody["temperature"] = t
	}
	if tp, ok := body["top_p"]; ok && tp != nil {
		chatBody["top_p"] = tp
	}
	if tools, ok := body["tools"]; ok {
		chatBody["tools"] = tools
	}
	if tc, ok := body["tool_choice"]; ok {
		chatBody["tool_choice"] = tc
	}
	if reasoning, ok := body["reasoning"].(map[string]interface{}); ok {
		if effort, ok := reasoning["effort"]; ok && effort != nil {
			chatBody["reasoning_effort"] = effort
		}
	}
	return openaiToAnthropic(chatBody)
}

// ---- 小工具 ----

func toIfaceSlice(in []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func boolOf(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

type plainError string

func (e plainError) Error() string { return string(e) }

func errString(s string) error { return plainError(s) }
