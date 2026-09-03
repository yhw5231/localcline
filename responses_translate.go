// OpenAI Responses API 转换：端点类型为 "responses" 的渠道上游使用
// /responses（Responses API），而下游固定说 chat/completions 协议。
// 这里做双向转换：
//   - 请求：chat/completions 请求体 -> Responses API 请求体（messages -> input、
//     max_tokens -> max_output_tokens、tools/tool_choice/response_format 映射）
//   - 非流式响应：response 对象 -> chat.completion 对象（output_text -> message.content、
//     function_call -> tool_calls、usage.input_tokens -> prompt_tokens）
//   - 流式响应：response.* SSE 事件 -> chat.completion.chunk 帧
//
// 转换失败（如上游返回的是错误体而非 response 对象）时原样透传。
package main

import (
	"encoding/json"
	"strings"
	"time"
)

// ---- 请求转换：chat/completions -> responses ----

// chatMessage chat/completions 请求中的消息（content 为 string 或 parts 数组）。
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// chatToolCall chat/completions 的 tool_calls 条目。
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// respInputItem Responses API 的 input 条目。
type respInputItem struct {
	Type      string `json:"type,omitempty"` // message / function_call / function_call_output
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"` // string 或 parts 数组
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

// respReasoning Responses API 的 reasoning 参数。
type respReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// respTextFormat Responses API 的 text.format（response_format 映射）。
type respTextFormat struct {
	Format json.RawMessage `json:"format,omitempty"`
}

// responsesRequest Responses API 请求体。
type responsesRequest struct {
	Model             string            `json:"model"`
	Input             []respInputItem   `json:"input"`
	Stream            *bool             `json:"stream,omitempty"`
	MaxOutputTokens   *int64            `json:"max_output_tokens,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Reasoning         *respReasoning    `json:"reasoning,omitempty"`
	Text              *respTextFormat   `json:"text,omitempty"`
	User              string            `json:"user,omitempty"`
	Metadata          json.RawMessage   `json:"metadata,omitempty"`
}

// chatToResponsesRequest 把 chat/completions 请求体转换为 Responses API 请求体；
// 不是合法 chat 请求时原样返回。
func chatToResponsesRequest(raw []byte) []byte {
	var req struct {
		Model             string            `json:"model"`
		Messages          []chatMessage     `json:"messages"`
		Stream            *bool             `json:"stream"`
		Temperature       *float64          `json:"temperature"`
		TopP              *float64          `json:"top_p"`
		MaxTokens         *int64            `json:"max_tokens"`
		MaxCompletion     *int64            `json:"max_completion_tokens"`
		Tools             []json.RawMessage `json:"tools"`
		ToolChoice        json.RawMessage   `json:"tool_choice"`
		ParallelToolCalls *bool             `json:"parallel_tool_calls"`
		ReasoningEffort   string            `json:"reasoning_effort"`
		ReasoningSummary  string            `json:"reasoning_summary"`
		User              string            `json:"user"`
		Metadata          json.RawMessage   `json:"metadata"`
		ResponseFormat    json.RawMessage   `json:"response_format"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return raw
	}

	out := responsesRequest{Model: req.Model}
	if req.Stream != nil {
		out.Stream = req.Stream
	}
	if req.MaxCompletion != nil {
		out.MaxOutputTokens = req.MaxCompletion
	} else if req.MaxTokens != nil {
		out.MaxOutputTokens = req.MaxTokens
	}
	out.Temperature, out.TopP = req.Temperature, req.TopP
	out.ParallelToolCalls = req.ParallelToolCalls
	out.User, out.Metadata = req.User, req.Metadata
	if req.ReasoningEffort != "" || req.ReasoningSummary != "" {
		out.Reasoning = &respReasoning{Effort: req.ReasoningEffort, Summary: req.ReasoningSummary}
	}

	// messages -> input：system/developer/user/assistant -> message，
	// assistant.tool_calls -> function_call，role=tool -> function_call_output
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			if m.ToolCallID != "" {
				out.Input = append(out.Input, respInputItem{
					Type:   "function_call_output",
					CallID: m.ToolCallID,
					Output: flattenChatText(m.Content),
				})
			}
			continue
		case "assistant":
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					out.Input = append(out.Input, respInputItem{
						Type:      "function_call",
						CallID:    tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					})
				}
				if text := flattenChatText(m.Content); text != "" {
					out.Input = append(out.Input, respInputItem{Type: "message", Role: "assistant", Content: text})
				}
				continue
			}
		}
		item := respInputItem{Type: "message", Role: m.Role}
		if parts, ok := chatContentParts(m.Content); ok {
			// 仅 user 消息保留多模态 parts（text -> input_text，image_url -> input_image）；
			// 其余角色拍平成纯文本
			if m.Role == "user" {
				var respParts []map[string]any
				hasMedia := false
				for _, p := range parts {
					switch p["type"] {
					case "text":
						respParts = append(respParts, map[string]any{"type": "input_text", "text": p["text"]})
					case "image_url":
						if iu, ok := p["image_url"].(map[string]any); ok {
							respParts = append(respParts, map[string]any{"type": "input_image", "image_url": iu["url"], "detail": iu["detail"]})
						} else if u, ok := p["image_url"].(string); ok {
							respParts = append(respParts, map[string]any{"type": "input_image", "image_url": u})
						}
						hasMedia = true
					}
				}
				if len(respParts) > 0 {
					item.Content = respParts
					if !hasMedia {
						// 纯文本 parts 拍平，兼容对 content: string 更挑剔的上游
						var sb strings.Builder
						for _, p := range respParts {
							if t, ok := p["text"].(string); ok {
								sb.WriteString(t)
							}
						}
						item.Content = sb.String()
					}
				}
			} else {
				var sb strings.Builder
				for _, p := range parts {
					if p["type"] == "text" {
						if t, ok := p["text"].(string); ok {
							sb.WriteString(t)
						}
					}
				}
				item.Content = sb.String()
			}
		} else {
			item.Content = flattenChatText(m.Content)
		}
		out.Input = append(out.Input, item)
	}

	// tools: chat {type:function,function:{...}} -> {type:function,name,...}
	for _, tr := range req.Tools {
		var ct struct {
			Type     string `json:"type"`
			Function *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
				Strict      *bool           `json:"strict"`
			} `json:"function"`
		}
		if json.Unmarshal(tr, &ct) != nil || ct.Type != "function" || ct.Function == nil {
			continue // 非 function 工具（代码解释器等）形态不同，跳过
		}
		rt := map[string]any{"type": "function", "name": ct.Function.Name}
		if ct.Function.Description != "" {
			rt["description"] = ct.Function.Description
		}
		if len(ct.Function.Parameters) > 0 && string(ct.Function.Parameters) != "null" {
			rt["parameters"] = ct.Function.Parameters
		}
		if ct.Function.Strict != nil {
			rt["strict"] = ct.Function.Strict
		}
		if b, err := json.Marshal(rt); err == nil {
			out.Tools = append(out.Tools, b)
		}
	}

	// tool_choice: 字符串透传；对象 {type:function,function:{name}} -> {type:function,name}
	if len(req.ToolChoice) > 0 {
		var tc struct {
			Type     string `json:"type"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(req.ToolChoice, &tc) == nil && tc.Type == "function" && tc.Function != nil {
			out.ToolChoice = mustJSON(map[string]any{"type": "function", "name": tc.Function.Name})
		} else {
			out.ToolChoice = req.ToolChoice
		}
	}

	// response_format -> text.format
	if len(req.ResponseFormat) > 0 {
		var rf struct {
			Type       string `json:"type"`
			JSONSchema *struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
				Strict *bool           `json:"strict"`
			} `json:"json_schema"`
		}
		if json.Unmarshal(req.ResponseFormat, &rf) == nil {
			var format map[string]any
			switch rf.Type {
			case "json_object":
				format = map[string]any{"type": "json_object"}
			case "json_schema":
				if rf.JSONSchema != nil {
					format = map[string]any{"type": "json_schema", "name": rf.JSONSchema.Name, "schema": rf.JSONSchema.Schema}
					if rf.JSONSchema.Strict != nil {
						format["strict"] = rf.JSONSchema.Strict
					}
				}
			}
			if format != nil {
				out.Text = &respTextFormat{Format: mustJSON(format)}
			}
		}
	}

	return mustJSON(out)
}

// flattenChatText 把 chat content（string / parts 数组 / null）拍平为纯文本。
func flattenChatText(raw json.RawMessage) string {
	if s, ok := chatContentParts(raw); ok {
		var sb strings.Builder
		for _, p := range s {
			if t, ok := p["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// chatContentParts 把 content 解析为 parts 数组（仅数组形态返回 ok）。
func chatContentParts(raw json.RawMessage) ([]map[string]any, bool) {
	if len(raw) == 0 || jsonIsNull(raw) {
		return nil, false
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false
	}
	return arr, true
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// ---- 非流式响应转换：response 对象 -> chat.completion ----

// responsesToChatCompletion 把 Responses API 的 response 对象转换为
// chat/completions 响应体；不是 response 对象（如错误体）时返回 (原样, false)。
func responsesToChatCompletion(raw []byte) ([]byte, bool) {
	var resp struct {
		ID     string            `json:"id"`
		Object string            `json:"object"`
		Model  string            `json:"model"`
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
		Usage  *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Output == nil {
		return raw, false
	}

	var content, reasoning strings.Builder
	var toolCalls []map[string]any
	for _, item := range resp.Output {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &head) != nil {
			continue
		}
		switch head.Type {
		case "message":
			var m struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(item, &m) == nil {
				for _, p := range m.Content {
					if p.Type == "output_text" || p.Type == "text" {
						content.WriteString(p.Text)
					}
				}
			}
		case "reasoning":
			var r struct {
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(item, &r) == nil {
				for _, p := range r.Summary {
					reasoning.WriteString(p.Text)
				}
				for _, p := range r.Content {
					reasoning.WriteString(p.Text)
				}
			}
		case "function_call":
			var f struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if json.Unmarshal(item, &f) == nil {
				toolCalls = append(toolCalls, map[string]any{
					"id":   f.CallID,
					"type": "function",
					"function": map[string]any{
						"name":      f.Name,
						"arguments": f.Arguments,
					},
				})
			}
		}
	}

	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	} else if resp.Status == "incomplete" {
		finish = "length"
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":      firstNonEmpty(resp.ID, "chatcmpl-"+randomHex(8)),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   resp.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if resp.Usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      firstNonZero(resp.Usage.TotalTokens, resp.Usage.InputTokens+resp.Usage.OutputTokens),
		}
	}
	return mustJSON(out), true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// ---- 流式响应转换：response.* SSE 事件 -> chat.completion.chunk 帧 ----

// responsesStreamConv 单条上游流式响应的转换器（逐帧喂数据）。
type responsesStreamConv struct {
	id        string
	model     string
	created   int64
	toolCalls int
	toolIdx   map[int64]int // output_index -> chat tool_calls index
	finish    string
	done      bool
}

func newResponsesStreamConv() *responsesStreamConv {
	return &responsesStreamConv{created: time.Now().Unix(), toolIdx: map[int64]int{}}
}

// convertFrame 转换一帧上游 SSE（可能产生多帧 chat chunk），无内容时返回 nil。
func (c *responsesStreamConv) convertFrame(frame []byte) []byte {
	var out []byte
	for _, line := range strings.Split(string(frame), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			out = append(out, c.finalize()...)
			continue
		}
		var ev struct {
			Type        string          `json:"type"`
			Delta       string          `json:"delta"`
			OutputIndex *int64          `json:"output_index"`
			Item        json.RawMessage `json:"item"`
			Response    json.RawMessage `json:"response"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.created", "response.in_progress":
			var r struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			}
			if json.Unmarshal(ev.Response, &r) == nil {
				if r.ID != "" {
					c.id = r.ID
				}
				if r.Model != "" {
					c.model = r.Model
				}
			}
		case "response.output_text.delta":
			if ev.Delta != "" {
				out = append(out, c.chunk(map[string]any{"content": ev.Delta})...)
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			if ev.Delta != "" {
				out = append(out, c.chunk(map[string]any{"reasoning_content": ev.Delta})...)
			}
		case "response.output_item.added":
			var item struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				ItemID string `json:"id"`
			}
			if json.Unmarshal(ev.Item, &item) == nil && item.Type == "function_call" {
				idx := c.toolCalls
				c.toolCalls++
				if ev.OutputIndex != nil {
					c.toolIdx[*ev.OutputIndex] = idx
				}
				out = append(out, c.chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index":    idx,
					"id":       firstNonEmpty(item.CallID, item.ItemID),
					"type":     "function",
					"function": map[string]any{"name": item.Name, "arguments": ""},
				}}})...)
			}
		case "response.function_call_arguments.delta":
			if ev.Delta != "" {
				idx := 0
				if ev.OutputIndex != nil {
					idx = c.toolIdx[*ev.OutputIndex]
				}
				out = append(out, c.chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index":    idx,
					"function": map[string]any{"arguments": ev.Delta},
				}}})...)
			}
		case "response.completed":
			c.finish = "stop"
			if u := c.usageFrom(ev.Response); u != nil {
				out = append(out, c.finalChunk(u)...)
			} else {
				out = append(out, c.finalChunk(nil)...)
			}
			out = append(out, []byte("data: [DONE]\n\n")...)
		case "response.incomplete":
			c.finish = "length"
			out = append(out, c.finalChunk(c.usageFrom(ev.Response))...)
			out = append(out, []byte("data: [DONE]\n\n")...)
		case "response.failed", "error":
			var e struct {
				Response *struct {
					Error json.RawMessage `json:"error"`
				} `json:"response"`
				Error   json.RawMessage `json:"error"`
				Message string          `json:"message"`
				Code    any             `json:"code"`
			}
			_ = json.Unmarshal([]byte(payload), &e)
			errObj := e.Error
			if len(errObj) == 0 && e.Response != nil {
				errObj = e.Response.Error
			}
			if len(errObj) == 0 || string(errObj) == "null" {
				errObj = mustJSON(map[string]any{"message": firstNonEmpty(e.Message, "upstream responses failed"), "code": e.Code})
			}
			out = append(out, []byte("data: "+string(mustJSON(map[string]any{"error": json.RawMessage(errObj)}))+"\n\n")...)
			out = append(out, []byte("data: [DONE]\n\n")...)
		}
		if c.done {
			break
		}
	}
	return out
}

// finalize 输出结束帧（finish_reason + [DONE]），保证只发一次。
// done 为 true 表示流已结束（正常收尾或上游异常断流补发），不再输出。
func (c *responsesStreamConv) finalize() []byte {
	if c.done {
		return nil
	}
	c.done = true
	finish := c.finish
	if finish == "" {
		finish = "stop"
	}
	return c.finalChunk(nil)
}

// finalChunk 输出带 finish_reason 的收尾 chunk（可带 usage）。
func (c *responsesStreamConv) finalChunk(usage map[string]any) []byte {
	chunk := map[string]any{
		"id":      firstNonEmpty(c.id, "chatcmpl-"+randomHex(8)),
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": c.finishOrStop(),
		}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return []byte("data: " + string(mustJSON(chunk)) + "\n\n")
}

func (c *responsesStreamConv) finishOrStop() string {
	if c.finish != "" {
		return c.finish
	}
	return "stop"
}

// chunk 输出一个 content/reasoning/tool_calls 增量 chunk。
func (c *responsesStreamConv) chunk(delta map[string]any) []byte {
	chunk := map[string]any{
		"id":      firstNonEmpty(c.id, "chatcmpl-"+randomHex(8)),
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
	return []byte("data: " + string(mustJSON(chunk)) + "\n\n")
}

// usageFrom 从 response 对象提取 chat 风格 usage。
func (c *responsesStreamConv) usageFrom(raw json.RawMessage) map[string]any {
	var r struct {
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Usage == nil {
		return nil
	}
	if r.Usage.InputTokens == 0 && r.Usage.OutputTokens == 0 {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     r.Usage.InputTokens,
		"completion_tokens": r.Usage.OutputTokens,
		"total_tokens":      firstNonZero(r.Usage.TotalTokens, r.Usage.InputTokens+r.Usage.OutputTokens),
	}
}
