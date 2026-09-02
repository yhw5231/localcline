// 路由引擎测试：故障转移（429→下一 key）、冷却跳过、鉴权失败冷却、
// ipv6pool 换 IP 联动（经真实 SOCKS5 stub 隧道）、reasoning 改写开关、模型过滤。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 测试基建 ----

func setupGateway(t *testing.T) {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("USAGE_DB_PATH", "") // :memory:
	t.Setenv("GW_KEY_AUTH", "true")
	t.Setenv("RATE_LIMIT_COOLDOWN", "60s")
	t.Setenv("AUTH_FAIL_COOLDOWN", "60s")
	t.Setenv("SERVER_ERR_COOLDOWN", "60s")
	t.Setenv("NET_ERR_COOLDOWN", "60s")
	t.Setenv("REQ_LOG_SIZE", "100")
	resetCfgForTest()
	store = newGatewayStore(filepath.Join(t.TempDir(), "gateway.json"))
	if err := store.load(); err != nil {
		t.Fatalf("store load: %v", err)
	}
	leaseMgr = newLeaseManager()
	cool.ClearAll()
}

func mustPutChannel(t *testing.T, ch *Channel) {
	t.Helper()
	if err := store.PutChannel(ch); err != nil {
		t.Fatalf("PutChannel: %v", err)
	}
}

func chatRequest(model string) *http.Request {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":false}`, model)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	return req
}

// upstreamRecorder 记录收到的 Authorization 与请求次数。
type upstreamRecorder struct {
	mu     sync.Mutex
	calls  int
	auths  []string
	status int
	body   string
	srv    *httptest.Server
	URL    string
}

func newUpstream(t *testing.T, status int, body string) *upstreamRecorder {
	t.Helper()
	u := &upstreamRecorder{status: status, body: body}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls++
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		st, b := u.status, u.body
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = io.WriteString(w, b)
	}))
	u.URL = u.srv.URL
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamRecorder) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *upstreamRecorder) lastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.auths) == 0 {
		return ""
	}
	return u.auths[len(u.auths)-1]
}

// fakeSocks5 最小 SOCKS5 服务端：no-auth 握手 + CONNECT 后转发到真实目标。
func startFakeSocks5(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleFakeSocks5(conn)
		}
	}()
	return ln.Addr().String()
}

func handleFakeSocks5(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	// 握手：VER + NMETHODS + METHODS
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	// CONNECT 请求（复用被测代码的地址解析）
	ver := make([]byte, 4)
	if _, err := io.ReadFull(br, ver); err != nil {
		return
	}
	host, err := readSocksAddr(br, ver[3])
	if err != nil {
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])
	target := net.JoinHostPort(host, strconv.Itoa(port))
	up, err := net.Dial("tcp", target)
	if err != nil {
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	go io.Copy(up, br)
	io.Copy(conn, up)
}

// ---- 故障转移 ----

func TestFailoverOn429(t *testing.T) {
	setupGateway(t)
	up1 := newUpstream(t, http.StatusTooManyRequests, `{"error":"rate limited"}`)
	up2 := newUpstream(t, http.StatusOK, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	mustPutChannel(t, &Channel{Name: "c", BaseURL: up1.srv.URL, Enabled: true,
		Keys: []*UpKey{
			{Name: "k1", APIKey: "sk-1", Enabled: true},
			{Name: "k2", APIKey: "sk-2", Enabled: true, BaseURL: up2.srv.URL},
		}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m1"), nil, false, "m1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if up1.count() != 1 || up2.count() != 1 {
		t.Fatalf("calls: up1=%d up2=%d", up1.count(), up2.count())
	}
	if up2.lastAuth() != "Bearer sk-2" {
		t.Fatalf("upstream auth = %q", up2.lastAuth())
	}
	// k1 进入冷却，下一次请求直接打 up2
	rr2 := httptest.NewRecorder()
	forwardChat(rr2, chatRequest("m1"), nil, false, "m1")
	if up1.count() != 1 {
		t.Fatalf("cooling key should be skipped, up1 calls = %d", up1.count())
	}
}

func TestAllRateLimitedReturns429(t *testing.T) {
	setupGateway(t)
	up1 := newUpstream(t, http.StatusTooManyRequests, `err`)
	mustPutChannel(t, &Channel{Name: "c", BaseURL: up1.srv.URL, Enabled: true,
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true}}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m1"), nil, false, "m1")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header")
	}
}

func TestAuthFailCooldown(t *testing.T) {
	setupGateway(t)
	up1 := newUpstream(t, http.StatusUnauthorized, `{"error":"bad key"}`)
	up2 := newUpstream(t, http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`)
	mustPutChannel(t, &Channel{Name: "c", BaseURL: up1.srv.URL, Enabled: true, CooldownScope: "key_model",
		Keys: []*UpKey{
			{Name: "k1", APIKey: "sk-1", Enabled: true},
			{Name: "k2", APIKey: "sk-2", Enabled: true, BaseURL: up2.srv.URL},
		}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m1"), nil, false, "m1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !cool.IsCooling(store.Snapshot().Channels[0].Keys[0].ID, "m1") {
		t.Fatal("401 should mark auth cooldown")
	}
}

func TestNetworkErrorFailover(t *testing.T) {
	setupGateway(t)
	// 关闭的服务器 → 网络错误
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	up2 := newUpstream(t, http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`)
	mustPutChannel(t, &Channel{Name: "c", BaseURL: deadURL, Enabled: true,
		Keys: []*UpKey{
			{Name: "k1", APIKey: "sk-1", Enabled: true},
			{Name: "k2", APIKey: "sk-2", Enabled: true, BaseURL: up2.srv.URL},
		}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m1"), nil, false, "m1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if up2.count() != 1 {
		t.Fatalf("expected failover to up2, calls=%d", up2.count())
	}
}

func TestModelFilterSkipsChannel(t *testing.T) {
	setupGateway(t)
	up := newUpstream(t, http.StatusOK, `{}`)
	mustPutChannel(t, &Channel{Name: "c", BaseURL: up.URL, Enabled: true, Models: []string{"gpt-4o-mini"},
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true}}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("claude-3"), nil, false, "claude-3")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (channel filtered)", rr.Code)
	}
	if up.count() != 0 {
		t.Fatal("filtered channel must not be called")
	}
}

func TestDisabledChannelAndKeySkipped(t *testing.T) {
	setupGateway(t)
	up := newUpstream(t, http.StatusOK, `{}`)
	mustPutChannel(t, &Channel{Name: "c", BaseURL: up.URL, Enabled: false,
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true}}})
	mustPutChannel(t, &Channel{Name: "c2", BaseURL: up.URL, Enabled: true,
		Keys: []*UpKey{{Name: "k2", APIKey: "sk-2", Enabled: false}}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m"), nil, false, "m")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if up.count() != 0 {
		t.Fatal("disabled channel/key must not be called")
	}
}

// ---- reasoning 改写 ----

func TestRewriteToggle(t *testing.T) {
	setupGateway(t)
	streamBody := "data: {\"choices\":[{\"delta\":{\"reasoning\":\"think\"}}]}\n\n" +
		"data: [DONE]\n\n"

	up1 := newUpstreamStream(t, streamBody)
	mustPutChannel(t, &Channel{Name: "rw", BaseURL: up1.URL, Enabled: true, Rewrite: true,
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true}}})
	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m"), nil, true, "m")
	if !strings.Contains(rr.Body.String(), `"reasoning_content":"think"`) {
		t.Fatalf("expected rewrite, got %s", rr.Body.String())
	}

	// 禁用第一个渠道，避免第二个请求仍被 rw 渠道服务
	snap := store.Snapshot()
	rw := snap.Channels[0]
	rw.Enabled = false
	_ = store.PutChannel(rw)

	up2 := newUpstreamStream(t, streamBody)
	mustPutChannel(t, &Channel{Name: "norw", BaseURL: up2.URL, Enabled: true, Rewrite: false,
		Keys: []*UpKey{{Name: "k2", APIKey: "sk-2", Enabled: true}}})
	rr2 := httptest.NewRecorder()
	forwardChat(rr2, chatRequest("m"), nil, true, "m")
	if strings.Contains(rr2.Body.String(), "reasoning_content") {
		t.Fatalf("rewrite must be off for this channel, got %s", rr2.Body.String())
	}
}

func newUpstreamStream(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- ipv6pool 联动：真实 SOCKS5 stub + fake pool ----

func TestIPv6PoolProxyRouting(t *testing.T) {
	setupGateway(t)
	up := newUpstream(t, http.StatusOK, `{"choices":[{"message":{"content":"via-socks"}}]}`)
	socksAddr := startFakeSocks5(t)
	pool, poolSrv := startFakePool(t)
	socksPort, _ := strconv.Atoi(socksAddr[strings.LastIndex(socksAddr, ":")+1:])
	pool.portOverride = socksPort

	mustPutChannel(t, &Channel{Name: "c", BaseURL: up.URL, Enabled: true,
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true,
			Proxy: &ProxySpec{Kind: "ipv6pool", PoolURL: poolSrv.URL, SocksHost: socksAddr[:strings.LastIndex(socksAddr, ":")]}}}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m1"), nil, false, "m1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "via-socks") {
		t.Fatalf("expected response via socks tunnel: %s", rr.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("upstream calls = %d", up.count())
	}
}

func TestRotateOnStatus(t *testing.T) {
	setupGateway(t)
	up := newUpstream(t, http.StatusForbidden, `{"error":"banned"}`)
	socksAddr := startFakeSocks5(t)
	pool, poolSrv := startFakePool(t)
	socksPort, _ := strconv.Atoi(socksAddr[strings.LastIndex(socksAddr, ":")+1:])
	pool.portOverride = socksPort

	mustPutChannel(t, &Channel{Name: "c", BaseURL: up.URL, Enabled: true,
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true,
			Proxy: &ProxySpec{Kind: "ipv6pool", PoolURL: poolSrv.URL,
				SocksHost:      socksAddr[:strings.LastIndex(socksAddr, ":")],
				RotateStatuses: []int{403, 429}}}}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m1"), nil, false, "m1")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d (all failed), body=%s", rr.Code, rr.Body.String())
	}
	// 403 命中 RotateStatuses → 池子应收到一次 rotate
	pool.mu.Lock()
	n := pool.rotateN
	pool.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected rotate after 403, got %d", n)
	}
}

// ---- 下游鉴权 ----

func TestGatewayAuth(t *testing.T) {
	setupGateway(t)
	up := newUpstream(t, http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`)
	mustPutChannel(t, &Channel{Name: "c", BaseURL: up.URL, Enabled: true,
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true}}})
	gw := &GWKey{Name: "downstream", Key: "sk-gw-test123", Enabled: true}
	_ = store.PutGWKey(gw)

	// 无 key → 401
	rr := httptest.NewRecorder()
	req := chatRequest("m")
	rootHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rr.Code)
	}
	// 正确 key → 200 且下游 key 记入日志
	rr2 := httptest.NewRecorder()
	req2 := chatRequest("m")
	req2.Header.Set("Authorization", "Bearer sk-gw-test123")
	rootHandler(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	// 错误 key → 401
	rr3 := httptest.NewRecorder()
	req3 := chatRequest("m")
	req3.Header.Set("Authorization", "Bearer sk-gw-wrong")
	rootHandler(rr3, req3)
	if rr3.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rr3.Code)
	}
}

func TestModelsAggregation(t *testing.T) {
	setupGateway(t)
	up1 := newUpstream(t, http.StatusOK, `{}`)
	mustPutChannel(t, &Channel{Name: "static", BaseURL: up1.srv.URL, Enabled: true, Models: []string{"m-static", "shared"},
		Keys: []*UpKey{{Name: "k1", APIKey: "sk-1", Enabled: true}}})

	dyn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m-dyn"},{"id":"shared"},{"id":"m-dyn2"}]}`))
	}))
	t.Cleanup(dyn.Close)
	mustPutChannel(t, &Channel{Name: "dyn", BaseURL: dyn.URL, Enabled: true,
		Keys: []*UpKey{{Name: "k2", APIKey: "sk-2", Enabled: true}}})

	_ = store.PutGWKey(&GWKey{Name: "t", Key: "sk-gw-mt", Enabled: true})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-gw-mt")
	rr := httptest.NewRecorder()
	rootHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]bool{}
	for _, m := range out.Data {
		got[m.ID] = true
	}
	for _, want := range []string{"m-static", "m-dyn", "m-dyn2"} {
		if !got[want] {
			t.Fatalf("missing model %q; got %v", want, got)
		}
	}
	if len(out.Data) != 4 { // m-static, shared, m-dyn, m-dyn2（shared 去重）
		t.Fatalf("expected 4 models, got %d: %v", len(out.Data), got)
	}
}

// ---- 冷却表 ----

func TestCooldowns(t *testing.T) {
	c := newCooldowns()
	c.Mark("k", "m", 50*time.Millisecond)
	if !c.IsCooling("k", "m") {
		t.Fatal("expected cooling")
	}
	if c.IsCooling("k", "other") || c.IsCooling("other", "m") {
		t.Fatal("cooldown is per (key, model)")
	}
	time.Sleep(60 * time.Millisecond)
	if c.IsCooling("k", "m") {
		t.Fatal("expected expiry")
	}
}

// TestCooldownScopeDefaultIsKey 默认冷却粒度为按 key：一个模型的故障冷却对该
// key 的所有模型生效（含旧配置空值）；显式 "key_model" 时不同模型互不影响。
func TestCooldownScopeDefaultIsKey(t *testing.T) {
	setupGateway(t)
	up1 := newUpstream(t, http.StatusTooManyRequests, `{"error":"rate limited"}`)
	up2 := newUpstream(t, http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`)
	mustPutChannel(t, &Channel{Name: "bykey", BaseURL: up1.srv.URL, Enabled: true,
		Keys: []*UpKey{
			{Name: "k1", APIKey: "sk-1", Enabled: true},
			{Name: "k2", APIKey: "sk-2", Enabled: true, BaseURL: up2.srv.URL},
		}})

	// m1 触发 429 → k1 按 key 冷却
	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m1"), nil, false, "m1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	// 换模型 m2：k1 仍应被跳过（跨模型共享冷却）
	rr2 := httptest.NewRecorder()
	forwardChat(rr2, chatRequest("m2"), nil, false, "m2")
	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr2.Code, rr2.Body.String())
	}
	if up1.count() != 1 {
		t.Fatalf("default scope=key: m2 must skip cooling k1, up1 calls = %d", up1.count())
	}
}

func TestCooldownScopeKeyModel(t *testing.T) {
	setupGateway(t)
	up1 := newUpstream(t, http.StatusTooManyRequests, `{"error":"rate limited"}`)
	up2 := newUpstream(t, http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`)
	mustPutChannel(t, &Channel{Name: "permodel", BaseURL: up1.srv.URL, Enabled: true, CooldownScope: "key_model",
		Keys: []*UpKey{
			{Name: "k1", APIKey: "sk-1", Enabled: true},
			{Name: "k2", APIKey: "sk-2", Enabled: true, BaseURL: up2.srv.URL},
		}})

	rr := httptest.NewRecorder()
	forwardChat(rr, chatRequest("m1"), nil, false, "m1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	// key_model 粒度：m1 的冷却不影响 k1 对 m2 的可用性 → k1 再次被尝试
	rr2 := httptest.NewRecorder()
	forwardChat(rr2, chatRequest("m2"), nil, false, "m2")
	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr2.Code, rr2.Body.String())
	}
	if up1.count() != 2 {
		t.Fatalf("scope=key_model: m2 must retry k1, up1 calls = %d", up1.count())
	}
}

// 防止 import 未用告警的引用点
var (
	_ = context.Background
	_ = os.Getenv
	_ = json.Marshal
)
