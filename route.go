// 路由引擎：把下游 OpenAI 兼容请求按渠道顺序 + key 顺序转发到上游，
// 失败时自动故障转移到下一个 key/渠道。
//
// 候选顺序：渠道在配置中的顺序即优先级；同一渠道内按 key 顺序。
// 每次请求最多尝试 cfg.MaxRouteTries 个候选；命中冷却 (keyID, model) 的候选直接跳过。
// 故障分类与冷却：
//   - 网络错误            → NET_ERR_COOLDOWN（默认 15s），可配置自动换出口 IP
//   - 429                 → Retry-After（缺省 RATE_LIMIT_COOLDOWN，默认 1h）
//   - 401/403（鉴权失败） → AUTH_FAIL_COOLDOWN（默认 10m）
//   - 5xx                 → SERVER_ERR_COOLDOWN（默认 30s）
//   - 其他（含上游 400）  → 原样透传给下游（上游的业务语义不动）
package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

// candidate 一个可路由的 (渠道, key) 组合。
type candidate struct {
	ch *Channel
	k  *UpKey
}

// chatTarget 返回该候选的上游 chat 端点（key BaseURL 优先）。
func (c *candidate) chatTarget() string {
	if u := c.k.BaseURL; u != "" {
		base := trimSlash(u)
		if endsWithChatCompletions(base) {
			return base
		}
		return base + "/chat/completions"
	}
	return c.ch.chatURL()
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func endsWithChatCompletions(s string) bool {
	const suffix = "/chat/completions"
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// buildCandidates 按优先级构建候选列表（深拷贝自 store）。
func buildCandidates(model string) []candidate {
	snap := store.Snapshot()
	var out []candidate
	for _, ch := range snap.Channels {
		if !ch.Enabled {
			continue
		}
		if !ch.allowsModel(model) {
			continue
		}
		for _, k := range ch.Keys {
			if k.Enabled {
				out = append(out, candidate{ch: ch, k: k})
			}
		}
	}
	return out
}

// resolveProxy 解析候选的代理路由（ipv6pool 懒解析：首次用时向池申请租约并缓存）。
func resolveProxy(cand *candidate) (*ProxyRoute, error) {
	spec := cand.k.Proxy
	if spec == nil || spec.Kind == "" {
		return nil, nil
	}
	switch spec.Kind {
	case "static":
		return parseProxyURL(spec.URL)
	case "ipv6pool":
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return leaseMgr.Ensure(ctx, spec, cand.k.ID, proxyGroup(cand.ch, cand.k))
	default:
		return nil, nil
	}
}

// maybeRotate 上游返回指定状态码时更换 ipv6pool 出口 IP（best effort）。
func maybeRotate(cand *candidate, status int) {
	spec := cand.k.Proxy
	if spec == nil || spec.Kind != "ipv6pool" {
		return
	}
	for _, s := range spec.RotateStatuses {
		if s != status {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := leaseMgr.Rotate(ctx, spec, cand.k.ID)
		cancel()
		if err != nil {
			log.Printf("rotate lease for key %s after status %d: %v", cand.k.Name, status, err)
		}
		return
	}
}

// rotateOnNetErr 网络失败时按配置换 IP。
func rotateOnNetErr(cand *candidate) {
	spec := cand.k.Proxy
	if spec == nil || spec.Kind != "ipv6pool" || !spec.RotateOnNetErr {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, err := leaseMgr.Rotate(ctx, spec, cand.k.ID)
	cancel()
	if err != nil {
		log.Printf("rotate lease for key %s after network error: %v", cand.k.Name, err)
	}
}

// applyChannelHeaders 把渠道级自定义头写入请求（头名原样保留，空值跳过）。
func applyChannelHeaders(h http.Header, headers map[string]string) {
	for name, value := range headers {
		if value == "" {
			continue
		}
		h[name] = []string{value}
	}
}

// forwardChat 执行故障转移转发，把最终响应写给下游 w。
// 返回实际服务的候选（用于统计），无可用上游时返回 nil。
func forwardChat(w http.ResponseWriter, r *http.Request, rawBody []byte, stream bool, model string) *candidate {
	cands := buildCandidates(model)
	if len(cands) == 0 {
		writeJSONError(w, http.StatusBadGateway, "no enabled upstream key available", "no_upstream")
		return nil
	}

	maxTries := cfg.MaxRouteTries
	if maxTries <= 0 {
		maxTries = len(cands)
	}
	if maxTries > len(cands) {
		maxTries = len(cands)
	}

	var (
		served      *candidate
		lastErr     string
		rateLimited bool
	)
	attempts := 0
	for _, cand := range cands {
		if attempts >= maxTries {
			break
		}
		if cool.IsCooling(cand.k.ID, model) {
			continue
		}
		attempts++

		route, err := resolveProxy(&cand)
		if err != nil {
			lastErr = "resolve proxy: " + err.Error()
			log.Printf("route: key %s proxy resolve failed: %v", cand.k.Name, err)
			cool.Mark(cand.k.ID, model, cfg.NetErrCooldown)
			continue
		}

		client := newUpstreamClient(route)
		resp, err := doUpstreamRequest(r.Context(), client, &cand, rawBody)
		if err != nil {
			lastErr = "upstream: " + err.Error()
			cool.Mark(cand.k.ID, model, cfg.NetErrCooldown)
			rotateOnNetErr(&cand)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			rateLimited = true
			d := retryAfterDuration(resp.Header.Get("Retry-After"), cfg.RateLimitCooldown)
			cool.Mark(cand.k.ID, model, d)
			leaseMgr.RecordUse(cand.k.Proxy, cand.k.ID)
			maybeRotate(&cand, resp.StatusCode)
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastErr = "upstream 429"
			continue
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			cool.Mark(cand.k.ID, model, cfg.AuthFailCooldown)
			maybeRotate(&cand, resp.StatusCode)
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastErr = "upstream auth failed"
			continue
		case resp.StatusCode >= 500:
			cool.Mark(cand.k.ID, model, cfg.ServerErrCooldown)
			maybeRotate(&cand, resp.StatusCode)
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastErr = "upstream 5xx"
			continue
		}

		// 成功拿到可透传的响应：改写（可选）后回给下游
		leaseMgr.RecordUse(cand.k.Proxy, cand.k.ID)
		serveUpstreamResponse(w, resp, stream, cand.ch.Rewrite)
		served = &cand
		break
	}

	if served != nil {
		return served
	}

	if rateLimited {
		ids := make([]string, 0, len(cands))
		for _, c := range cands {
			ids = append(ids, c.k.ID)
		}
		if d, ok := cool.EarliestRetry(ids, model); ok {
			secs := int64(d.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
			writeJSONError(w, http.StatusTooManyRequests,
				"all upstream keys rate limited for model "+model, "rate_limited")
			return nil
		}
	}
	msg := "all upstream attempts failed"
	if lastErr != "" {
		msg += ": " + lastErr
	}
	writeJSONError(w, http.StatusBadGateway, msg, "upstream_error")
	return nil
}

// doUpstreamRequest 构造并发送上游请求：注入渠道头 + Bearer key。
func doUpstreamRequest(ctx context.Context, client *http.Client, cand *candidate, rawBody []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cand.chatTarget(), bytes.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cand.k.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cand.k.APIKey)
	}
	applyChannelHeaders(req.Header, cand.ch.Headers)
	return client.Do(req)
}
