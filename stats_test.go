// 可观察性测试：请求记录环形缓冲、用量聚合、statsHandler 与 admin 端点。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestLogAddSnapshot(t *testing.T) {
	l := newRequestLog(3)
	l.Add(RequestRecord{ID: "a"})
	l.Add(RequestRecord{ID: "b"})
	l.Add(RequestRecord{ID: "c"})
	l.Add(RequestRecord{ID: "d"}) // 覆盖 a

	recs := l.Snapshot()
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	if recs[0].ID != "d" || recs[1].ID != "c" || recs[2].ID != "b" {
		t.Fatalf("order wrong (newest first): %+v", recs)
	}
}

func TestRequestLogSmallCap(t *testing.T) {
	l := newRequestLog(1)
	l.Add(RequestRecord{ID: "x"})
	l.Add(RequestRecord{ID: "y"})
	recs := l.Snapshot()
	if len(recs) != 1 || recs[0].ID != "y" {
		t.Fatalf("got %+v", recs)
	}
}

func TestUsageStatsRecord(t *testing.T) {
	s := newUsageStats()
	now := time.Now()
	s.Record(RequestRecord{Status: 200, BytesOut: 100, DurationMs: 5, Time: now, User: "alice", Model: "m1", Key: "sk-a"})
	s.Record(RequestRecord{Status: 500, BytesOut: 20, DurationMs: 3, Time: now, User: "alice", Model: "m1", Key: "sk-a"})
	s.Record(RequestRecord{Status: 200, BytesOut: 50, DurationMs: 2, Time: now, User: "bob", Model: "m2", Key: "sk-b"})

	if s.Totals.Requests != 3 || s.Totals.Errors != 1 || s.Totals.BytesOut != 170 {
		t.Fatalf("totals = %+v", s.Totals)
	}
	if u := s.ByUser["alice"]; u == nil || u.Requests != 2 || u.Errors != 1 {
		t.Fatalf("alice = %+v", u)
	}
	if m := s.ByModel["m1"]; m == nil || m.Requests != 2 || m.Errors != 1 {
		t.Fatalf("m1 = %+v", m)
	}
	if k := s.ByKey["sk-b"]; k == nil || k.Requests != 1 {
		t.Fatalf("sk-b = %+v", k)
	}
}

func TestMaskKey(t *testing.T) {
	if maskKey("sk-12345678abcd") != "sk-1****abcd" {
		t.Fatalf("got %q", maskKey("sk-12345678abcd"))
	}
	if maskKey("short") != "****" {
		t.Fatalf("got %q", maskKey("short"))
	}
}

func TestResponseRecorder(t *testing.T) {
	inner := httptest.NewRecorder()
	rr := &responseRecorder{ResponseWriter: inner}
	rr.WriteHeader(http.StatusCreated)
	_, _ = rr.Write([]byte("hello"))
	if rr.status != http.StatusCreated || rr.bytes != 5 {
		t.Fatalf("status=%d bytes=%d", rr.status, rr.bytes)
	}
}

// TestStatsHandlerRecordsRequest 验证 statsHandler 记录一次成功请求。
func TestStatsHandlerRecordsRequest(t *testing.T) {
	reloadConfig()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("UPSTREAM_KEYS", "sk-secret-1234")
	t.Setenv("LOGIN_REQUIRED", "false")
	t.Setenv("USAGE_DB_PATH", "") // 内存模式，避免落盘
	reloadConfig()

	initStats() // 清空
	body := `{"model":"deepseek/model-x","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	statsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	recs := reqLog.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Status != http.StatusOK || rec.Method != http.MethodPost || rec.Path != "/v1/chat/completions" {
		t.Fatalf("rec = %+v", rec)
	}
	if rec.Model != "deepseek/model-x" {
		t.Fatalf("model = %q", rec.Model)
	}
	if rec.Key != "sk-s****1234" {
		t.Fatalf("key = %q (should be masked)", rec.Key)
	}
	if usageStat.Totals.Requests != 1 {
		t.Fatalf("totals = %+v", usageStat.Totals)
	}
}

// TestAdminStatsEndpoint 验证 /admin/stats 需要管理员。
func TestAdminStatsEndpoint(t *testing.T) {
	reloadConfig()
	initStats()
	// 注入两条记录
	reqLog.Add(RequestRecord{Status: 200, Time: time.Now(), User: "admin", Model: "m1", Key: "sk-a", DurationMs: 4})
	usageStat.Record(reqLog.Snapshot()[0])

	// 未登录 → 401
	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}

	// 普通用户（登录但非 admin）→ 403
	userToken, _ := issueToken("alice")
	req2 := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	req2.Header.Set("Authorization", "Bearer "+userToken)
	rr2 := httptest.NewRecorder()
	handler(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr2.Code)
	}

	// 管理员 → 200
	adminToken, _ := issueToken("admin")
	req3 := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	req3.Header.Set("Authorization", "Bearer "+adminToken)
	rr3 := httptest.NewRecorder()
	handler(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr3.Code, rr3.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr3.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["totals"] == nil {
		t.Fatal("missing totals")
	}
}

// TestAdminRequestsEndpoint 验证 /admin/requests 返回记录。
func TestAdminRequestsEndpoint(t *testing.T) {
	reloadConfig()
	initStats()
	for i := 0; i < 5; i++ {
		rec := RequestRecord{Status: 200, Time: time.Now(), Method: "POST", Path: "/v1/chat/completions", Model: "m1"}
		reqLog.Add(rec)
	}

	adminToken, _ := issueToken("admin")
	req := httptest.NewRequest(http.MethodGet, "/admin/requests?limit=3", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var out struct {
		Total   int             `json:"total"`
		Records []RequestRecord `json:"records"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 5 || len(out.Records) != 3 {
		t.Fatalf("total=%d records=%d", out.Total, len(out.Records))
	}
}

// TestStatsHandlerUnauthorized 验证未授权请求也被记录（401）。
func TestStatsHandlerUnauthorized(t *testing.T) {
	t.Setenv("USAGE_DB_PATH", "")
	reloadConfig()
	initStats()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil) // 无 token
	rr := httptest.NewRecorder()
	statsHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	recs := reqLog.Snapshot()
	if len(recs) != 1 || recs[0].Status != http.StatusUnauthorized {
		t.Fatalf("recs = %+v", recs)
	}
	if usageStat.Totals.Errors != 1 {
		t.Fatalf("errors = %d, want 1", usageStat.Totals.Errors)
	}
}

// TestStatsHandlerLoginRecordsUser 验证登录后请求记录用户。
func TestStatsHandlerLoginRecordsUser(t *testing.T) {
	t.Setenv("USAGE_DB_PATH", "")
	reloadConfig()
	initStats()
	token, _ := issueToken("admin")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()
	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("UPSTREAM_KEYS", "sk-secret-1234")
	reloadConfig()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":false}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	statsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	recs := reqLog.Snapshot()
	if len(recs) != 1 || recs[0].User != "admin" {
		t.Fatalf("recs = %+v", recs)
	}
	if u := usageStat.ByUser["admin"]; u == nil || u.Requests != 1 {
		t.Fatalf("admin usage = %+v", u)
	}
}