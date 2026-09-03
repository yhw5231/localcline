// unigate —— 通用 AI 网关：多渠道账号、每 key 独立代理（含 ipv6-proxy-pool
// 动态租约）、OpenAI 兼容转发、故障转移、请求日志与 WebUI。
//
// 路由总览：
//
//	POST /login                    管理员登录（WebUI / Admin API 用）
//	/admin/api/*                   Admin REST API（需管理员 token）
//	POST /v1/chat/completions      下游 OpenAI 兼容转发（需通用 key，GW_KEY_AUTH=false 时免鉴权）
//	GET  /v1/models                聚合各渠道模型列表
//	/                              WebUI 静态资源（嵌入二进制）
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	// 用量数据库所在目录（容器需挂载该目录以持久化）
	if dir := filepath.Dir(cfg.UsageDBPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("warning: cannot create usage db dir %s: %v", dir, err)
		}
	}

	// 网关配置存储
	store = newGatewayStore(cfg.GWPath)
	if err := store.load(); err != nil {
		log.Fatalf("load gateway config %s: %v", cfg.GWPath, err)
	}
	snap := store.Snapshot()
	log.Printf("gateway config %s: %d channels, %d gateway keys", cfg.GWPath, len(snap.Channels), len(snap.GWKeys))

	// 租约分配表持久化（跨渠道复用：重启后分配与 IP 保持稳定）
	assignPath := getenv("LEASE_ASSIGN_PATH", filepath.Join(cfg.DataDir, "lease-assignments.json"))
	if err := leaseMgr.SetPersistPath(assignPath); err != nil {
		log.Printf("warning: load lease assignments: %v", err)
	}

	cool = newCooldowns()
	initStats()
	initUsageDB()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           statsMiddleware(rootHandler),
		ReadHeaderTimeout: 30 * time.Second,
	}
	log.Printf("unigate v%s listening on :%s (admin default %s/%s)", displayVersion(), cfg.Port, cfg.AdminUser, cfg.AdminPass)
	log.Fatal(srv.ListenAndServe())
}

// rootHandler 顶层分发。
func rootHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/api/version":
		handleVersion(w, r)
	case path == "/login":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte("Method Not Allowed"))
			return
		}
		handleLogin(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		adminAPIHandler().ServeHTTP(w, r)
	case strings.HasPrefix(path, "/v1/"), path == "/models", path == "/models/", path == "/chat/completions":
		user, ok := authorizeGW(w, r)
		if !ok {
			return
		}
		if rs := reqStatsFrom(r.Context()); rs != nil {
			rs.user = user
		}
		handleGateway(w, r)
	default:
		webHandler(w, r)
	}
}
