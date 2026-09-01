// 代理池测试：条目解析、单端口凭证区分、端口范围自动绑定。
package main

import (
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestParsePoolEntries(t *testing.T) {
	ents := parsePoolEntries("user1:pass1:key1,user2:pass2:key2@http://backend:3128,badentry")
	if len(ents) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(ents), ents)
	}
	e0 := ents[0]
	if e0.User != "user1" || e0.Pass != "pass1" || e0.Key != "key1" || e0.Backend != nil {
		t.Fatalf("entry0 = %+v", e0)
	}
	e1 := ents[1]
	if e1.User != "user2" || e1.Pass != "pass2" || e1.Key != "key2" {
		t.Fatalf("entry1 = %+v", e1)
	}
	if e1.Backend == nil || e1.Backend.Kind != "http" || e1.Backend.Addr != "backend:3128" {
		t.Fatalf("entry1 backend = %+v", e1.Backend)
	}
}

// TestProxyPoolSinglePort 验证单端口模式：不同凭证使用不同 key。
func TestProxyPoolSinglePort(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		// 把收到的 auth 原样返回
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"reasoning":"r1","content":"c"}}],"echo_auth":"` + auth + `"}`))
	}))
	defer upstream.Close()

	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("PROXY_POOL_PORT", "0") // 0 表示测试内不启动真实端口，直接测 handler
	t.Setenv("PROXY_POOL_ENTRIES", "ua:pa:key-a,ub:pb:key-b")
	t.Setenv("LOGIN_REQUIRED", "false")
	reloadConfig()

	ph := newPoolSinglePortHandler(extractSchemeHost(upstream.URL), "",
		func(user, pass string) (string, *ProxyRoute, bool) {
			for _, e := range cfg.PoolEntries {
				if e.User == user && e.Pass == pass {
					return e.Key, e.Backend, true
				}
			}
			return "", nil, false
		})

	// 凭证 ua:pa → key-a
	req := httptest.NewRequest(http.MethodPost, "http://api.cline.bot/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":false}`))
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("ua:pa")))
	rr := httptest.NewRecorder()
	ph.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"echo_auth":"Bearer key-a"`) {
		t.Fatalf("expected key-a auth: %s", rr.Body.String())
	}

	// 凭证 ub:pb → key-b
	req2 := httptest.NewRequest(http.MethodPost, "http://api.cline.bot/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":false}`))
	req2.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("ub:pb")))
	rr2 := httptest.NewRecorder()
	ph.ServeHTTP(rr2, req2)
	if !strings.Contains(rr2.Body.String(), `"echo_auth":"Bearer key-b"`) {
		t.Fatalf("expected key-b auth: %s", rr2.Body.String())
	}

	// 错误凭证 → 407
	req3 := httptest.NewRequest(http.MethodPost, "http://api.cline.bot/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	req3.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("nobody:nothing")))
	rr3 := httptest.NewRecorder()
	ph.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407", rr3.Code)
	}
}

// TestProxyPoolPortRange 验证端口范围模式：自动检测空闲端口并绑定，每个条目独立端口。
func TestProxyPoolPortRange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	// 找一个空闲端口作为范围起点
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	startPort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	t.Setenv("UPSTREAM_URL", upstream.URL)
	t.Setenv("PROXY_POOL_RANGE", strconv.Itoa(startPort)+"-"+strconv.Itoa(startPort+5))
	t.Setenv("PROXY_POOL_ENTRIES", "ua:pa:key-a,ub:pb:key-b")
	t.Setenv("LOGIN_REQUIRED", "false")
	reloadConfig()

	pp := newProxyPool(cfg)
	listeners, err := pp.StartListeners()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()

	if len(listeners) != 2 {
		t.Fatalf("got %d listeners, want 2", len(listeners))
	}
	// 两个端口必须不同
	port0 := listeners[0].Addr().(*net.TCPAddr).Port
	port1 := listeners[1].Addr().(*net.TCPAddr).Port
	if port0 == port1 {
		t.Fatal("two entries should bind different ports")
	}
	if port0 < startPort || port0 > startPort+5 || port1 < startPort || port1 > startPort+5 {
		t.Fatalf("ports %d,%d outside range %d-%d", port0, port1, startPort, startPort+5)
	}

	// 通过端口 0 访问（该条目 key-a，无认证要求？—— 条目配置了用户密码，需要认证）
	// 这里条目带用户密码，但 newPoolProxyHandler 会设置 creds，需要带认证访问。
	// 用 http.Client 走绝对路径（managed 模式，managedAll=true）
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port0) + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 条目 ua:pa 配置了密码，未带凭证应 407
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407 (entry requires creds)", resp.StatusCode)
	}
}

// TestProxyPoolRangeExhausted 验证范围不够时返回错误。
func TestProxyPoolRangeExhausted(t *testing.T) {
	// 占满范围：直接用通配端口占住两个端口
	l1, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	p1 := l1.Addr().(*net.TCPAddr).Port
	l2, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	p2 := l2.Addr().(*net.TCPAddr).Port

	t.Setenv("PROXY_POOL_RANGE", strconv.Itoa(p1)+"-"+strconv.Itoa(p2))
	t.Setenv("PROXY_POOL_ENTRIES", "ua:pa:key-a,ub:pb:key-b")
	reloadConfig()

	pp := newProxyPool(cfg)
	listeners, err := pp.StartListeners()
	if err == nil {
		for _, l := range listeners {
			l.Close()
		}
		t.Fatal("expected error when range exhausted")
	}
}