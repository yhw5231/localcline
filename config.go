// 配置：全部来自环境变量，支持运行时 reload（测试用 t.Setenv + reloadConfig）。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"
)

// PoolEntry：代理池中的一条代理。
// User/Pass 是该代理的凭证（单端口模式下凭 Proxy-Authorization 区分不同代理）；
// Key 是该代理转发到上游时使用的上游 key（空表示用全局 keyMgr 选择）；
// Backend 非空时表示先链经过该上游代理再访问目标。
type PoolEntry struct {
	User    string
	Pass    string
	Key     string
	Backend *ProxyRoute
}

// Config 汇总全部运行时配置。
type Config struct {
	// API 服务
	Port           string
	UpstreamURL    string
	ModelsUpstream string

	// 登录验证
	LoginRequired bool
	AdminUser     string
	AdminPass     string
	ExtraUsers    map[string]string
	TokenTTL      time.Duration

	// 多上游 key
	UpstreamKeys      []string
	KeySelectMode     string // "sequential" | "roundrobin"
	RateLimitCooldown time.Duration
	MaxKeyTries       int

	// 普通代理（HTTP 正向代理）
	ProxyPort string
	ProxyUser string
	ProxyPass string
	// 普通代理（SOCKS5）
	SocksPort string
	SocksUser string
	SocksPass string
	// PROXY_MANAGED_ALL：为 true 时，普通代理把所有 absolute-form 请求都当作
	// “转发到上游”处理（忽略目标 host 是否等于上游 host），用于客户端随便填 base URL 的场景。
	ProxyManagedAll bool

	// 代理池
	PoolPort       int // 单端口模式：>0 时所有池代理共用一个端口
	PoolRangeStart int // 端口范围模式
	PoolRangeEnd   int
	PoolEntries    []PoolEntry

	// 可观察性
	ReqLogSize        int // 请求记录环形缓冲容量
	UsageDBPath       string
	UsageRetentionDays int
	UsageMaxRecords   int
}

// defaultTokenSecret 在进程内保持稳定（restart 后失效），避免每次 reloadConfig 都换密钥
// 导致已签发的 token 立刻失效。生产环境请用 TOKEN_SECRET 固定。
var defaultTokenSecret = randomHex(32)

// 全局运行时状态：cfg 每次请求/测试可 reload；keyMgr 保存多 key 的轮询游标与限流冷却。
var (
	cfg    Config
	keyMgr *KeyManager
)

func init() {
	reloadConfig()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// getenv：取环境变量，空则返回默认值。
func getenv(key, def string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	return v
}

// loadConfig 从环境变量读取配置。
func loadConfig() Config {
	extra := map[string]string{}
	for _, pair := range splitCSV(getenv("EXTRA_USERS", "")) {
		if u, p, ok := cutPair(pair); ok {
			extra[u] = p
		}
	}

	loginReq, _ := parseBoolEnv("LOGIN_REQUIRED", true)
	managedAll, _ := parseBoolEnv("PROXY_MANAGED_ALL", false)

	keys := splitCSV(getenv("UPSTREAM_KEYS", ""))
	mode := strings.ToLower(getenv("KEY_SELECT_MODE", "sequential"))
	if mode != "roundrobin" {
		mode = "sequential"
	}
	cooldown := durationEnv("RATE_LIMIT_COOLDOWN", 60*time.Second)
	maxTries := intEnv("MAX_KEY_TRIES", 0)

	rangeStart, rangeEnd := 0, 0
	if r := os.Getenv("PROXY_POOL_RANGE"); r != "" {
		rangeStart, rangeEnd = parsePortRange(r)
	}

	return Config{
		Port:              getenv("PORT", defaultPort),
		UpstreamURL:       getenv("UPSTREAM_URL", defaultUpstream),
		ModelsUpstream:    getenv("MODELS_UPSTREAM", defaultModelsUpstream),
		LoginRequired:     loginReq,
		AdminUser:         getenv("ADMIN_USERNAME", "admin"),
		AdminPass:         getenv("ADMIN_PASSWORD", "admin"),
		ExtraUsers:        extra,
		TokenTTL:          durationEnv("TOKEN_TTL", 24*time.Hour),
		UpstreamKeys:      keys,
		KeySelectMode:     mode,
		RateLimitCooldown: cooldown,
		MaxKeyTries:       maxTries,
		ProxyPort:         getenv("PROXY_PORT", ""),
		ProxyUser:         getenv("PROXY_USER", ""),
		ProxyPass:         getenv("PROXY_PASS", ""),
		SocksPort:         getenv("SOCKS_PORT", ""),
		SocksUser:         getenv("SOCKS_USER", ""),
		SocksPass:         getenv("SOCKS_PASS", ""),
		ProxyManagedAll:   managedAll,
		PoolPort:          intEnv("PROXY_POOL_PORT", 0),
		PoolRangeStart:    rangeStart,
		PoolRangeEnd:      rangeEnd,
		PoolEntries:       parsePoolEntries(getenv("PROXY_POOL_ENTRIES", "")),
		ReqLogSize:        intEnv("REQ_LOG_SIZE", 1000),
		UsageDBPath:       getenv("USAGE_DB_PATH", "data/usage.db"),
		UsageRetentionDays: intEnv("USAGE_RETENTION_DAYS", 30),
		UsageMaxRecords:   intEnv("USAGE_MAX_RECORDS", 100000),
	}
}

// reloadConfig 重新加载配置并重建依赖配置的全局状态（keyMgr、请求日志、用量库）。
func reloadConfig() {
	cfg = loadConfig()
	keyMgr = newKeyManager(effectiveKeys(), cfg.KeySelectMode)
	initStats()
	initUsageDB()
}

// parseBoolEnv：解析 0/false/no/off 为 false，其余（含空）取默认值。
func parseBoolEnv(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, err
	}
	return b, nil
}

func intEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func durationEnv(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// splitCSV 按逗号切分并去空白、去空项。
func splitCSV(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cutPair 把 "user:pass" 切成用户名与密码。
func cutPair(s string) (string, string, bool) {
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// parsePortRange 解析 "start-end"（如 20000-20050）。
func parsePortRange(s string) (int, int) {
	idx := strings.IndexByte(s, '-')
	if idx < 0 {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		return n, n
	}
	start, _ := strconv.Atoi(strings.TrimSpace(s[:idx]))
	end, _ := strconv.Atoi(strings.TrimSpace(s[idx+1:]))
	return start, end
}

// parsePoolEntries 解析代理池条目列表（逗号分隔）。
// 每条格式：user:pass[:key][@backendURL]，key 为空表示用全局 keyMgr；
// backendURL 支持 http(s):// 与 socks5://，表示先链经过该上游代理。
func parsePoolEntries(s string) []PoolEntry {
	var out []PoolEntry
	for _, p := range splitCSV(s) {
		creds, backendURL := p, ""
		if i := strings.LastIndex(p, "@"); i >= 0 {
			creds, backendURL = p[:i], p[i+1:]
		}
		parts := strings.SplitN(creds, ":", 3)
		if len(parts) < 2 {
			continue // 至少需要 user:pass
		}
		ent := PoolEntry{User: parts[0], Pass: parts[1]}
		if len(parts) == 3 {
			ent.Key = parts[2]
		}
		if backendURL != "" {
			if r, err := parseProxyRoute(backendURL); err == nil {
				ent.Backend = r
			}
		}
		out = append(out, ent)
	}
	return out
}
