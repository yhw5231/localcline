// 可观察性测试：请求记录环形缓冲与用量聚合。
package main

import (
	"net/http"
	"net/http/httptest"
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
