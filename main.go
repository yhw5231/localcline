// cline2api 的 Go 移植版：行为与 src/index.js（Cloudflare Worker）保持一致。
//
// 核心目标：把 Cline 返回的 delta.reasoning 重写为 delta.reasoning_content，
// 使 sub2api（只识别 reasoning_content）能把思考转成 Anthropic thinking block。
//
// 路由：
//   - POST        原样转发到上游 chat/completions（默认 api.cline.bot），
//     SSE 逐帧把 choices[].delta.reasoning 复制到 delta.reasoning_content；
//     非流式把 choices[].message.reasoning 复制到 message.reasoning_content。
//   - GET /v1/models（及 /v1/models/、/models、/models/ 别名）
//     拉取 Cline 推荐模型，只保留 free（免费）模型，转 OpenAI 兼容格式。
//
// 鉴权：优先环境变量 CLINE_API_KEY，否则透传请求自带的 Authorization。
//
// 环境变量：
//
//	UPSTREAM_URL    聊天上游，默认 https://api.cline.bot/api/v1/chat/completions
//	MODELS_UPSTREAM 免费模型上游，默认 https://api.cline.bot/api/v1/ai/cline/recommended-models
//	CLINE_API_KEY   可选，覆盖透传的 Authorization
//	PORT            监听端口，默认 8080
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	defaultUpstream       = "https://api.cline.bot/api/v1/chat/completions"
	defaultModelsUpstream = "https://api.cline.bot/api/v1/ai/cline/recommended-models"

	// Cline 需要的客户端标识（与官方 CLI 一致）。聊天转发与模型拉取都用同一 UA，
	// 避免 Go 默认暴露 "Go-http-client/1.1"。
	defaultUserAgent = "Cline/3.0.38"

	modelsCacheTTL     = 5 * time.Minute  // 免费模型列表成功缓存时长
	modelsErrorCache   = 30 * time.Second // 上游失败后的负缓存时长（吸收突发请求）
	modelsFetchTimeout = 10 * time.Second // 拉取超时，对应 JS 的 AbortController 10s

	defaultPort = "8080"
)

// ModelsList / ModelEntry：/v1/models 的 OpenAI 兼容返回结构。
// 注意 created 不带 omitempty，必须输出 0（JS 里硬编码 created: 0）。
type ModelsList struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

type ModelEntry struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int    `json:"created"`
	OwnedBy     string `json:"owned_by"`
	Name        string `json:"name,omitempty"`        // 附加字段，客户端可显示友好名
	Description string `json:"description,omitempty"` // 附加字段
}

// ---- 免费模型列表内存缓存（成功与失败都缓存） ----
// modelsPending 按 upstream 记录 in-flight 的完成信号：同一 upstream 并发只发一次上游请求。
var (
	modelsMu      sync.Mutex
	modelsEntry   *modelsEntryRec
	modelsPending = make(map[string]chan struct{})
)

type modelsEntryRec struct {
	upstream  string
	timestamp time.Time
	list      *ModelsList
	err       error
}

func main() {
	addr := ":" + getenv("PORT", defaultPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(handler),
		ReadHeaderTimeout: 30 * time.Second,
	}
	log.Printf("cline2api (go) listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func handler(w http.ResponseWriter, r *http.Request) {
	// 模型列表：GET /v1/models（兼容 OpenAI 客户端），只返回免费模型
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/v1/models", "/v1/models/", "/models", "/models/":
			handleModels(w)
			return
		}
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("Method Not Allowed"))
		return
	}

	proxyChat(w, r)
}

func proxyChat(w http.ResponseWriter, r *http.Request) {
	// 读取请求体（顺带判断是否流式）
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read request body failed: "+err.Error(), "bad_request")
		return
	}
	stream := isStreamRequest(rawBody)

	upstream := getenv("UPSTREAM_URL", defaultUpstream)

	// 鉴权：优先用环境变量里的 Cline key；否则透传请求自带的 Authorization
	auth := getenv("CLINE_API_KEY", "")
	if auth == "" {
		auth = r.Header.Get("Authorization")
	}
	if auth != "" && !isBearerToken(auth) {
		auth = "Bearer " + auth
	}

	req, err := http.NewRequest(http.MethodPost, upstream, bytes.NewReader(rawBody))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "upstream_error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	// Cline 需要的客户端标识
	req.Header.Set("x-client-type", "cline-cli")
	req.Header.Set("User-Agent", defaultUserAgent)

	// 无总超时：流式响应可能长跑（对应 JS fetch 不带超时）
	upResp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "upstream_error")
		return
	}
	defer upResp.Body.Close()

	// 非流式：改写 message.reasoning -> message.reasoning_content；非 JSON 原样透传
	if !stream {
		body, readErr := io.ReadAll(upResp.Body)
		if readErr != nil {
			writeJSONError(w, http.StatusBadGateway, "upstream read failed: "+readErr.Error(), "upstream_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		out, ok := rewriteNonStream(body)
		if !ok {
			// 上游返回非 JSON（如错误页）：JS 用 `new Response(body, {status})` 让
			// 平台默认给 text/plain，这里显式设置，避免 Go 的 net/http 嗅探出 text/html。
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		w.WriteHeader(upResp.StatusCode)
		_, _ = w.Write(out)
		return
	}

	// 流式：逐帧改写 SSE
	// 无 body 的响应（如 204/304）：JS `if (!upstreamResp.body) return upstreamResp` 原样
	// 透传上游响应（保留其头）。Go 在 204/304 时 Body 恒为空的 NoBody、Content-Length 0。
	if upResp.ContentLength == 0 {
		copyHeader(w.Header(), upResp.Header)
		w.WriteHeader(upResp.StatusCode)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(upResp.StatusCode)
	flusher, _ := w.(http.Flusher)

	br := bufio.NewReader(upResp.Body)
	for {
		frame, readErr := readFrame(br)
		if readErr != nil && readErr != io.EOF {
			break // 上游连接中断：丢弃未完成帧（对应 JS TransformStream 报错中止流）
		}
		if len(frame) > 0 {
			if out := processFrame(frame); out != nil {
				_, _ = w.Write(out)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if readErr != nil {
			break // EOF：结束转发
		}
	}
}

// copyHeader 把源 header 复制到目标（用于无 body 响应的整体透传）。
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// isStreamRequest：请求体里 stream === false 才算非流式；
// 缺省 / stream 为 true / null / 其他值时按流式处理（与 JS `parsed.stream === false` 一致）。
func isStreamRequest(raw []byte) bool {
	var parsed struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return true
	}
	if parsed.Stream == nil {
		return true
	}
	return *parsed.Stream
}

// isBearerToken：是否已带 "Bearer " 前缀（大小写不敏感，对应 JS 正则 /^Bearer /i）。
func isBearerToken(auth string) bool {
	return len(auth) >= len("Bearer ") && strings.EqualFold(auth[:len("Bearer ")], "Bearer ")
}

// ---- SSE 处理 ----

// readFrame 以 "\n\n" 为帧边界读取一帧（对应 JS 的 TransformStream），返回不含分隔符的帧内容。
// 到达 EOF 时：若有残余且非全空白，作为最后一帧返回 (frame, nil)；否则返回 (nil, io.EOF)。
// 行超长（超出 bufio 缓冲区）时继续累积，避免把单帧拆散。
func readFrame(br *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		line, err := br.ReadSlice('\n')
		frame = append(frame, line...)
		if frameEndsWithDoubleNL(frame) {
			return frame[:len(frame)-2], nil
		}
		if err != nil {
			if err == bufio.ErrBufferFull {
				continue
			}
			if err == io.EOF {
				if len(bytes.TrimSpace(frame)) == 0 {
					return nil, io.EOF
				}
				return frame, nil // 非空残余帧（对应 JS flush 分支）
			}
			return nil, err
		}
	}
}

func frameEndsWithDoubleNL(frame []byte) bool {
	n := len(frame)
	return n >= 2 && frame[n-2] == '\n' && frame[n-1] == '\n'
}

// processFrame 处理一帧 SSE：把 data 行中的 delta.reasoning 复制到 delta.reasoning_content。
// 与 JS 一致：data 行与非 data 行分开再拼接；无有效行时返回 nil（不输出）。
func processFrame(frame []byte) []byte {
	lines := strings.Split(string(frame), "\n")
	var dataLines, otherLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			// 对应 JS 的 line.slice(5).trimStart()：剥掉 slice 后左侧全部 Unicode whitespace
			// （含 \t \v \f \r 及 NBSP 等），避免 \v 等残留在 JSON 前面导致解析失败。
			dataLines = append(dataLines, strings.TrimLeftFunc(line[len("data:"):], unicode.IsSpace))
		} else {
			otherLines = append(otherLines, line)
		}
	}

	parts := make([]string, 0, len(otherLines)+len(dataLines))
	for _, l := range otherLines {
		if l != "" {
			parts = append(parts, l)
		}
	}
	for _, d := range dataLines {
		if d == "[DONE]" {
			parts = append(parts, "data: [DONE]")
			continue
		}
		// 快路径：不含 "reasoning" 的 chunk 大概率无需改写，直接透传，
		// 跳过 JSON 解析/序列化，显著降低长流式下的 CPU 消耗。
		// 注：JSON 字符串值内的引号会被转义为 \"，不会误匹配字段名 "reasoning"。
		if strings.Contains(d, `"reasoning"`) {
			if out, changed := rewriteDelta([]byte(d)); changed {
				parts = append(parts, "data: "+string(out))
				continue
			}
			// 不可解析或未改写则原样透传（对应 JS 的 catch / 未 changed 分支）
		}
		parts = append(parts, "data: "+d)
	}

	if len(parts) == 0 {
		return nil
	}
	return []byte(strings.Join(parts, "\n") + "\n\n")
}

// rewriteDelta 改写流式 chunk：delta.reasoning -> delta.reasoning_content。
// 返回改写后的字节与是否确实发生了改写（未改 / 不可解析时返回原字节 + false）。
// 用 map[string]json.RawMessage 保留各字段原始字节，只重排键序、不改数字精度与转义。
func rewriteDelta(raw []byte) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, false
	}
	rawChoices, ok := obj["choices"]
	if !ok {
		return raw, false
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil {
		return raw, false
	}

	changed := false
	for i := range choices {
		// 逐元素解码：null / 原始值 / 脏对象一律跳过（对应 JS `const d = c && c.delta` 的宽松处理），
		// 其它带 delta.reasoning 的对象元素照常改写 —— 避免整个数组因单个脏元素一起解析失败而放弃改写
		var choice map[string]json.RawMessage
		if err := json.Unmarshal(choices[i], &choice); err != nil {
			continue
		}
		rawDelta, ok := choice["delta"]
		if !ok {
			continue
		}
		var delta map[string]json.RawMessage
		if err := json.Unmarshal(rawDelta, &delta); err != nil {
			continue
		}
		reasoning, hasReasoning := delta["reasoning"]
		// 对应 JS `d.reasoning != null`：字段存在且值非 JSON null
		if !hasReasoning || jsonIsNull(reasoning) {
			continue
		}
		// 对应 JS `!d.reasoning_content`：缺失或为 falsy 值才覆盖
		if rc, hasRC := delta["reasoning_content"]; hasRC && !jsonFalsy(rc) {
			continue
		}
		delta["reasoning_content"] = reasoning
		newDelta, err := json.Marshal(delta)
		if err != nil {
			continue
		}
		choice["delta"] = newDelta
		newChoice, err := json.Marshal(choice)
		if err != nil {
			continue
		}
		choices[i] = newChoice
		changed = true
	}

	if !changed {
		return raw, false
	}
	newChoices, err := json.Marshal(choices)
	if err != nil {
		return raw, false
	}
	obj["choices"] = newChoices
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, false
	}
	return out, true
}

// rewriteNonStream 改写非流式响应：message.reasoning -> message.reasoning_content。
// 返回 (改写后字节, 是否解析成功)。解析失败返回 (原字节, false)，外层据此原样透传（不设 JSON Content-Type）。
func rewriteNonStream(raw []byte) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, false
	}
	rawChoices, ok := obj["choices"]
	if !ok {
		return raw, true // 解析成功但无 choices（如上游错误体），原样返回
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil {
		return raw, true
	}

	changed := false
	for i := range choices {
		var choice map[string]json.RawMessage
		if err := json.Unmarshal(choices[i], &choice); err != nil {
			continue // 跳过 null / 原始值元素，与 JS 逐元素容错一致
		}
		rawMsg, ok := choice["message"]
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}
		reasoning, hasReasoning := msg["reasoning"]
		if !hasReasoning || jsonIsNull(reasoning) {
			continue
		}
		if rc, hasRC := msg["reasoning_content"]; hasRC && !jsonFalsy(rc) {
			continue
		}
		msg["reasoning_content"] = reasoning
		newMsg, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		choice["message"] = newMsg
		newChoice, err := json.Marshal(choice)
		if err != nil {
			continue
		}
		choices[i] = newChoice
		changed = true
	}

	if !changed {
		return raw, true
	}
	newChoices, err := json.Marshal(choices)
	if err != nil {
		return raw, true
	}
	obj["choices"] = newChoices
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, true
	}
	return out, true
}

// jsonFalsy：对应 JS 的宽松 truthiness 检查（!value 为 true 的情况）。
// JS 里 null / "" / 0（含 -0、0.0）/ false / undefined 均为 falsy。
func jsonFalsy(rm json.RawMessage) bool {
	t := bytes.TrimSpace(rm)
	// 只有数字字面量（以 - 或数字开头）才尝试数值判断。
	// 不能直接对 json.Number 调 Unmarshal：其底层是 string，会顺手接收 JSON 字符串 "0"，
	// 使 jsonFalsy(`"0"`) 误判为 true（JS 里 "0" 是 truthy）。
	if len(t) > 0 && (t[0] == '-' || (t[0] >= '0' && t[0] <= '9')) {
		var n json.Number
		if err := json.Unmarshal(t, &n); err == nil {
			f, err := n.Float64()
			return err == nil && f == 0
		}
		return false
	}
	// 注：JSON null 解码到 string/bool 时 Go 不报错而置零值，恰好落入 falsy 分支。
	var s string
	if err := json.Unmarshal(t, &s); err == nil {
		return s == ""
	}
	var b bool
	if err := json.Unmarshal(t, &b); err == nil {
		return !b
	}
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

func jsonIsNull(rm json.RawMessage) bool {
	t := bytes.TrimSpace(rm)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// ---- GET /v1/models ----

func handleModels(w http.ResponseWriter) {
	upstream := getenv("MODELS_UPSTREAM", defaultModelsUpstream)
	list, err := fetchFreeModels(upstream)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "models fetch failed: "+err.Error(), "upstream_error")
		return
	}
	body, _ := json.Marshal(list)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// fetchFreeModels 拉取免费模型列表：成功/失败都按 upstream 缓存（TTL / 负缓存），
// 并发去重避免重复打上游（同一 upstream 并发只发一次请求）。
func fetchFreeModels(upstream string) (*ModelsList, error) {
	for {
		modelsMu.Lock()
		if modelsEntry != nil && modelsEntry.upstream == upstream {
			age := time.Since(modelsEntry.timestamp)
			if modelsEntry.err != nil {
				if age < modelsErrorCache {
					err := modelsEntry.err // 负缓存：短时间内直接复用上次失败
					modelsMu.Unlock()
					return nil, err
				}
			} else if age < modelsCacheTTL {
				list := modelsEntry.list
				modelsMu.Unlock()
				return list, nil
			}
		}
		if ch, ok := modelsPending[upstream]; ok {
			modelsMu.Unlock()
			<-ch // 等待 in-flight 完成
			continue
		}
		ch := make(chan struct{})
		modelsPending[upstream] = ch
		modelsMu.Unlock()

		list, err := fetchModelsUpstream(upstream)

		modelsMu.Lock()
		modelsEntry = &modelsEntryRec{upstream: upstream, timestamp: time.Now(), list: list, err: err}
		delete(modelsPending, upstream)
		close(ch)
		modelsMu.Unlock()
		return list, err
	}
}

// fetchModelsUpstream 请求 Cline 推荐模型端点，只保留 free 数组中的有效模型。
// 容错语义与 JS 对齐：resp.ok（2xx）都算成功；free 缺失/非数组视为空列表而非报错；
// free 数组中的原始值/脏元素逐条过滤，只保留「id 是非空字符串」的对象。
func fetchModelsUpstream(upstream string) (*ModelsList, error) {
	client := &http.Client{Timeout: modelsFetchTimeout}
	req, err := http.NewRequest(http.MethodGet, upstream, nil)
	if err != nil {
		return nil, err
	}
	// Cline 推荐模型端点无需鉴权；带 UA 与官方客户端一致
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("recommended-models returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// 先按任意 JSON 解析：与 JS `resp.json()` 一致 —— body 非法 JSON 时整体失败；
	// 合法 JSON 但顶层不是对象时（数组/标量），json.free 自然为 undefined → 空列表。
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	obj, isObject := parsed.(map[string]any)
	var free []any
	if isObject {
		free, _ = obj["free"].([]any) // 非数组 → empty slice（对应 JS Array.isArray 兜底）
	}

	list := &ModelsList{Object: "list"}
	for _, e := range free {
		eb, err := json.Marshal(e)
		if err != nil {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(eb, &m); err != nil {
			continue // 原始值 / 脏元素丢弃（对应 JS `typeof m === "object"` 过滤）
		}
		var id string
		if err := json.Unmarshal(m["id"], &id); err != nil || id == "" {
			continue
		}
		entry := ModelEntry{
			ID:      id,
			Object:  "model",
			Created: 0,
			OwnedBy: ownedByOf(id),
		}
		if name, ok := stringField(m["name"]); ok && name != "" {
			entry.Name = name
		}
		if desc, ok := stringField(m["description"]); ok && desc != "" {
			entry.Description = desc
		}
		list.Data = append(list.Data, entry)
	}
	return list, nil
}

// stringField：把字段字节解析成字符串；字段缺失或非字符串返回 (_, false)。
func stringField(rm json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(rm, &s); err != nil {
		return "", false
	}
	return s, true
}

// ownedByOf：取 id.split("/")[0]，空则 "cline"（对应 JS `m.id.split("/")[0] || "cline"`）。
func ownedByOf(id string) string {
	s := id
	if idx := strings.Index(id, "/"); idx >= 0 {
		s = id[:idx]
	}
	if s == "" {
		return "cline"
	}
	return s
}

func writeJSONError(w http.ResponseWriter, status int, message, typ string) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
