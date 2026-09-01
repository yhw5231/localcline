// 多上游 key 管理与限流冷却。
// 支持顺序优先（sequential）与轮询优先（roundrobin）两种选择模式。
// 冷却按 (key, model) 单独记录，满足"同一个账号每个模型额度单独计算"的要求。
package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

type keyModelPair struct {
	key   string
	model string
}

// KeyManager 管理一组上游 key 的选择与冷却状态。
type KeyManager struct {
	mu       sync.Mutex
	keys     []string
	mode     string // "sequential" | "roundrobin"
	rrNext   int    // round-robin 游标
	cooldown map[keyModelPair]time.Time
}

// newKeyManager 创建 KeyManager。
func newKeyManager(keys []string, mode string) *KeyManager {
	if mode != "roundrobin" {
		mode = "sequential"
	}
	// 去重（避免重复 key 在同一个 model 下产生歧义）
	dedup := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" && !seen[k] {
			dedup = append(dedup, k)
			seen[k] = true
		}
	}
	return &KeyManager{
		keys:     dedup,
		mode:     mode,
		cooldown: make(map[keyModelPair]time.Time),
	}
}

// Next 返回给定 model 下一个可用的 key（不在冷却中）。
// 如果所有 key 都在冷却，返回 ("", retryAfterDuration)；此时 retryAfter 为最早冷却到期前的时长。
// 如果没有任何 key 被配置，返回 ("", 0)。
func (km *KeyManager) Next(model string) (string, time.Duration) {
	km.mu.Lock()
	defer km.mu.Unlock()

	n := len(km.keys)
	if n == 0 {
		return "", 0
	}
	now := time.Now()
	var earliest time.Time

	for i := 0; i < n; i++ {
		idx := i
		if km.mode == "roundrobin" {
			idx = (km.rrNext + i) % n
		}
		key := km.keys[idx]
		until, cooling := km.cooldown[keyModelPair{key, model}]
		if cooling && now.Before(until) {
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
			continue
		}
		// 可用
		if km.mode == "roundrobin" {
			km.rrNext = (idx + 1) % n
		}
		return key, 0
	}
	// 全部冷却
	if earliest.IsZero() {
		return "", 0
	}
	d := time.Until(earliest)
	if d < 0 {
		d = 0
	}
	return "", d
}

// MarkRateLimited 记录 key 的 model 在冷却中，持续 dur 时长。
// dur <= 0 时使用全局默认冷却时长（60s）。
func (km *KeyManager) MarkRateLimited(key, model string, dur time.Duration) {
	if dur <= 0 {
		dur = 60 * time.Second
	}
	km.mu.Lock()
	defer km.mu.Unlock()
	until := time.Now().Add(dur)
	km.cooldown[keyModelPair{key, model}] = until
	// 定期清理：map 过大时删掉已过期的条目
	if len(km.cooldown) > 10000 {
		km.pruneLocked()
	}
}

// CooldownEnd 返回 (key, model) 的冷却到期时间；若未冷却返回零值。
func (km *KeyManager) CooldownEnd(key, model string) time.Time {
	km.mu.Lock()
	defer km.mu.Unlock()
	return km.cooldown[keyModelPair{key, model}]
}

// ClearAll 清空所有冷却记录（测试用）。
func (km *KeyManager) ClearAll() {
	km.mu.Lock()
	defer km.mu.Unlock()
	km.cooldown = make(map[keyModelPair]time.Time)
	km.rrNext = 0
}

// pruneLocked 删除已过期的冷却记录（调用者持有锁）。
func (km *KeyManager) pruneLocked() {
	now := time.Now()
	for k, until := range km.cooldown {
		if !now.Before(until) {
			delete(km.cooldown, k)
		}
	}
}

// Keys 返回当前管理的 key 列表（副本）。
func (km *KeyManager) Keys() []string {
	km.mu.Lock()
	defer km.mu.Unlock()
	out := make([]string, len(km.keys))
	copy(out, km.keys)
	return out
}

// retryAfterDuration 从上游 Retry-After 响应头解析冷却时长。
// 如果头为空或解析失败，返回默认值 def。
func retryAfterDuration(header string, def time.Duration) time.Duration {
	if header == "" {
		return def
	}
	// 优先尝试 HTTP-date，但 Cline 一般返回秒数
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	// 尝试 HTTP-date（如 "Wed, 21 Oct 2015 07:28:00 GMT"）
	t, err := time.Parse(time.RFC1123, header)
	if err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return def
}