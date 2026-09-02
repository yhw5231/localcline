// 冷却表：按 (上游 keyID, model) 记录故障冷却，路由引擎据此跳过不可用 key。
// 不同上游故障使用不同冷却时长：429 按 Retry-After（缺省 RATE_LIMIT_COOLDOWN）、
// 鉴权失败（401/403）用 AUTH_FAIL_COOLDOWN、服务端错误（5xx）用 SERVER_ERR_COOLDOWN、
// 网络错误用 NET_ERR_COOLDOWN。
package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

type cooldownPair struct {
	keyID string
	model string
}

// Cooldowns 冷却状态表。
type Cooldowns struct {
	mu    sync.Mutex
	until map[cooldownPair]time.Time
}

var cool *Cooldowns

func newCooldowns() *Cooldowns {
	return &Cooldowns{until: map[cooldownPair]time.Time{}}
}

// IsCooling 返回 (keyID, model) 是否处于冷却中。
func (c *Cooldowns) IsCooling(keyID, model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[cooldownPair{keyID, model}]
	return ok && time.Now().Before(until)
}

// Mark 记录冷却（dur <=0 时忽略）。
func (c *Cooldowns) Mark(keyID, model string, dur time.Duration) {
	if dur <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until[cooldownPair{keyID, model}] = time.Now().Add(dur)
	// 防膨胀：条目过多时清理已过期项
	if len(c.until) > 10000 {
		c.pruneLocked()
	}
}

// EarliestRetry 返回给定候选 key 集合中最早的冷却到期剩余时长；
// 全部未冷却返回 0, false。
func (c *Cooldowns) EarliestRetry(keyIDs []string, model string) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	var earliest time.Time
	for _, id := range keyIDs {
		until, ok := c.until[cooldownPair{id, model}]
		if ok && now.Before(until) {
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	d := time.Until(earliest)
	if d < 0 {
		d = 0
	}
	return d, true
}

// ClearAll 清空（测试用）。
func (c *Cooldowns) ClearAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = map[cooldownPair]time.Time{}
}

func (c *Cooldowns) pruneLocked() {
	now := time.Now()
	for k, until := range c.until {
		if !now.Before(until) {
			delete(c.until, k)
		}
	}
}

// retryAfterDuration 从上游 Retry-After 响应头解析冷却时长；空/解析失败返回 def。
func retryAfterDuration(header string, def time.Duration) time.Duration {
	if header == "" {
		return def
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	t, err := time.Parse(time.RFC1123, header)
	if err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return def
}
