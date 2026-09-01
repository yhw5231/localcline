// 代理池：支持单端口模式（用户名/密码区分不同代理）与端口范围模式（自动检测可用端口绑定）。
package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

// ProxyPool 管理代理池中的全部代理。
type ProxyPool struct {
	entries []PoolEntry
	port    int    // 单端口模式端口
	rStart  int    // 端口范围起始
	rEnd    int    // 端口范围结束
}

// newProxyPool 从配置创建代理池。
func newProxyPool(cfg Config) *ProxyPool {
	return &ProxyPool{
		entries: cfg.PoolEntries,
		port:    cfg.PoolPort,
		rStart:  cfg.PoolRangeStart,
		rEnd:    cfg.PoolRangeEnd,
	}
}

// StartListeners 启动代理池的全部监听器。
// 返回监听器列表，供调用者阻塞/关闭。
func (pp *ProxyPool) StartListeners() ([]net.Listener, error) {
	if len(pp.entries) == 0 {
		return nil, nil
	}

	upstreamBase := extractSchemeHost(cfg.UpstreamURL)

	if pp.port > 0 {
		// 单端口模式：所有条目共用一个端口，由 Proxy-Authorization 区分
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(pp.port))
		if err != nil {
			return nil, fmt.Errorf("pool port %d: %w", pp.port, err)
		}
		resolver := func(user, pass string) (string, *ProxyRoute, bool) {
			for _, e := range pp.entries {
				if e.User == user && e.Pass == pass {
					return e.Key, e.Backend, true
				}
			}
			return "", nil, false
		}
		ph := newPoolSinglePortHandler(upstreamBase, "", resolver)
		go func() {
			_ = http.Serve(ln, ph)
		}()
		return []net.Listener{ln}, nil
	}

	if pp.rStart > 0 && pp.rEnd >= pp.rStart {
		// 端口范围模式：每个条目独立端口
		var listeners []net.Listener
		nextPort := pp.rStart
		for _, e := range pp.entries {
			port := pp.findFreePort(nextPort)
			if port < 0 {
				// 回滚已启动的 listener
				for _, l := range listeners {
					l.Close()
				}
				return nil, fmt.Errorf("no free port for pool entry %s:%s in range %d-%d", e.User, e.Pass, pp.rStart, pp.rEnd)
			}
			nextPort = port + 1
			ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
			if err != nil {
				for _, l := range listeners {
					l.Close()
				}
				return nil, fmt.Errorf("port %d: %w", port, err)
			}
			ph := newPoolProxyHandler(upstreamBase, "", e.Key, e.Backend)
			if e.User != "" && e.Pass != "" {
				ph.creds = func(u, p string) bool { return u == e.User && p == e.Pass }
			}
			go func() {
				_ = http.Serve(ln, ph)
			}()
			listeners = append(listeners, ln)
		}
		return listeners, nil
	}

	return nil, nil
}

// findFreePort 从 start 开始尝试 listen，找到第一个可用端口。
func (pp *ProxyPool) findFreePort(start int) int {
	max := pp.rEnd
	if max <= 0 {
		max = start + 100
	}
	for p := start; p <= max; p++ {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err == nil {
			ln.Close()
			return p
		}
	}
	return -1
}

// extractSchemeHost 从 URL 中提取 scheme://host（不含 path）。
func extractSchemeHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	return u.Scheme + "://" + u.Host
}