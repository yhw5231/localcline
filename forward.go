// 上游响应转发：SSE 逐帧读取、可选 reasoning -> reasoning_content 改写、用量解析。
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// passFrame 不改写地透传一帧 SSE（保留原字节，确保帧尾分隔符完整）。
func passFrame(frame []byte) []byte {
	if len(frame) == 0 {
		return nil
	}
	if frameEndsWithDoubleNL(frame) {
		return frame
	}
	return append(frame, '\n', '\n')
}

// serveUpstreamResponse 把上游响应转发给客户端（rewrite 时做 reasoning -> reasoning_content 改写）。
func serveUpstreamResponse(w http.ResponseWriter, upResp *http.Response, stream, rewrite bool) {
	defer upResp.Body.Close()

	if !stream {
		body, readErr := io.ReadAll(upResp.Body)
		if readErr != nil {
			writeJSONError(w, http.StatusBadGateway, "upstream read failed: "+readErr.Error(), "upstream_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		out, ok := body, false
		if rewrite {
			out, ok = rewriteNonStream(body)
		}
		if rewrite && !ok {
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
			var out []byte
			if rewrite {
				out = processFrame(frame)
			} else {
				out = passFrame(frame)
			}
			if out != nil {
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
