// SOCKS5 代理服务端测试：无认证 / 用户名密码认证 / 隧道转发。
package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startSocksServer 启动一个 SOCKS5 服务器，返回监听地址。
func startSocksServer(t *testing.T, creds func(u, p string) bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ss := newSOCKSServer(creds, nil)
	go ss.Serve(ln)
	return ln.Addr().String()
}

// socksHTTPGet 通过 SOCKS5 隧道向目标发 HTTP GET。
func socksHTTPGet(t *testing.T, socksAddr, targetAddr string, user, pass string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialSocks5(ctx, socksAddr, user, pass, targetAddr)
	if err != nil {
		t.Fatalf("dialSocks5: %v", err)
	}
	defer conn.Close()

	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: " + targetAddr + "\r\n\r\n"))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestSocks5NoAuth(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("socks-ok"))
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	socksAddr := startSocksServer(t, nil)

	body := socksHTTPGet(t, socksAddr, targetAddr, "", "")
	if body != "socks-ok" {
		t.Fatalf("got %q, want socks-ok", body)
	}
}

func TestSocks5WithAuth(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("auth-ok"))
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	creds := func(u, p string) bool { return u == "user" && p == "pass" }
	socksAddr := startSocksServer(t, creds)

	// 正确凭证
	if body := socksHTTPGet(t, socksAddr, targetAddr, "user", "pass"); body != "auth-ok" {
		t.Fatalf("got %q, want auth-ok", body)
	}

	// 错误凭证
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dialSocks5(ctx, socksAddr, "user", "wrong", targetAddr)
	if err == nil {
		conn.Close()
		t.Fatal("wrong creds should fail")
	}
}

func TestSocks5ConnectFail(t *testing.T) {
	socksAddr := startSocksServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dialSocks5(ctx, socksAddr, "", "", "127.0.0.1:1")
	if err == nil {
		conn.Close()
		t.Fatal("connect to closed port should fail")
	}
}

// TestSocks5ChainThrough tests socks5 backend chaining: socks server -> http backend proxy -> target.
func TestSocks5ChainThrough(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("chained"))
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	// HTTP 后端代理：接受 CONNECT 并建立隧道
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			hj := w.(http.Hijacker)
			client, _, err := hj.Hijack()
			if err != nil {
				return
			}
			dest, err := net.Dial("tcp", r.Host)
			if err != nil {
				client.Close()
				return
			}
			_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() {
				_, _ = io.Copy(dest, client)
				dest.Close()
			}()
			_, _ = io.Copy(client, dest)
			client.Close()
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer backend.Close()
	backendAddr := strings.TrimPrefix(backend.URL, "http://")

	// socks 服务器链经 http backend
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	route, err := parseProxyRoute("http://" + backendAddr)
	if err != nil {
		t.Fatal(err)
	}
	ss := newSOCKSServer(nil, route)
	go ss.Serve(ln)

	body := socksHTTPGet(t, ln.Addr().String(), targetAddr, "", "")
	if body != "chained" {
		t.Fatalf("got %q, want chained", body)
	}
}