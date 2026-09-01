// 用量数据库测试：SQLite 持久化、窗口聚合、条件过滤、token 记录。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain 默认把 USAGE_DB_PATH 置空（内存模式），避免测试在仓库目录落盘
// 或残留未关闭的 SQLite 连接导致文件锁；需要真实文件的用例用 t.Setenv 覆盖。
func TestMain(m *testing.M) {
	if os.Getenv("USAGE_DB_PATH") == "" {
		_ = os.Setenv("USAGE_DB_PATH", "")
	}
	os.Exit(m.Run())
}

func TestUsageDBAppendAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")

	db := newUsageDB(path, 30, 1000)
	defer db.Close()
	db.Append(UsageEvent{Time: time.Now(), User: "alice", Model: "m1", Key: "sk-a",
		PromptTokens: 100, CompletionTokens: 50, Status: 200})
	db.Append(UsageEvent{Time: time.Now(), User: "bob", Model: "m2", Key: "sk-b",
		PromptTokens: 10, CompletionTokens: 5, Status: 500})

	// 重新加载，验证持久化
	db2 := newUsageDB(path, 30, 1000)
	defer db2.Close()
	res := db2.Query(UsageFilter{Window: "all"})
	if res.Requests != 2 {
		t.Fatalf("requests = %d, want 2", res.Requests)
	}
	if res.PromptTokens != 110 || res.CompletionTokens != 55 || res.TotalTokens != 165 {
		t.Fatalf("tokens = %+v", res)
	}
}

func TestUsageDBWindowFilter(t *testing.T) {
	dir := t.TempDir()
	db := newUsageDB(filepath.Join(dir, "usage.db"), 30, 1000)
	defer db.Close()

	now := time.Now()
	// 今天（within today）
	db.Append(UsageEvent{Time: now, User: "alice", Model: "m1", Key: "sk-a", PromptTokens: 100, Status: 200})
	// 昨天（25h 前）
	db.Append(UsageEvent{Time: now.Add(-25 * time.Hour), User: "alice", Model: "m1", Key: "sk-a", PromptTokens: 50, Status: 200})
	// 8 天前
	db.Append(UsageEvent{Time: now.Add(-8 * 24 * time.Hour), User: "bob", Model: "m2", Key: "sk-b", PromptTokens: 30, Status: 200})

	if res := db.Query(UsageFilter{Window: "today"}); res.Requests != 1 || res.PromptTokens != 100 {
		t.Fatalf("today = %+v", res)
	}
	if res := db.Query(UsageFilter{Window: "24h"}); res.Requests != 1 {
		t.Fatalf("24h = %+v", res)
	}
	if res := db.Query(UsageFilter{Window: "7d"}); res.Requests != 2 {
		t.Fatalf("7d = %+v", res)
	}
	if res := db.Query(UsageFilter{Window: "30d"}); res.Requests != 3 {
		t.Fatalf("30d = %+v", res)
	}
	if res := db.Query(UsageFilter{Window: "all"}); res.Requests != 3 {
		t.Fatalf("all = %+v", res)
	}
}

func TestUsageDBDimensionFilter(t *testing.T) {
	dir := t.TempDir()
	db := newUsageDB(filepath.Join(dir, "usage.db"), 30, 1000)
	defer db.Close()
	now := time.Now()
	db.Append(UsageEvent{Time: now, User: "alice", Model: "m1", Key: "sk-a", PromptTokens: 100, Status: 200})
	db.Append(UsageEvent{Time: now, User: "bob", Model: "m2", Key: "sk-b", PromptTokens: 50, Status: 200})

	res := db.Query(UsageFilter{Window: "all", User: "alice"})
	if res.Requests != 1 || res.PromptTokens != 100 {
		t.Fatalf("by user = %+v", res)
	}
	res = db.Query(UsageFilter{Window: "all", Model: "m2"})
	if res.Requests != 1 {
		t.Fatalf("by model = %+v", res)
	}
	res = db.Query(UsageFilter{Window: "all", Key: "sk-a"})
	if res.Requests != 1 {
		t.Fatalf("by key = %+v", res)
	}
	res = db.Query(UsageFilter{Window: "all", User: "nobody"})
	if res.Requests != 0 {
		t.Fatalf("none = %+v", res)
	}
}

func TestUsageDBBreakdowns(t *testing.T) {
	dir := t.TempDir()
	db := newUsageDB(filepath.Join(dir, "usage.db"), 30, 1000)
	defer db.Close()
	now := time.Now()
	db.Append(UsageEvent{Time: now, User: "alice", Model: "m1", Key: "sk-a", PromptTokens: 100, CompletionTokens: 20, Status: 200})
	db.Append(UsageEvent{Time: now, User: "alice", Model: "m2", Key: "sk-a", PromptTokens: 50, CompletionTokens: 10, Status: 500})
	db.Append(UsageEvent{Time: now, User: "bob", Model: "m1", Key: "sk-b", PromptTokens: 30, Status: 200})

	res := db.Query(UsageFilter{Window: "all"})
	if len(res.ByUser) != 2 || res.ByUser[0].Name != "alice" || res.ByUser[0].Requests != 2 || res.ByUser[0].Errors != 1 {
		t.Fatalf("by_user = %+v", res.ByUser)
	}
	if len(res.ByModel) != 2 || res.ByModel[0].Name != "m1" || res.ByModel[0].Requests != 2 {
		t.Fatalf("by_model = %+v", res.ByModel)
	}
	if len(res.ByKey) != 2 || res.ByKey[0].Name != "sk-a" || res.ByKey[0].TotalTokens != 180 {
		t.Fatalf("by_key = %+v", res.ByKey)
	}
	if res.Errors != 1 {
		t.Fatalf("errors = %d, want 1", res.Errors)
	}
}

func TestUsageDBRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")
	db := newUsageDB(path, 2, 10) // 2 天保留
	defer db.Close()
	now := time.Now()
	db.Append(UsageEvent{Time: now, User: "a", Model: "m", Key: "k", Status: 200})
	db.Append(UsageEvent{Time: now.Add(-3 * 24 * time.Hour), User: "b", Model: "m", Key: "k", Status: 200}) // 过期

	// 重新加载：过期事件应被丢弃
	db2 := newUsageDB(path, 2, 10)
	defer db2.Close()
	if res := db2.Query(UsageFilter{Window: "all"}); res.Requests != 1 {
		t.Fatalf("requests = %d, want 1", res.Requests)
	}
}

func TestUsageDBCompactOnOverflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")
	db := newUsageDB(path, 30, 5) // 上限 5
	defer db.Close()
	now := time.Now()
	for i := 0; i < 10; i++ {
		db.Append(UsageEvent{Time: now, User: "u", Model: "m", Key: "k", Status: 200})
	}
	db.Cleanup() // 触发压缩
	if n := db.Count(); n > 5 {
		t.Fatalf("count = %d, want <= 5 after compact", n)
	}
}

// TestParseUsageJSON 验证 usage 解析。
func TestParseUsageJSON(t *testing.T) {
	p, c := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":12,"completion_tokens":34}}`))
	if p != 12 || c != 34 {
		t.Fatalf("got (%d,%d)", p, c)
	}
	p, c = parseUsageJSON([]byte(`{"choices":[]}`))
	if p != 0 || c != 0 {
		t.Fatalf("no usage should be 0: (%d,%d)", p, c)
	}
}

// TestParseUsageFromFrame 验证流式帧 usage 解析。
func TestParseUsageFromFrame(t *testing.T) {
	frame := []byte("data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":8}}\n\n")
	p, c, ok := parseUsageFromFrame(frame)
	if !ok || p != 7 || c != 8 {
		t.Fatalf("got (%d,%d,%v)", p, c, ok)
	}
	p, c, ok = parseUsageFromFrame([]byte("data: [DONE]\n\n"))
	if ok {
		t.Fatal("[DONE] should not parse usage")
	}
}

// TestUsageTokensRecordedInStats 验证 statsHandler 记录 token 用量。
func TestUsageTokensRecordedInStats(t *testing.T) {
	t.Setenv("USAGE_DB_PATH", "")
	reloadConfig()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":22,"completion_tokens":33}}`))
	}))
	defer upstream.Close()
	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("UPSTREAM_KEYS", "sk-secret-1234")
	t.Setenv("LOGIN_REQUIRED", "false")
	reloadConfig()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":false}`))
	rr := httptest.NewRecorder()
	statsHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}

	res := usageDB.Query(UsageFilter{Window: "all"})
	if res.Requests != 1 || res.PromptTokens != 22 || res.CompletionTokens != 33 {
		t.Fatalf("usage = %+v", res)
	}
	// key 记录原始值
	if len(res.ByKey) != 1 || res.ByKey[0].Name != "sk-secret-1234" {
		t.Fatalf("by_key = %+v", res.ByKey)
	}
}

// TestUsageStreamTokensRecorded 验证流式响应末尾 chunk 的 usage 也被记录。
func TestUsageStreamTokensRecorded(t *testing.T) {
	t.Setenv("USAGE_DB_PATH", "")
	reloadConfig()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":10}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("UPSTREAM_KEYS", "sk-secret-1234")
	t.Setenv("LOGIN_REQUIRED", "false")
	reloadConfig()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	rr := httptest.NewRecorder()
	statsHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}

	res := usageDB.Query(UsageFilter{Window: "all"})
	if res.Requests != 1 || res.PromptTokens != 9 || res.CompletionTokens != 10 {
		t.Fatalf("stream usage = %+v", res)
	}
	if len(res.ByKey) != 1 || res.ByKey[0].Name != "sk-secret-1234" {
		t.Fatalf("by_key = %+v", res.ByKey)
	}
}

// TestAdminUsageEndpoint 验证 /admin/usage 端点。
func TestAdminUsageEndpoint(t *testing.T) {
	t.Setenv("USAGE_DB_PATH", "")
	reloadConfig()

	usageDB.Append(UsageEvent{Time: time.Now(), User: "alice", Model: "m1", Key: "sk-secret-1234", PromptTokens: 5, CompletionTokens: 6, Status: 200})

	adminToken, _ := issueToken("admin")
	req := httptest.NewRequest(http.MethodGet, "/admin/usage?window=all", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out UsageResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Requests != 1 || out.PromptTokens != 5 {
		t.Fatalf("out = %+v", out)
	}
	// by_key 应脱敏
	if len(out.ByKey) != 1 || out.ByKey[0].Name != "sk-s****1234" {
		t.Fatalf("by_key = %+v", out.ByKey)
	}

	// 非法 window → 400
	req2 := httptest.NewRequest(http.MethodGet, "/admin/usage?window=bogus", nil)
	req2.Header.Set("Authorization", "Bearer "+adminToken)
	rr2 := httptest.NewRecorder()
	handler(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr2.Code)
	}

	// 普通用户 → 403
	userToken, _ := issueToken("alice")
	req3 := httptest.NewRequest(http.MethodGet, "/admin/usage", nil)
	req3.Header.Set("Authorization", "Bearer "+userToken)
	rr3 := httptest.NewRecorder()
	handler(rr3, req3)
	if rr3.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr3.Code)
	}
}

// TestUsageDBEmptyPath 验证空路径为纯内存模式。
func TestUsageDBEmptyPath(t *testing.T) {
	db := newUsageDB("", 30, 100)
	db.Append(UsageEvent{Time: time.Now(), User: "a", Model: "m", Key: "k", Status: 200})
	if res := db.Query(UsageFilter{Window: "all"}); res.Requests != 1 {
		t.Fatalf("res = %+v", res)
	}
}