// Admin REST API：供 WebUI 与脚本管理网关（渠道、上游 key、下游通用 key、
// 代理池操作、请求日志、用量统计）。全部端点要求管理员登录 token。
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// adminAPIHandler 分发 /admin/api/* 请求（Go 1.22 路由模式），整体要求管理员 token。
func adminAPIHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /admin/api/state", handleAdminState)
	mux.HandleFunc("PUT /admin/api/channels", handleAdminPutChannel)
	mux.HandleFunc("DELETE /admin/api/channels/{id}", handleAdminDeleteChannel)
	mux.HandleFunc("PUT /admin/api/gwkeys", handleAdminPutGWKey)
	mux.HandleFunc("DELETE /admin/api/gwkeys/{id}", handleAdminDeleteGWKey)
	mux.HandleFunc("POST /admin/api/pool/test", handleAdminPoolTest)
	mux.HandleFunc("POST /admin/api/pool/rotate", handleAdminPoolRotate)
	mux.HandleFunc("POST /admin/api/pool/release", handleAdminPoolRelease)
	mux.HandleFunc("GET /admin/api/pool/leases", handleAdminPoolLeases)
	mux.HandleFunc("POST /admin/api/testkey", handleAdminTestKey)
	mux.HandleFunc("GET /admin/api/requests", handleAdminRequests)
	mux.HandleFunc("GET /admin/api/usage", handleAdminUsage)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r); !ok {
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// requireAdmin 校验管理员 token。
func requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := bearerToken(r)
	if token == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing admin token", "unauthorized")
		return "", false
	}
	user, err := verifyToken(token)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid or expired token", "unauthorized")
		return "", false
	}
	if !isAdmin(user) {
		writeJSONError(w, http.StatusForbidden, "admin access required", "forbidden")
		return "", false
	}
	return user, true
}

// handleAdminState 返回 WebUI 所需的全部状态：渠道、下游 key、池租约缓存、总览。
func handleAdminState(w http.ResponseWriter, r *http.Request) {
	snap := store.Snapshot()
	type channelInfo struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Enabled    bool     `json:"enabled"`
		KeysTotal  int      `json:"keys_total"`
		KeysOn     int      `json:"keys_enabled"`
		HasPoolKey bool     `json:"has_pool_key"`
		Models     []string `json:"models,omitempty"`
	}
	chans := make([]channelInfo, 0, len(snap.Channels))
	for _, ch := range snap.Channels {
		ci := channelInfo{ID: ch.ID, Name: ch.Name, Enabled: ch.Enabled, KeysTotal: len(ch.Keys)}
		for _, k := range ch.Keys {
			if k.Enabled {
				ci.KeysOn++
			}
			if k.Proxy != nil && k.Proxy.Kind == "ipv6pool" {
				ci.HasPoolKey = true
			}
		}
		ci.Models = ch.Models
		chans = append(chans, ci)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channels":     snap.Channels,
		"gateway_keys": snap.GWKeys,
		"leases":       leaseMgr.ListLeases(),
		"channel_info": chans,
	})
}

// handleAdminPutChannel 新增/整体更新渠道（含内嵌 keys），随后按全量配置
// Reconcile 回收不再使用的池租约。
func handleAdminPutChannel(w http.ResponseWriter, r *http.Request) {
	var ch Channel
	if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid channel json: "+err.Error(), "bad_request")
		return
	}
	if err := store.PutChannel(&ch); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}
	reconcileLeases()
	writeJSON(w, http.StatusOK, ch)
}

// reconcileLeases 按当前全部渠道配置构建在用 key 集合并回收孤儿租约。
func reconcileLeases() {
	live := map[string]map[string]livePoolKey{}
	for _, ch := range store.Snapshot().Channels {
		for _, k := range ch.Keys {
			if k.Proxy == nil || k.Proxy.Kind != "ipv6pool" {
				continue
			}
			if live[k.Proxy.PoolURL] == nil {
				live[k.Proxy.PoolURL] = map[string]livePoolKey{}
			}
			live[k.Proxy.PoolURL][k.ID] = livePoolKey{Group: proxyGroup(ch, k), Shared: k.Proxy.Share}
		}
	}
	leaseMgr.Reconcile(live)
}

// handleAdminDeleteChannel 删除渠道并释放其池租约。
func handleAdminDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := store.DeleteChannel(id); !ok {
		writeJSONError(w, http.StatusNotFound, "channel not found", "not_found")
		return
	}
	reconcileLeases()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleAdminPutGWKey 新增/更新下游通用 key（Key 为空时自动生成）。
func handleAdminPutGWKey(w http.ResponseWriter, r *http.Request) {
	var k GWKey
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid gwkey json: "+err.Error(), "bad_request")
		return
	}
	generated := false
	if strings.TrimSpace(k.Key) == "" {
		k.Key = newGWKeyValue()
		generated = true
	}
	if err := store.PutGWKey(&k); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k, "generated": generated})
}

// handleAdminDeleteGWKey 删除下游 key。
func handleAdminDeleteGWKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := store.DeleteGWKey(id); !ok {
		writeJSONError(w, http.StatusNotFound, "gateway key not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---- 代理池操作 ----

// poolActionBody Admin 池操作的请求体。
// 指定 lease_id 时直接操作本地代理池中的该租约；否则按 channel_id+key_id 定位。
type poolActionBody struct {
	PoolURL   string `json:"pool_url"`
	PoolToken string `json:"pool_token"`
	ChannelID string `json:"channel_id"`
	KeyID     string `json:"key_id"`
	LeaseID   string `json:"lease_id"`
}

// handleAdminPoolTest 测试池子连通性（返回池状态）。
func handleAdminPoolTest(w http.ResponseWriter, r *http.Request) {
	var body poolActionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error(), "bad_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := newPoolClient(body.PoolURL, body.PoolToken).Status(ctx)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "pool test failed: "+err.Error(), "pool_error")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// findKeySpec 取指定渠道 key 的代理配置（快照）。
func findKeySpec(channelID, keyID string) (*ProxySpec, string, bool) {
	_, k, ok := store.FindUpKey2(channelID, keyID)
	if !ok {
		return nil, "", false
	}
	if k.Proxy == nil || k.Proxy.Kind != "ipv6pool" {
		return nil, k.Name, false
	}
	return k.Proxy, k.Name, true
}

// handleAdminPoolRotate 手动换 IP（lease_id 直连本地代理池条目，或按 key 定位）。
func handleAdminPoolRotate(w http.ResponseWriter, r *http.Request) {
	var body poolActionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error(), "bad_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var (
		lease *PoolLease
		err   error
		desc  string
	)
	if lid := strings.TrimSpace(body.LeaseID); lid != "" {
		lease, err = leaseMgr.RotateCached(ctx, body.PoolURL, lid)
		desc = lid
	} else {
		var spec *ProxySpec
		var keyName string
		var ok bool
		spec, keyName, ok = findKeySpec(body.ChannelID, body.KeyID)
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "key is not bound to an ipv6pool proxy", "bad_request")
			return
		}
		lease, err = leaseMgr.Rotate(ctx, spec, body.KeyID)
		desc = keyName + " @ " + spec.PoolURL
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "rotate failed: "+err.Error(), "pool_error")
		return
	}
	log.Printf("admin rotated lease %s: -> %s", desc, lease.IPv6)
	writeJSON(w, http.StatusOK, lease)
}

// handleAdminPoolRelease 手动释放租约（lease_id 直连本地代理池条目，或按 key 定位）。
func handleAdminPoolRelease(w http.ResponseWriter, r *http.Request) {
	var body poolActionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error(), "bad_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var err error
	var desc string
	if lid := strings.TrimSpace(body.LeaseID); lid != "" {
		err = leaseMgr.ReleaseLease(ctx, body.PoolURL, lid)
		desc = lid
	} else {
		var spec *ProxySpec
		var keyName string
		var ok bool
		spec, keyName, ok = findKeySpec(body.ChannelID, body.KeyID)
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "key is not bound to an ipv6pool proxy", "bad_request")
			return
		}
		leaseID := leaseMgr.leaseIDForKey(spec, body.KeyID)
		err = leaseMgr.ReleaseLease(ctx, spec.PoolURL, leaseID)
		desc = keyName + " lease " + leaseID + " @ " + spec.PoolURL
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "release failed: "+err.Error(), "pool_error")
		return
	}
	log.Printf("admin released lease %s", desc)
	writeJSON(w, http.StatusOK, map[string]any{"released": true})
}

// handleAdminPoolLeases 列出网关持有的池租约缓存。
func handleAdminPoolLeases(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"leases": leaseMgr.ListLeases()})
}

// handleAdminTestKey 用指定渠道 key 发一条测试请求，验证上游与代理连通性。
func handleAdminTestKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChannelID string `json:"channel_id"`
		KeyID     string `json:"key_id"`
		Model     string `json:"model"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error(), "bad_request")
		return
	}
	ch, k, ok := store.FindUpKey2(body.ChannelID, body.KeyID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "key not found", "not_found")
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		if len(ch.Models) > 0 {
			model = ch.Models[0]
		} else {
			model = "gpt-4o-mini"
		}
	}
	msg := body.Message
	if msg == "" {
		msg = "ping"
	}
	reqBody, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": msg}},
		"stream":     false,
		"max_tokens": 16,
	})

	cand := candidate{ch: ch, k: k}
	route, err := resolveProxy(&cand)
	var proxyDesc string
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "proxy resolve failed: " + err.Error()})
		return
	}
	proxyDesc = route.describe()

	client := newUpstreamClient(route)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := doUpstreamRequest(ctx, client, &cand, reqBody)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "proxy": proxyDesc, "error": err.Error(), "latency_ms": time.Since(start).Milliseconds()})
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	leaseMgr.RecordUse(cand.k.Proxy, cand.k.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status":     resp.StatusCode,
		"proxy":      proxyDesc,
		"latency_ms": time.Since(start).Milliseconds(),
		"snippet":    truncate(string(data), 512),
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
