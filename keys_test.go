// KeyManager 选择模式与冷却测试。
package main

import (
	"testing"
	"time"
)

func TestNewKeyManager(t *testing.T) {
	km := newKeyManager([]string{"k1", "k2", "k3"}, "sequential")
	if len(km.Keys()) != 3 {
		t.Fatalf("got %d keys, want 3", len(km.Keys()))
	}
}

func TestKeyManagerSequential(t *testing.T) {
	km := newKeyManager([]string{"k1", "k2", "k3"}, "sequential")
	// 依次返回 k1, k1, k1（始终优先第一个）
	for i := 0; i < 3; i++ {
		key, retry := km.Next("gpt-4")
		if key != "k1" {
			t.Fatalf("iter %d: got key %q, want k1", i, key)
		}
		if retry != 0 {
			t.Fatalf("iter %d: retry = %v, want 0", i, retry)
		}
	}
}

func TestKeyManagerSequentialCooling(t *testing.T) {
	km := newKeyManager([]string{"k1", "k2", "k3"}, "sequential")
	km.MarkRateLimited("k1", "gpt-4", 100*time.Second)

	// k1 冷却中，应返回 k2
	key, _ := km.Next("gpt-4")
	if key != "k2" {
		t.Fatalf("got key %q, want k2 (k1 cooling)", key)
	}

	// 冷却 k2
	km.MarkRateLimited("k2", "gpt-4", 100*time.Second)
	key, _ = km.Next("gpt-4")
	if key != "k3" {
		t.Fatalf("got key %q, want k3 (k1,k2 cooling)", key)
	}

	// 全部冷却
	km.MarkRateLimited("k3", "gpt-4", 100*time.Second)
	key, retry := km.Next("gpt-4")
	if key != "" {
		t.Fatalf("got key %q, want empty (all cooling)", key)
	}
	if retry <= 0 {
		t.Fatalf("retry = %v, want > 0", retry)
	}
}

func TestKeyManagerRoundRobin(t *testing.T) {
	km := newKeyManager([]string{"k1", "k2", "k3"}, "roundrobin")

	seen := make(map[string]int)
	for i := 0; i < 9; i++ {
		key, _ := km.Next("gpt-4")
		seen[key]++
	}
	// 每个 key 应该出现 3 次
	for _, k := range []string{"k1", "k2", "k3"} {
		if seen[k] != 3 {
			t.Fatalf("key %s seen %d times, want 3", k, seen[k])
		}
	}
}

func TestKeyManagerRoundRobinSkipCooling(t *testing.T) {
	km := newKeyManager([]string{"k1", "k2", "k3"}, "roundrobin")
	km.MarkRateLimited("k1", "gpt-4", 100*time.Second)
	km.MarkRateLimited("k2", "gpt-4", 100*time.Second)

	// 跳过 k1, k2，第一次返回 k3
	key, _ := km.Next("gpt-4")
	if key != "k3" {
		t.Fatalf("got key %q, want k3", key)
	}
	// 游标应在 k3 之后，下一个应是 k3（因为 k1,k2 冷却中）
	key, _ = km.Next("gpt-4")
	if key != "k3" {
		t.Fatalf("round 2: got key %q, want k3", key)
	}
}

func TestKeyManagerPerModelCooldown(t *testing.T) {
	km := newKeyManager([]string{"k1", "k2"}, "sequential")
	km.MarkRateLimited("k1", "gpt-4", 100*time.Second)

	// gpt-4 模型 k1 冷却，应返回 k2
	key, _ := km.Next("gpt-4")
	if key != "k2" {
		t.Fatalf("gpt-4: got %q, want k2", key)
	}

	// 其他模型（claude-3）k1 应可用
	key, _ = km.Next("claude-3")
	if key != "k1" {
		t.Fatalf("claude-3: got %q, want k1", key)
	}
}

func TestKeyManagerCooldownExpiry(t *testing.T) {
	km := newKeyManager([]string{"k1"}, "sequential")
	km.MarkRateLimited("k1", "gpt-4", 50*time.Millisecond)

	key, _ := km.Next("gpt-4")
	if key != "" {
		t.Fatalf("before expiry: got key %q, want empty", key)
	}

	time.Sleep(60 * time.Millisecond)
	key, _ = km.Next("gpt-4")
	if key != "k1" {
		t.Fatalf("after expiry: got key %q, want k1", key)
	}
}

func TestKeyManagerEmptyKeys(t *testing.T) {
	km := newKeyManager(nil, "sequential")
	key, _ := km.Next("gpt-4")
	if key != "" {
		t.Fatalf("got key %q, want empty", key)
	}
}

func TestKeyManagerDedup(t *testing.T) {
	km := newKeyManager([]string{"k1", "k1", "k2"}, "sequential")
	keys := km.Keys()
	if len(keys) != 2 || keys[0] != "k1" || keys[1] != "k2" {
		t.Fatalf("got %v, want [k1 k2]", keys)
	}
}

func TestRetryAfterDuration(t *testing.T) {
	// 秒数
	if d := retryAfterDuration("30", 10*time.Second); d != 30*time.Second {
		t.Fatalf("got %v, want 30s", d)
	}
	// 空 → 默认
	if d := retryAfterDuration("", 30*time.Second); d != 30*time.Second {
		t.Fatalf("got %v", d)
	}
	// 非法 → 默认
	if d := retryAfterDuration("abc", 30*time.Second); d != 30*time.Second {
		t.Fatalf("got %v", d)
	}
}

func TestKeyManagerClearAll(t *testing.T) {
	km := newKeyManager([]string{"k1"}, "sequential")
	km.MarkRateLimited("k1", "gpt-4", 100*time.Second)
	km.ClearAll()
	key, _ := km.Next("gpt-4")
	if key != "k1" {
		t.Fatalf("after clear: got %q, want k1", key)
	}
}