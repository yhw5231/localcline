// 上游模型列表拉取：用渠道 key（含其代理配置）请求上游 /models，
// 解析模型 ID 与价格（上游返回价格且输入/输出均为 0 视为免费），
// 并把结果写回渠道的 Models（作为可用模型，用于路由过滤与 /v1/models）。
// 免费模型清单仅随 Admin API 响应返回，供 WebUI 临时标注展示，不落盘。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// fetchModelsMaxKeys 拉取模型时最多依次尝试的 key 数（key 失败自动换下一个）。
const fetchModelsMaxKeys = 3

// fetchModelsTimeout 单次请求上游模型列表的超时。
const fetchModelsTimeout = 20 * time.Second

// fetchUpstreamModels 用渠道的启用 key 依次拉取上游模型列表（Bearer 鉴权，
// key 配置了代理则走代理）。返回模型 ID 列表与免费模型子集。
// 全部尝试失败时返回最后一个错误。
func fetchUpstreamModels(ctx context.Context, ch *Channel) (models []string, free []string, usedKey string, err error) {
	var lastErr error
	attempts := 0
	for _, k := range ch.Keys {
		if !k.Enabled || attempts >= fetchModelsMaxKeys {
			continue
		}
		attempts++
		cand := candidate{ch: ch, k: k}
		list, freelist, ferr := fetchModelsWithKey(ctx, &cand)
		if ferr != nil {
			lastErr = fmt.Errorf("key %q: %w", k.Name, ferr)
			continue
		}
		return list, freelist, k.Name, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no enabled key to fetch models")
	}
	return nil, nil, "", lastErr
}

// fetchModelsWithKey 用单个 key 拉取模型列表（应用 key 代理与渠道自定义头）。
func fetchModelsWithKey(ctx context.Context, cand *candidate) (models []string, free []string, err error) {
	route, err := resolveProxy(cand)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve proxy: %w", err)
	}
	client := newUpstreamClient(route)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cand.modelsTarget(), nil)
	if err != nil {
		return nil, nil, err
	}
	if cand.k.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cand.k.APIKey)
	}
	applyChannelHeaders(req.Header, cand.ch.Headers)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseModelsPayload(body)
}

// modelsTarget 返回该候选的上游模型列表端点（key BaseURL 优先，覆盖渠道配置）。
func (c *candidate) modelsTarget() string {
	if u := c.k.BaseURL; u != "" {
		base := trimSlash(u)
		if strings.HasSuffix(base, "/models") {
			return base
		}
		return base + "/models"
	}
	return c.ch.modelsURL()
}

// parseModelsPayload 解析上游 /models 响应：data[].id 为模型 ID；
// data[].pricing（prompt/completion 或 input/output，数字或字符串）解析价格，
// 两项价格都存在且均为 0 时该模型记为免费。
func parseModelsPayload(body []byte) (models []string, free []string, err error) {
	var parsed struct {
		Data []struct {
			ID      string          `json:"id"`
			Pricing json.RawMessage `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("parse models response: %w", err)
	}
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, m.ID)
		if p, c, ok := parseModelPricing(m.Pricing); ok && p == 0 && c == 0 {
			free = append(free, m.ID)
		}
	}
	return models, free, nil
}

// parseModelPricing 从 pricing 对象解析 (输入价格, 输出价格, 是否提供了价格)。
// 兼容数字与字符串形式，键名兼容 prompt/completion 与 input/output。
func parseModelPricing(raw json.RawMessage) (prompt, completion float64, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, 0, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return 0, 0, false
	}
	prompt, pok := pricingField(obj, "prompt", "input")
	completion, cok := pricingField(obj, "completion", "output")
	return prompt, completion, pok || cok
}

func pricingField(obj map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, present := obj[k]
		if !present || v == nil {
			continue
		}
		switch n := v.(type) {
		case float64:
			return n, true
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}
