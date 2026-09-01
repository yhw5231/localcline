// HTTP 正向代理：支持 CONNECT 隧道与 absolute-form 请求。
// managed 路径（目标 host 匹配上游 host 或 PROXY_MANAGED_ALL）使用 key 选择 + 改写；
// 非 managed 路径做普通正向代理转发。
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ProxyRoute 描述链经的上游代理（普通代理/代理池条目可选的 Backend）。
type ProxyRoute struct {
	Kind string // "http" | "socks5"
	Addr string // host:port
	User string
	Pass string
}

func (r *ProxyRoute) key() string {
	return fmt.Sprintf("%s://%s:%s@%s", r.Kind, r.User, r.Pass, r.Addr)
}

// parseProxyRoute 解析代理路 URL（如 "http://user:pass@host:port" 或 "socks5://host:port"）。
func parseProxyRoute(raw string) (*ProxyRoute, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	kind := u.Scheme
	if kind == "" {
		kind = "http"
	}
	if kind != "http" && kind != "https" && kind != "socks5" && kind != "socks5h" {
		return nil, fmt.Errorf("unsupported proxy scheme %q", kind)
	}
	if kind == "socks5h" {
		kind = "socks5"
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing proxy host")
	}
	user := ""
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return &ProxyRoute{Kind: kind, Addr: u.Host, User: user, Pass: pass}, nil
}

// proxyHandler 是一个正向代理的 http.Handler。
type proxyHandler struct {
	// managed 转发目标（scheme://host），不含 path；拼接请求 path/query 后发给上游。
	upstreamBase string
	managedHost  string
	managedAll   bool

	// 全局 key 模式
	useGlobal bool

	// 固定 key 模式（池条目专用）
	fixedKey string
	localKM  *KeyManager

	// 认证 / 解析
	creds    func(user, pass string) bool
	resolver func(user, pass string) (key string, backend *ProxyRoute, ok bool)

	// 默认后端（非 resolver 时）
	backend *ProxyRoute

	mu  sync.Mutex
	trs map[string]*http.Transport // 按 backend key 缓存 transport
}

func newProxyHandler(upstreamBase, managedHost string, managedAll bool, creds func(u, p string) bool, backend *ProxyRoute) *proxyHandler {
	return &proxyHandler{
		upstreamBase: upstreamBase,
		managedHost:  managedHost,
		managedAll:   managedAll,
		useGlobal:    true,
		creds:        creds,
		backend:      backend,
		trs:          map[string]*http.Transport{},
	}
}

func newPoolProxyHandler(upstreamBase, managedHost string, fixedKey string, backend *ProxyRoute) *proxyHandler {
	km := newKeyManager([]string{fixedKey}, "sequential")
	return &proxyHandler{
		upstreamBase: upstreamBase,
		managedHost:  managedHost,
		managedAll:   true,
		fixedKey:     fixedKey,
		localKM:      km,
		backend:      backend,
		trs:          map[string]*http.Transport{},
	}
}

func newPoolSinglePortHandler(upstreamBase, managedHost string, resolver func(u, p string) (string, *ProxyRoute, bool)) *proxyHandler {
	return &proxyHandler{
		upstreamBase: upstreamBase,
		managedHost:  managedHost,
		managedAll:   true,
		resolver:     resolver,
		trs:          map[string]*http.Transport{},
	}
}

// resolveForRequest 返回该请求使用的 key 与后端。
func (p *proxyHandler) resolveForRequest(r *http.Request) (key string, backend *ProxyRoute, ok bool) {
	if p.resolver != nil {
		user, pass, aok := proxyBasicAuth(r)
		if !aok {
			return "", nil, false
		}
		return p.resolver(user, pass)
	}
	return p.fixedKey, p.backend, true
}

func (p *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 认证
	if p.creds != nil {
		user, pass, ok := proxyBasicAuth(r)
		if !ok || !p.creds(user, pass) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="proxy"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
	}
	// 当 resolver 存在时，认证集成在 resolver 中
	if p.resolver != nil {
		user, pass, ok := proxyBasicAuth(r)
		if !ok {
			w.Header().Set("Proxy-Authenticate", `Basic realm="proxy"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		key, backend, ok := p.resolver(user, pass)
		if !ok {
			w.Header().Set("Proxy-Authenticate", `Basic realm="proxy"`)
			http.Error(w, "invalid proxy credentials", http.StatusProxyAuthRequired)
			return
		}
		if r.Method == http.MethodConnect {
			p.handleConnect(w, r, backend)
			return
		}
		p.handleForward(w, r, key, backend)
		return
	}

	if r.Method == http.MethodConnect {
		p.handleConnect(w, r, p.backend)
		return
	}
	p.handleForward(w, r, p.fixedKey, p.backend)
}

func (p *proxyHandler) handleConnect(w http.ResponseWriter, r *http.Request, backend *ProxyRoute) {
	dest := r.Host
	if dest == "" {
		http.Error(w, "missing CONNECT target", http.StatusBadRequest)
		return
	}
	up, err := p.dialConn(context.Background(), "tcp", dest, backend)
	if err != nil {
		http.Error(w, "cannot connect to "+dest+": "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}
	_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = buf.Flush()
	go func() {
		_, _ = io.Copy(up, clientConn)
		up.Close()
	}()
	_, _ = io.Copy(clientConn, up)
	_ = clientConn.Close()
}

func (p *proxyHandler) handleForward(w http.ResponseWriter, r *http.Request, key string, backend *ProxyRoute) {
	if !r.URL.IsAbs() {
		http.Error(w, "forward proxy requires absolute-form request URI", http.StatusBadRequest)
		return
	}
	managed := p.isManaged(r.URL.Hostname())
	if managed {
		p.managedForward(w, r, key, backend)
		return
	}
	p.plainForward(w, r, backend)
}

func (p *proxyHandler) isManaged(host string) bool {
	return p.managedAll || (p.managedHost != "" && strings.EqualFold(host, p.managedHost))
}

// managedForward 带 key 选择 + 冷却重试 + 改写转发的 managed 路径。
func (p *proxyHandler) managedForward(w http.ResponseWriter, r *http.Request, key string, backend *ProxyRoute) {
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	stream := isStreamRequest(rawBody)
	model := extractModel(rawBody)

	targetURL := p.upstreamBase + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	if p.useGlobal {
		forwardWithKeys(w, rawBody, stream, model, targetURL,
			func(m string) (string, time.Duration) { return keyMgr.Next(m) },
			func(k, m string, d time.Duration) { keyMgr.MarkRateLimited(k, m, d) },
			cfg.MaxKeyTries)
		return
	}

	// 固定 key（池条目）
	if key != "" {
		if p.localKM != nil {
			if _, retry := p.localKM.Next(model); retry > 0 {
				secs := int64(retry.Seconds()) + 1
				w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
				writeJSONError(w, http.StatusTooManyRequests, "key cooling for "+model, "rate_limited")
				return
			}
		}
		upResp, err := doUpstream(targetURL, rawBody, "Bearer "+key, nil)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "upstream_error")
			return
		}
		if upResp.StatusCode == http.StatusTooManyRequests {
			d := retryAfterDuration(upResp.Header.Get("Retry-After"), cfg.RateLimitCooldown)
			if p.localKM != nil {
				p.localKM.MarkRateLimited(key, model, d)
			}
			upResp.Body.Close()
			writeJSONError(w, http.StatusTooManyRequests, "upstream key rate limited for "+model, "rate_limited")
			return
		}
		serveUpstreamResponse(w, upResp, stream)
		return
	}

	// 无 key
	upResp, err := doUpstream(targetURL, rawBody, "", nil)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "upstream_error")
		return
	}
	serveUpstreamResponse(w, upResp, stream)
}

// plainForward 普通正向代理转发（不改写、不选 key）。
func (p *proxyHandler) plainForward(w http.ResponseWriter, r *http.Request, backend *ProxyRoute) {
	target := *r.URL
	req := r.Clone(r.Context())
	req.RequestURI = ""
	req.URL = &target
	req.Host = target.Host
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Connection")
	req.Header.Del("Keep-Alive")
	req.Header.Del("Transfer-Encoding")

	cl := p.clientFor(backend)
	resp, err := cl.Do(req)
	if err != nil {
		http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *proxyHandler) clientFor(backend *ProxyRoute) *http.Client {
	tr := p.transportFor(backend)
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func (p *proxyHandler) transportFor(backend *ProxyRoute) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := ""
	if backend != nil {
		k = backend.key()
	}
	if p.trs == nil {
		p.trs = map[string]*http.Transport{}
	}
	if tr, ok := p.trs[k]; ok {
		return tr
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	if backend != nil {
		switch backend.Kind {
		case "http":
			proxyURL := &url.URL{
				Scheme: "http",
				Host:   backend.Addr,
				User:   url.UserPassword(backend.User, backend.Pass),
			}
			tr.Proxy = http.ProxyURL(proxyURL)
		case "socks5":
			tr.Proxy = nil
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialSocks5(ctx, backend.Addr, backend.User, backend.Pass, addr)
			}
		}
	}
	p.trs[k] = tr
	return tr
}

func (p *proxyHandler) dialConn(ctx context.Context, network, addr string, backend *ProxyRoute) (net.Conn, error) {
	if backend == nil {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	switch backend.Kind {
	case "http":
		return dialViaHTTPProxy(ctx, backend, addr)
	case "socks5":
		return dialSocks5(ctx, backend.Addr, backend.User, backend.Pass, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// proxyBasicAuth 提取 Proxy-Authorization Basic 的用户名密码。
func proxyBasicAuth(r *http.Request) (user, pass string, ok bool) {
	h := r.Header.Get("Proxy-Authorization")
	const prefix = "Basic "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	s := string(raw)
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// dialViaHTTPProxy 通过 HTTP 正向代理建立到目标的 TCP 连接（CONNECT 隧道）。
func dialViaHTTPProxy(ctx context.Context, route *ProxyRoute, target string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", route.Addr)
	if err != nil {
		return nil, fmt.Errorf("connect to proxy %s: %w", route.Addr, err)
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if route.User != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(route.User + ":" + route.Pass))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read CONNECT headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if !strings.Contains(statusLine, " 200 ") {
		conn.Close()
		return nil, fmt.Errorf("CONNECT to %s returned: %s", target, strings.TrimSpace(statusLine))
	}
	return &bufferedConn{Conn: conn, reader: br}, nil
}

// bufferedConn 包装 net.Conn，提供一个 bufio.Reader 用于读取已缓冲的数据。
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}