// 端点类型（chat / responses）测试：URL 推导与归一化、chat ↔ responses 请求/响应
// 双向转换（非流式与流式）、以及经 forwardChat 的端到端转发。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ---- endpoint_type 归一化与 URL 推导 ----

func TestNormalizeEndpointType(t *testing.T) {
	for in, want := range map[string]string{
		"": "chat", "chat": "chat", "Chat": "chat", "chat/completions": "chat",
		"responses": "responses", "Responses": "responses",
	} {
		got, err := normalizeEndpointType(in)
		if err != nil || got != want {
			t.Fatalf("normalizeEndpointType(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := normalizeEndpointType("grpc"); err == nil {
		t.Fatal("expected error for unsupported endpoint type")
	}
}

func TestResponsesURLOf(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com/v1":          "https://api.openai.com/v1/responses",
		"https://api.openai.com":             "https://api.openai.com/v1/responses",
		"https://x.example.com/api/v2":       "https://x.example.com/api/v2/responses",
		"https://x.example.com/v1/responses": "https://x.example.com/v1/responses",
		"https://x.example.com/openai":       "https://x.example.com/openai/v1/responses",
		"https://x.example.com/vision":       "https://x.example.com/vision/v1/responses", // v 开头但非版本段
	}
	for in, want := range cases {
		if got := responsesURLOf(in); got != want {
			t.Fatalf("responsesURLOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// 端点类型保存与非法值校验
func TestChannelEndpointTypePersisted(t *testing.T) {
	setupGateway(t)
	mustPutChannel(t, &Channel{Name: "r", BaseURL: "https://api.openai.com/v1",
		EndpointType: "responses", Enabled: true})
	ch := store.Snapshot().Channels[0]
	if ch.EndpointType != "responses" {
		t.Fatalf("endpoint_type lost: %q", ch.EndpointType)
	}
	// 非法值应报错
	if err := store.PutChannel(&Channel{Name: "x", BaseURL: "https://x", EndpointType: "bogus"}); err == nil {
		t.Fatal("expected error for invalid endpoint_type")
	}
}

// responses 渠道：chat 请求体应被转换为 Responses API 请求体并发到 /responses
func TestResponsesChannelRequestTranslation(t *testing.T) {
	setupGateway(t)
	var mu sync.Mutex
	var gotPath string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		_ = json.Unmarshal(b, &gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1","object":"response","model":"gpt-5.1","status":"completed",
			"output":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking..."}]},
				{"type":"message","content":[{"type":"output_text","text":"hello!"}]}
			],
			"usage":{"input_tokens":12,"output_tokens":34,"total_tokens":46}
		}`))
	}))
	defer up.Close()

	mustPutChannel(t, &Channel{Name: "r", BaseURL: up.URL, EndpointType: "responses", Enabled: true,
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true}}})
	if err := store.PutGWKey(&GWKey{Name: "down", Key: "sk-gw-test", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := chatRequest("gpt-5.1")
	req.Header.Set("Authorization", "Bearer sk-gw-test")
	rootHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	path, body := gotPath, gotBody
	mu.Unlock()
	if path != "/v1/responses" {
		t.Fatalf("upstream path = %s, want /v1/responses", path)
	}
	if body["model"] != "gpt-5.1" {
		t.Fatalf("upstream model: %v", body["model"])
	}
	input, _ := body["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("upstream input items: %v", body["input"])
	}
	item, _ := input[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" || item["content"] != "hi" {
		t.Fatalf("input item: %v", item)
	}
	if _, has := body["max_output_tokens"]; has {
		t.Fatalf("max_output_tokens should be omitted when max_tokens absent: %v", body)
	}

	// 响应已转回 chat.completion
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode chat response: %v\nbody: %s", err, rr.Body.String())
	}
	if out.Object != "chat.completion" {
		t.Fatalf("object = %s", out.Object)
	}
	m := out.Choices[0].Message
	if m.Content != "hello!" || m.ReasoningContent != "thinking..." {
		t.Fatalf("message: %+v", m)
	}
	if out.Usage.PromptTokens != 12 || out.Usage.CompletionTokens != 34 {
		t.Fatalf("usage mapping: %+v", out.Usage)
	}
}

// responses 渠道流式：response.output_text.delta 等事件应转换为 chat chunk
func TestResponsesChannelStreamTranslation(t *testing.T) {
	setupGateway(t)
	sse := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_9","model":"gpt-5.1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"he"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"llo"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_9","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}` + "\n\n"
	up := newUpstreamStream(t, sse)

	mustPutChannel(t, &Channel{Name: "r", BaseURL: up.URL, EndpointType: "responses", Enabled: true,
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true}}})
	if err := store.PutGWKey(&GWKey{Name: "down", Key: "sk-gw-test", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5.1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-gw-test")
	rootHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"content":"he"`) || !strings.Contains(body, `"content":"llo"`) {
		t.Fatalf("text deltas missing: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("finish frame missing: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":3`) {
		t.Fatalf("usage mapping missing: %s", body)
	}
}

// responses 渠道非流式的 tool_calls 往返
func TestResponsesToolCallsTranslation(t *testing.T) {
	raw := []byte(`{
		"id":"resp_2","object":"response","model":"gpt-5.1","status":"completed",
		"output":[
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}
		],
		"usage":{"input_tokens":7,"output_tokens":9,"total_tokens":16}
	}`)
	out, ok := responsesToChatCompletion(raw)
	if !ok {
		t.Fatal("expected conversion")
	}
	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tc := parsed.Choices[0].Message.ToolCalls
	if parsed.Choices[0].FinishReason != "tool_calls" || len(tc) != 1 ||
		tc[0].ID != "call_1" || tc[0].Function.Name != "get_weather" {
		t.Fatalf("tool_calls conversion: %+v", parsed)
	}

	// 请求方向：assistant tool_calls -> function_call，tool -> function_call_output
	chatReq := []byte(`{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}
	]}`)
	conv := chatToResponsesRequest(chatReq)
	var rr struct {
		Input []struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Output    string `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(conv, &rr); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	if len(rr.Input) != 3 ||
		rr.Input[1].Type != "function_call" || rr.Input[1].Name != "get_weather" ||
		rr.Input[2].Type != "function_call_output" || rr.Input[2].Output != "sunny" {
		t.Fatalf("request conversion: %+v", rr.Input)
	}
}

// 非 response 对象（错误体）应原样透传
func TestResponsesErrorPassthrough(t *testing.T) {
	raw := []byte(`{"error":{"message":"insufficient quota","type":"insufficient_quota"}}`)
	out, ok := responsesToChatCompletion(raw)
	if ok || string(out) != string(raw) {
		t.Fatalf("error body should pass through unchanged, ok=%v out=%s", ok, out)
	}
}
