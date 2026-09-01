// HTTP 正向代理测试：managed 转发、普通转发、CONNECT 隧道、认证。
package main

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startTestUpstream 返回一个模拟上游，校验 Authorization 并返回可改写响应。
func startTestUpstream(t *testing.T, wantAuth string) (*httptest.Server, *[]string) {
	var gotAuths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuths = append(gotAuths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"reasoning":"think","content":"hi"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotAuths
}

// TestProxyManagedForward 验证 managed 路径：带 key 选择 + 改写。
func TestProxyManagedForward(t *testing.T) {
	upstream, gotAuth := startTestUpstream(t, "")

	t.Setenv("UPSTREAM_KEYS", "sk-a,sk-b")
	t.Setenv("LOGIN_REQUIRED", "false")
	reloadConfig()

	// managedHost 用 hostname（不含端口），与生产代码一致
	ph := newProxyHandler(extractSchemeHost(upstream.URL), hostOnly(upstream.URL), false, nil, nil)

	body := `{"model":"gpt-4","stream":false}`
	req := httptest.NewRequest(http.MethodPost, upstream.URL+"/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	ph.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// managed 路径应带 Authorization（首个 key sk-a）
	if len(*gotAuth) == 0 {
		t.Fatal("upstream received no Authorization")
	}
	if (*gotAuth)[0] != "Bearer sk-a" {
		t.Fatalf("got auth %q, want Bearer sk-a", (*gotAuth)[0])
	}
	// 改写
	if !strings.Contains(rr.Body.String(), `"reasoning_content":"think"`) {
		t.Fatalf("missing rewrite: %s", rr.Body.String())
	}
}

// TestProxyPlainForward 验证普通正向代理转发（非 managed 目标）。
func TestProxyPlainForward(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain-ok"))
	}))
	defer target.Close()

	host := strings.TrimPrefix(target.URL, "http://")
	ph := newProxyHandler("http://unrelated.invalid", "otherhost", false, nil, nil)

	req := httptest.NewRequest(http.MethodGet, target.URL+"/some/path", nil)
	rr := httptest.NewRecorder()
	ph.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "plain-ok" {
		t.Fatalf("body = %q, want plain-ok", rr.Body.String())
	}
	if host == "" {
		t.Fatal("unexpected")
	}
}

// TestProxyManagedAll 验证 PROXY_MANAGED_ALL=true 时任意 host 都走 managed。
func TestProxyManagedAll(t *testing.T) {
	upstream, gotAuth := startTestUpstream(t, "")
	t.Setenv("UPSTREAM_KEYS", "sk-x")
	t.Setenv("LOGIN_REQUIRED", "false")
	reloadConfig()

	ph := newProxyHandler(extractSchemeHost(upstream.URL), "ignored", true, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "http://arbitrary.invalid/v1/chat/completions", strings.NewReader(`{"model":"x","stream":false}`))
	rr := httptest.NewRecorder()
	ph.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(*gotAuth) == 0 || (*gotAuth)[0] != "Bearer sk-x" {
		t.Fatalf("got auths %v, want [Bearer sk-x]", *gotAuth)
	}
}

// TestProxyAuthRequired 验证要求 Proxy-Authorization 认证。
func TestProxyAuthRequired(t *testing.T) {
	ph := newProxyHandler("http://x.invalid", "", true,
		func(u, p string) bool { return u == "user" && p == "pass" }, nil)

	req := httptest.NewRequest(http.MethodGet, "http://x.invalid/v1/chat/completions", nil)
	rr := httptest.NewRecorder()
	ph.ServeHTTP(rr, req)
	if rr.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407", rr.Code)
	}

	// 正确凭证
	req2 := httptest.NewRequest(http.MethodGet, "http://x.invalid/v1/chat/completions", nil)
	req2.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")))
	rr2 := httptest.NewRecorder()
	ph.ServeHTTP(rr2, req2)
	if rr2.Code == http.StatusProxyAuthRequired {
		t.Fatalf("correct creds should not 407")
	}
}

// TestProxyBasicAuth 验证 proxyBasicAuth 解析。
func TestProxyBasicAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("u:p")))
	u, p, ok := proxyBasicAuth(req)
	if !ok || u != "u" || p != "p" {
		t.Fatalf("got (%q,%q,%v)", u, p, ok)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Proxy-Authorization", "Bearer xxx")
	if _, _, ok := proxyBasicAuth(req2); ok {
		t.Fatal("Bearer should not parse as Basic")
	}
}

// TestProxyConnectTunnel 验证 CONNECT 隧道。
func TestProxyConnectTunnel(t *testing.T) {
	// 目标服务：回显
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("echo-hello"))
	}))
	defer echo.Close()
	echoHost := strings.TrimPrefix(echo.URL, "http://")

	// 启动真实代理 listener（CONNECT 需要 Hijack）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ph := newProxyHandler("http://x.invalid", "", false, nil, nil)
	srv := &http.Server{Handler: ph}
	go srv.Serve(ln)
	defer srv.Close()

	// 连接代理，发起 CONNECT
	proxyAddr := ln.Addr().String()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("CONNECT " + echoHost + " HTTP/1.1\r\nHost: " + echoHost + "\r\n\r\n"))
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	// 消费 CONNECT 响应头直到空行
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	// 通过隧道发 HTTP 请求
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: " + echoHost + "\r\n\r\n"))
	// 读取响应直到 body
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "echo-hello" {
		t.Fatalf("tunnel body = %q, want echo-hello", body)
	}
}

// TestProxyConnectNoAuthPassthrough 验证 CONNECT 到不可达目标返回 502。
func TestProxyConnectFail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ph := newProxyHandler("http://x.invalid", "", false, nil, nil)
	srv := &http.Server{Handler: ph}
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n"))
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "502") {
		t.Fatalf("CONNECT status = %q, want 502", status)
	}
}

// TestParseProxyRoute 验证代理路由解析。
func TestParseProxyRoute(t *testing.T) {
	r, err := parseProxyRoute("socks5://u:p@1.2.3.4:1080")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "socks5" || r.Addr != "1.2.3.4:1080" || r.User != "u" || r.Pass != "p" {
		t.Fatalf("got %+v", r)
	}

	r2, err := parseProxyRoute("http://proxy.example.com:8080")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Kind != "http" || r2.Addr != "proxy.example.com:8080" {
		t.Fatalf("got %+v", r2)
	}

	if _, err := parseProxyRoute("ftp://x"); err == nil {
		t.Fatal("ftp should be rejected")
	}
}

// TestExtractSchemeHost 验证 scheme://host 提取。
func TestExtractSchemeHost(t *testing.T) {
	if got := extractSchemeHost("https://api.cline.bot/api/v1/chat/completions"); got != "https://api.cline.bot" {
		t.Fatalf("got %q", got)
	}
	if got := extractSchemeHost("http://localhost:8080/x"); got != "http://localhost:8080" {
		t.Fatalf("got %q", got)
	}
}