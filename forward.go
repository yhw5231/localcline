// 共享的"选 key → 转发上游 → 429 冷却重试 → 改写响应"逻辑，供 API 反向代理与
// 普通代理/代理池的 managed 路径复用。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// extractModel 从请求体解析 model 字段（用于按 (key, model) 计算冷却）。
func extractModel(raw []byte) string {
	var parsed struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	return parsed.Model
}

// applyClientHeaders 把配置的客户端指纹头写入请求头（默认模拟官方 Cline VSCode 客户端）。
// 头名按配置原样发送，不经 Go 的 canonical 化；User-Agent 例外：Go 只认 canonical 键，
// 直接写小写键会被忽略并回退发送默认的 "Go-http-client/1.1"。空值跳过不发送。
func applyClientHeaders(h http.Header) {
	ch := cfg.ClientHeaders
	set := func(name, value string) {
		if value == "" {
			return
		}
		if strings.EqualFold(name, "User-Agent") {
			h.Set("User-Agent", value)
			return
		}
		h[name] = []string{value}
	}
	set("http-referer", ch.HTTPReferer)
	set("x-title", ch.Title)
	set("user-agent", ch.UserAgent)
	set("x-core-version", ch.CoreVersion)
	set("x-platform-version", ch.PlatformVersion)
	set("x-client-version", ch.ClientVersion)
	set("x-platform", ch.Platform)
	set("x-client-type", ch.ClientType)
	for name, value := range ch.Extra {
		set(name, value)
	}
}

// doUpstream 构造并发送上游请求（post + JSON body）。
func doUpstream(upstream string, rawBody []byte, auth string, extra func(*http.Request)) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, upstream, bytes.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	// 客户端指纹头（http-referer / x-platform / x-client-type 等），环境变量可改
	applyClientHeaders(req.Header)
	if extra != nil {
		extra(req)
	}
	return http.DefaultClient.Do(req)
}

// parseUsageJSON 从 OpenAI 响应体解析 usage.prompt_tokens / completion_tokens。
func parseUsageJSON(raw []byte) (int64, int64) {
	var obj struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Usage == nil {
		return 0, 0
	}
	return obj.Usage.PromptTokens, obj.Usage.CompletionTokens
}

// parseUsageFromFrame 从 SSE 帧中解析 usage（OpenAI 流式在末尾 chunk 带 usage）。
func parseUsageFromFrame(frame []byte) (int64, int64, bool) {
	for _, line := range strings.Split(string(frame), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if !strings.Contains(payload, "usage") {
			continue
		}
		p, c := parseUsageJSON([]byte(payload))
		if p > 0 || c > 0 {
			return p, c, true
		}
	}
	return 0, 0, false
}

// recordUsageToRecorder 把 token 用量写回 responseRecorder 的 reqStats。
func recordUsageToRecorder(w http.ResponseWriter, prompt, completion int64) {
	if prompt <= 0 && completion <= 0 {
		return
	}
	if rr, ok := w.(*responseRecorder); ok && rr.rs != nil {
		rr.rs.promptTokens += prompt
		rr.rs.completionTokens += completion
	}
}

// serveUpstreamResponse 把上游响应转发给客户端（含 reasoning -> reasoning_content 改写）。
func serveUpstreamResponse(w http.ResponseWriter, upResp *http.Response, stream bool) {
	defer upResp.Body.Close()

	if !stream {
		body, readErr := io.ReadAll(upResp.Body)
		if readErr != nil {
			writeJSONError(w, http.StatusBadGateway, "upstream read failed: "+readErr.Error(), "upstream_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		out, ok := rewriteNonStream(body)
		if !ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		p, c := parseUsageJSON(out)
		recordUsageToRecorder(w, p, c)
		w.WriteHeader(upResp.StatusCode)
		_, _ = w.Write(out)
		return
	}

	// 流式：无 body 的响应（如 204/304）原样透传
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
			break
		}
		if len(frame) > 0 {
			if p, c, ok := parseUsageFromFrame(frame); ok {
				recordUsageToRecorder(w, p, c)
			}
			if out := processFrame(frame); out != nil {
				_, _ = w.Write(out)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if readErr != nil {
			break
		}
	}
}

// forwardWithKeys 执行多 key 选择 + 429 冷却重试 + 改写转发的完整流程。
// nextKey 返回下一个可用的 key（及全部冷却时的 retryAfter）；
// markCool 记录 (key, model) 的冷却。
// maxTries <= 0 时默认 3 次。
func forwardWithKeys(w http.ResponseWriter, rawBody []byte, stream bool, model, upstream string,
	nextKey func(model string) (string, time.Duration),
	markCool func(key, model string, dur time.Duration),
	maxTries int) {

	if maxTries <= 0 {
		maxTries = 3
	}

	for attempt := 0; attempt < maxTries; attempt++ {
		key, retry := nextKey(model)
		if key == "" {
			// 没有可用 key（全部冷却）
			secs := int64(retry.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
			writeJSONError(w, http.StatusTooManyRequests,
				"no available upstream key for model "+model+" (retry after "+retry.String()+")", "rate_limited")
			return
		}
		upResp, err := doUpstream(upstream, rawBody, "Bearer "+key, nil)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "upstream_error")
			return
		}
		if upResp.StatusCode == http.StatusTooManyRequests {
			d := retryAfterDuration(upResp.Header.Get("Retry-After"), cfg.RateLimitCooldown)
			if markCool != nil {
				markCool(key, model, d)
			}
			upResp.Body.Close()
			continue
		}
		serveUpstreamResponse(w, upResp, stream)
		return
	}

	// 所有尝试都 429
	writeJSONError(w, http.StatusTooManyRequests, "all upstream keys rate limited for model "+model, "rate_limited")
}
