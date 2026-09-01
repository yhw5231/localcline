package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 通用辅助 ----

func callProxy(t *testing.T, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	reloadConfig() // 让 t.Setenv 生效
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 默认注入有效登录 token（LOGIN_REQUIRED 默认 true）
	if req.Header.Get("Authorization") == "" {
		token, err := issueToken("admin")
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

func resetModelsCache() {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	modelsEntry = nil
	modelsPending = make(map[string]chan struct{})
}

func freeModelsBody() []byte {
	return []byte(`{"recommended":[],"free":[{"id":"deepseek/deepseek-v4-flash","name":"DeepSeek V4 Flash","description":"deep desc","tags":[]},{"id":123,"name":"bad-num"},{"id":"","name":"empty"},{"id":"noprefix"}],"clinePass":[],"clineCloud":[]}`)
}

// ---- JSON 改写原语 ----

func TestRewriteDelta(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOut  bool
		wantHave string // 期望输出中包含的子串
		wantSame bool   // 期望输出与输入完全一致
	}{
		{"无 reasoning 直接透传", `{"choices":[{"delta":{"content":"hi"}}]}`, false, "", true},
		{"含 reasoning 改写", `{"choices":[{"delta":{"content":"hi","reasoning":"think"}}]}`, true, `"reasoning_content":"think"`, false},
		{"已是 truthy reasoning_content 不改", `{"choices":[{"delta":{"content":"hi","reasoning":"think","reasoning_content":"exist"}}]}`, false, "", true},
		{"reasoning_content 空串仍改写", `{"choices":[{"delta":{"content":"hi","reasoning":"think","reasoning_content":""}}]}`, true, `"reasoning_content":"think"`, false},
		{"reasoning_content null 仍改写", `{"choices":[{"delta":{"content":"hi","reasoning":"think","reasoning_content":null}}]}`, true, `"reasoning_content":"think"`, false},
		{"reasoning 为 null 不改", `{"choices":[{"delta":{"content":"hi","reasoning":null}}]}`, false, "", true},
		{"无 choices 不改", `{"id":"x","object":"chat.completion.chunk"}`, false, "", true},
		{"多 choice 逐个改写", `{"choices":[{"delta":{"a":1,"reasoning":"r1"}},{"delta":{"reasoning":"r2"}}]}`, true, `"reasoning_content":"r1"`, false},
		{"不可解析原样", `data: {bad json`, false, "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed := rewriteDelta([]byte(c.in))
			if changed != c.wantOut {
				t.Fatalf("changed = %v, want %v", changed, c.wantOut)
			}
			if c.wantSame && string(out) != c.in {
				t.Fatalf("expected byte-identical passthrough, got:\n  in : %s\n  out: %s", c.in, out)
			}
			if c.wantHave != "" && !bytes.Contains(out, []byte(c.wantHave)) {
				t.Fatalf("output missing %q, got: %s", c.wantHave, out)
			}
		})
	}
}

func TestRewriteNonStream(t *testing.T) {
	in := `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi","reasoning":"think"}}]}`
	out, ok := rewriteNonStream([]byte(in))
	if !ok {
		t.Fatal("expected parse success")
	}
	if !strings.Contains(string(out), `"reasoning_content":"think"`) {
		t.Fatalf("missing reasoning_content, got: %s", out)
	}

	// 无 reasoning：解析成功但原字节透传
	in2 := `{"choices":[{"message":{"content":"hi"}}]}`
	out2, ok2 := rewriteNonStream([]byte(in2))
	if !ok2 {
		t.Fatal("expected parse success")
	}
	if string(out2) != in2 {
		t.Fatalf("expected byte-identical, got %s", out2)
	}

	// message.reasoning null：不改
	in3 := `{"choices":[{"message":{"reasoning":null,"content":"hi"}}]}`
	out3, ok3 := rewriteNonStream([]byte(in3))
	if !ok3 {
		t.Fatal("expected parse success")
	}
	if string(out3) != in3 {
		t.Fatalf("expected byte-identical for null reasoning, got %s", out3)
	}

	// 出错体（非 JSON）：原样 + 失败标记
	in4 := `<html>502 Bad Gateway</html>`
	out4, ok4 := rewriteNonStream([]byte(in4))
	if ok4 {
		t.Fatal("expected parse failure")
	}
	if string(out4) != in4 {
		t.Fatalf("expected passthrough of error body, got %s", out4)
	}
}

func TestProcessFrame(t *testing.T) {
	// [DONE]
	out := processFrame([]byte("data: [DONE]"))
	if string(out) != "data: [DONE]\n\n" {
		t.Fatalf("[DONE] frame got %q", out)
	}

	// 快路径：无 reasoning 原样透传（event + data 顺序保持一致）
	in := "event: message\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}"
	out = processFrame([]byte(in))
	if string(out) != in+"\n\n" {
		t.Fatalf("passthrough frame got %q", out)
	}

	// 含 reasoning：改写 data 行，保留 event 行
	in = "event: message\ndata: {\"choices\":[{\"delta\":{\"reasoning\":\"t\",\"content\":\"hi\"}}]}"
	out = processFrame([]byte(in))
	if !bytes.Contains(out, []byte(`"reasoning_content":"t"`)) {
		t.Fatalf("missing reasoning_content, got %q", out)
	}
	if !strings.HasPrefix(string(out), "event: message\n") {
		t.Fatalf("event line should come first, got %q", out)
	}

	// 全空帧 → nil（不输出）
	if processFrame([]byte("")) != nil {
		t.Fatal("empty frame should return nil")
	}
	if processFrame([]byte("\n")) != nil {
		t.Fatal("blank frame should return nil")
	}
}

func TestReadFrame(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("data: a\n\ndata: b\n\nx"))
	f, err := readFrame(br)
	if err != nil || string(f) != "data: a" {
		t.Fatalf("frame1 = %q err=%v", f, err)
	}
	f, err = readFrame(br)
	if err != nil || string(f) != "data: b" {
		t.Fatalf("frame2 = %q err=%v", f, err)
	}
	f, err = readFrame(br)
	if err != nil || string(f) != "x" { // EOF 残余帧（对应 JS flush 非空余量）
		t.Fatalf("frame3 = %q err=%v", f, err)
	}
	f, err = readFrame(br)
	if err != io.EOF || len(f) != 0 {
		t.Fatalf("frame4 = %q err=%v, want EOF", f, err)
	}
}

func TestJSONFalsyAndNull(t *testing.T) {
	falsy := []string{"null", `""`, "0", "-0", "0.0", "false", "0e0"}
	for _, in := range falsy {
		if !jsonFalsy([]byte(in)) {
			t.Errorf("jsonFalsy(%s) = false, want true", in)
		}
	}
	truthy := []string{`"x"`, "1", "-1", "true", "[]", "{}", `"0"`}
	for _, in := range truthy {
		if jsonFalsy([]byte(in)) {
			t.Errorf("jsonFalsy(%s) = true, want false", in)
		}
	}

	// jsonIsNull 只认 JSON null（与空字节）
	for _, in := range []string{"null"} {
		if !jsonIsNull([]byte(in)) {
			t.Errorf("jsonIsNull(%s) = false, want true", in)
		}
	}
	for _, in := range []string{`""`, "0", "-0", "0.0", "false", `"x"`, "1", "[]"} {
		if jsonIsNull([]byte(in)) {
			t.Errorf("jsonIsNull(%s) = true, want false", in)
		}
	}
}

// ---- 辅助函数 ----

func TestOwnedBy(t *testing.T) {
	cases := map[string]string{
		"deepseek/deepseek-v4-flash": "deepseek",
		"noprefix":                   "noprefix",
		"/weird":                     "cline", // split("/")[0] 为空 → "cline"
		"":                           "cline",
	}
	for in, want := range cases {
		if got := ownedByOf(in); got != want {
			t.Errorf("ownedByOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsStreamRequest(t *testing.T) {
	if !isStreamRequest([]byte(`{"stream":true}`)) {
		t.Error("stream:true should be streaming")
	}
	if isStreamRequest([]byte(`{"stream":false}`)) {
		t.Error("stream:false should be non-streaming")
	}
	if !isStreamRequest([]byte(`{"model":"x"}`)) { // 缺省
		t.Error("missing stream should be streaming")
	}
	if !isStreamRequest([]byte(`{"stream":null}`)) {
		t.Error("null stream should be streaming")
	}
	if !isStreamRequest([]byte(`not json`)) { // 解析失败按流式
		t.Error("unparseable body should be streaming")
	}
}

// ---- 代理集成测试 ----

func TestProxyChatNonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("x-client-type"); ct != "cline-cli" {
			t.Errorf("x-client-type = %q, want cline-cli", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi","reasoning":"think"}}]}`))
	}))
	defer upstream.Close()

	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("CLINE_API_KEY", "")
	t.Setenv("LOGIN_REQUIRED", "false")
	rr := callProxy(t, http.MethodPost, "/v1/chat/completions",
		[]byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`), nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rr.Body.String(), `"reasoning_content":"think"`) {
		t.Fatalf("missing rewritten reasoning_content: %s", rr.Body.String())
	}
}

func TestProxyChatStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\",\"reasoning\":\"think\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"bye\"}}]}\n\n")) // 快路径原样
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("CLINE_API_KEY", "")
	t.Setenv("LOGIN_REQUIRED", "false")
	rr := callProxy(t, http.MethodPost, "/v1/chat/completions",
		[]byte(`{"model":"x","stream":true}`), nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"reasoning_content":"think"`) {
		t.Fatalf("missing reasoning_content: %s", body)
	}
	if !strings.Contains(body, `data: {"choices":[{"delta":{"content":"bye"}}]}`) {
		t.Fatalf("passthrough chunk changed: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
}

func TestProxyChatAuth(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	t.Run("env key 优先于请求头", func(t *testing.T) {
		t.Setenv("UPSTREAM_URL", upstream.URL)
		t.Setenv("CLINE_API_KEY", "sk-env")
		t.Setenv("LOGIN_REQUIRED", "false")
		callProxy(t, http.MethodPost, "/v1/chat/completions",
			[]byte(`{"stream":false}`), map[string]string{"Authorization": "Bearer sk-client"})
		if gotAuth != "Bearer sk-env" {
			t.Fatalf("got auth %q, want Bearer sk-env", gotAuth)
		}
	})

	t.Run("无 env 时透传且不重复加前缀", func(t *testing.T) {
		t.Setenv("UPSTREAM_URL", upstream.URL)
		t.Setenv("CLINE_API_KEY", "")
		t.Setenv("LOGIN_REQUIRED", "false")
		callProxy(t, http.MethodPost, "/v1/chat/completions",
			[]byte(`{"stream":false}`), map[string]string{"Authorization": "Bearer sk-client"})
		if gotAuth != "Bearer sk-client" {
			t.Fatalf("got auth %q, want Bearer sk-client", gotAuth)
		}
	})

	t.Run("无 env 无前缀时自动加 Bearer", func(t *testing.T) {
		t.Setenv("UPSTREAM_URL", upstream.URL)
		t.Setenv("CLINE_API_KEY", "")
		t.Setenv("LOGIN_REQUIRED", "false")
		callProxy(t, http.MethodPost, "/v1/chat/completions",
			[]byte(`{"stream":false}`), map[string]string{"Authorization": "sk-raw"})
		if gotAuth != "Bearer sk-raw" {
			t.Fatalf("got auth %q, want Bearer sk-raw", gotAuth)
		}
	})
}

func TestProxyChatUpstreamError(t *testing.T) {
	t.Setenv("CLINE_API_KEY", "")
	t.Setenv("UPSTREAM_URL", "http://127.0.0.1:1") // 不可达 → 连接拒绝
	t.Setenv("LOGIN_REQUIRED", "false")
	rr := callProxy(t, http.MethodPost, "/v1/chat/completions", []byte(`{"stream":false}`), nil)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"upstream_error"`) {
		t.Fatalf("missing error type: %s", rr.Body.String())
	}
}

// ---- models 端点 ----

func TestFetchFreeModels(t *testing.T) {
	resetModelsCache()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != defaultUserAgent {
			t.Errorf("User-Agent = %q, want %q", ua, defaultUserAgent)
		}
		_, _ = w.Write(freeModelsBody())
	}))
	defer upstream.Close()

	list, err := fetchFreeModels(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 { // 123 / "" 两个脏 id 被过滤
		t.Fatalf("data len = %d, want 2: %+v", len(list.Data), list.Data)
	}
	first := list.Data[0]
	if first.ID != "deepseek/deepseek-v4-flash" || first.OwnedBy != "deepseek" ||
		first.Object != "model" || first.Name != "DeepSeek V4 Flash" ||
		first.Description != "deep desc" {
		t.Fatalf("first entry = %+v", first)
	}
	if list.Data[1].ID != "noprefix" || list.Data[1].OwnedBy != "noprefix" {
		t.Fatalf("second entry = %+v", list.Data[1])
	}
}

func TestModelsEndpoint(t *testing.T) {
	resetModelsCache()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(freeModelsBody())
	}))
	defer upstream.Close()
	t.Setenv("MODELS_UPSTREAM", upstream.URL)

	rr := callProxy(t, http.MethodGet, "/v1/models", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var resp ModelsList
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "list" || len(resp.Data) != 2 {
		t.Fatalf("resp = %+v", resp)
	}

	// 别名
	for _, p := range []string{"/v1/models/", "/models", "/models/"} {
		rr2 := callProxy(t, http.MethodGet, p, nil, nil)
		if rr2.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", p, rr2.Code)
		}
	}
}

func TestModelsEndpointFailure(t *testing.T) {
	resetModelsCache()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer upstream.Close()
	t.Setenv("MODELS_UPSTREAM", upstream.URL)

	rr := callProxy(t, http.MethodGet, "/v1/models", nil, nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"upstream_error"`) {
		t.Fatalf("missing error type: %s", rr.Body.String())
	}
}

func TestFetchFreeModelsCache(t *testing.T) {
	resetModelsCache()
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write(freeModelsBody())
	}))
	defer upstream.Close()

	if _, err := fetchFreeModels(upstream.URL); err != nil {
		t.Fatal(err)
	}
	list2, err := fetchFreeModels(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(list2.Data) != 2 {
		t.Fatalf("cached data len = %d", len(list2.Data))
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (cache hit)", calls)
	}
}

func TestFetchFreeModelsNegativeCache(t *testing.T) {
	resetModelsCache()
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	if _, err := fetchFreeModels(upstream.URL); err == nil {
		t.Fatal("expected first call to fail")
	}
	if _, err := fetchFreeModels(upstream.URL); err == nil {
		t.Fatal("expected negative cache to re-fail without upstream hit")
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (negative cache)", calls)
	}
}

func TestFetchFreeModelsConcurrency(t *testing.T) {
	resetModelsCache()
	calls := 0
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(100 * time.Millisecond) // 制造并发窗口
		_, _ = w.Write(freeModelsBody())
	}))
	defer upstream.Close()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fetchFreeModels(upstream.URL); err != nil {
				t.Errorf("fetchFreeModels: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (in-flight dedup)", calls)
	}
}

// ---- 路由 ----

func TestMethodNotAllowed(t *testing.T) {
	rr := callProxy(t, http.MethodGet, "/v1/chat/completions", nil, nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if rr.Body.String() != "Method Not Allowed" {
		t.Fatalf("body = %q, want Method Not Allowed", rr.Body.String())
	}
}

// ---- 回归：JS 对脏数组逐元素容错（Go 曾整体失败） ----

func TestRewriteDeltaMixedRawChoices(t *testing.T) {
	// choices 数组混入原始值元素：合法对象元素仍应改写（对应 JS `c && c.delta`）
	in := `{"choices":[123,{"delta":{"reasoning":"r","content":"c"}}]}`
	out, changed := rewriteDelta([]byte(in))
	if !changed {
		t.Fatal("expected rewrite despite raw element")
	}
	if !strings.Contains(string(out), `"reasoning_content":"r"`) {
		t.Fatalf("missing reasoning_content: %s", out)
	}
	if !strings.Contains(string(out), `123`) {
		t.Fatalf("raw element should be preserved: %s", out)
	}
}

func TestRewriteNonStreamMixedRawChoices(t *testing.T) {
	in := `{"choices":["x",{"message":{"reasoning":"r","content":"c"}}]}`
	out, ok := rewriteNonStream([]byte(in))
	if !ok {
		t.Fatal("expected parse success")
	}
	if !strings.Contains(string(out), `"reasoning_content":"r"`) {
		t.Fatalf("missing reasoning_content: %s", out)
	}
	if !strings.Contains(string(out), `"x"`) {
		t.Fatalf("raw element should be preserved: %s", out)
	}
}

// ---- 回归：processFrame 对 data: 后 \v 等 Unicode 空白剥离（对齐 JS trimStart） ----

func TestProcessFrameVTWhitespace(t *testing.T) {
	in := "data:\v{\"choices\":[{\"delta\":{\"reasoning\":\"r\",\"content\":\"c\"}}]}"
	out := processFrame([]byte(in))
	if out == nil {
		t.Fatal("expected a frame output")
	}
	if !bytes.Contains(out, []byte(`"reasoning_content":"r"`)) {
		t.Fatalf("expected rewrite after trimming \\v, got %q", out)
	}
}

// ---- 回归：models 上游脏数据容错 ----

func TestFetchFreeModelsTolerant(t *testing.T) {
	resetModelsCache()
	body := `{"free":[123,"abc",{"id":"deepseek/x","name":"X"},null,{"id":"","name":"empty"},{"no_id":1}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	list, err := fetchFreeModels(upstream.URL)
	if err != nil {
		t.Fatalf("dirty free array should not error: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "deepseek/x" {
		t.Fatalf("data = %+v, want exactly deepseek/x", list.Data)
	}
}

func TestFetchFreeModelsFreeNotArray(t *testing.T) {
	resetModelsCache()
	for _, body := range []string{`{"free":{"id":"x"}}`, `[1,2,3]`, `{"no_free":true}`} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		list, err := fetchFreeModels(upstream.URL)
		if err != nil {
			t.Fatalf("body %s should not error: %v", body, err)
		}
		if len(list.Data) != 0 {
			t.Fatalf("body %s: expected empty list, got %+v", body, list.Data)
		}
		upstream.Close()
		resetModelsCache()
	}
}

func TestFetchFreeModelsInvalidJSON(t *testing.T) {
	resetModelsCache()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"free":[`)) // 非法 JSON → 502（对应 JS resp.json() 抛错）
	}))
	defer upstream.Close()
	if _, err := fetchFreeModels(upstream.URL); err == nil {
		t.Fatal("invalid JSON body should error")
	}
}

// ---- 回归：非流式非 JSON 透传的 Content-Type / 流式无 body 透传 ----

func TestProxyChatNonStreamNonJSONContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer upstream.Close()
	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("CLINE_API_KEY", "")
	t.Setenv("LOGIN_REQUIRED", "false")

	rr := callProxy(t, http.MethodPost, "/v1/chat/completions",
		[]byte(`{"stream":false}`), nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain (JS 默认)", ct)
	}
	if rr.Body.String() != "<html>502 Bad Gateway</html>" {
		t.Fatalf("body = %q, want passthrough", rr.Body.String())
	}
}

func TestProxyChatStreamingNoBodyPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("CLINE_API_KEY", "")
	t.Setenv("LOGIN_REQUIRED", "false")

	rr := callProxy(t, http.MethodPost, "/v1/chat/completions",
		[]byte(`{"stream":true}`), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 passthrough", rr.Code)
	}
	if rr.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("upstream header not preserved: %q", rr.Header())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rr.Body.String())
	}
}
