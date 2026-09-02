// 配置：全部来自环境变量，支持运行时 reload（测试用 t.Setenv + reloadConfig）。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 客户端指纹头默认值：与官方 Cline VSCode 客户端一致。
const (
	defaultClientReferer     = "https://cline.bot"
	defaultClientTitle       = "Cline"
	defaultClientUserAgent   = "Cline/4.1.16"
	defaultClientCoreVersion = "4.1.16"
	defaultClientPlatVersion = "1.106.0"
	defaultClientClientVer   = "4.1.16"
	defaultClientPlatform    = "vscode"
	defaultClientType        = "cline-vscode"
)

// ClientHeaders 上游请求携带的客户端指纹头（Content-Type / Authorization 之外的部分）。
// 每项可通过独立环境变量覆盖，另可用 CLIENT_HEADERS（JSON 对象）覆盖单项或追加额外头。
type ClientHeaders struct {
	HTTPReferer     string
	Title           string
	UserAgent       string
	CoreVersion     string
	PlatformVersion string
	ClientVersion   string
	Platform        string
	ClientType      string
	Extra           map[string]string // CLIENT_HEADERS 中无法映射到已知头名的额外自定义头
}

// applyOverrides 用 JSON 对象覆盖指纹头：键名（大小写不敏感）匹配已知头时替换
// 对应字段，空字符串表示不发送该头；其余键原样保留为额外自定义头。
func (ch *ClientHeaders) applyOverrides(custom map[string]string) {
	known := map[string]*string{
		"http-referer":       &ch.HTTPReferer,
		"x-title":            &ch.Title,
		"user-agent":         &ch.UserAgent,
		"x-core-version":     &ch.CoreVersion,
		"x-platform-version": &ch.PlatformVersion,
		"x-client-version":   &ch.ClientVersion,
		"x-platform":         &ch.Platform,
		"x-client-type":      &ch.ClientType,
	}
	ch.Extra = nil
	for name, value := range custom {
		if field, ok := known[strings.ToLower(name)]; ok {
			*field = value
			continue
		}
		if ch.Extra == nil {
			ch.Extra = map[string]string{}
		}
		ch.Extra[name] = value
	}
}

// loadClientHeaders 组装客户端指纹头：默认值 ← 单项环境变量 ← CLIENT_HEADERS JSON。
func loadClientHeaders() ClientHeaders {
	ch := ClientHeaders{
		HTTPReferer:     getenv("CLIENT_HTTP_REFERER", defaultClientReferer),
		Title:           getenv("CLIENT_TITLE", defaultClientTitle),
		UserAgent:       getenv("CLIENT_USER_AGENT", defaultClientUserAgent),
		CoreVersion:     getenv("CLIENT_CORE_VERSION", defaultClientCoreVersion),
		PlatformVersion: getenv("CLIENT_PLATFORM_VERSION", defaultClientPlatVersion),
		ClientVersion:   getenv("CLIENT_CLIENT_VERSION", defaultClientClientVer),
		Platform:        getenv("CLIENT_PLATFORM", defaultClientPlatform),
		ClientType:      getenv("CLIENT_TYPE", defaultClientType),
	}
	if raw := getenv("CLIENT_HEADERS", ""); raw != "" {
		var custom map[string]string
		if err := json.Unmarshal([]byte(raw), &custom); err != nil {
			log.Printf("warning: CLIENT_HEADERS is not a valid JSON object, ignored: %v", err)
		} else {
			ch.applyOverrides(custom)
		}
	}
	return ch
}

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

	// 上游请求的客户端指纹头
	ClientHeaders ClientHeaders

	// 登录验证
	LoginRequired   bool
	AdminUser       string
	AdminPass       string
	ExtraUsers      map[string]string
	TokenTTL        time.Duration
	DataDir         string
	AccountsPath    string
	TokenSecretPath string

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
	ReqLogSize         int // 请求记录环形缓冲容量
	UsageDBPath        string
	UsageRetentionDays int
	UsageMaxRecords    int
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
	dataDir := getenv("DATA_DIR", "data")
	accountsPath := getenv("ACCOUNTS_PATH", filepath.Join(dataDir, "accounts.json"))
	tokenSecretPath := getenv("TOKEN_SECRET_PATH", filepath.Join(dataDir, "token-secret"))
	usageDBPath := getenv("USAGE_DB_PATH", filepath.Join(dataDir, "usage.db"))

	persisted, _ := loadPersistedAccounts(accountsPath)
	adminUser := persisted.AdminUsername
	if adminUser == "" {
		adminUser = "admin"
	}
	adminPass := persisted.AdminPassword
	if adminPass == "" {
		adminPass = "admin"
	}
	adminUser = getenv("ADMIN_USERNAME", adminUser)
	adminPass = getenv("ADMIN_PASSWORD", adminPass)

	extra := map[string]string{}
	for user, password := range persisted.ExtraUsers {
		extra[user] = password
	}
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
		Port:               getenv("PORT", defaultPort),
		UpstreamURL:        getenv("UPSTREAM_URL", defaultUpstream),
		ModelsUpstream:     getenv("MODELS_UPSTREAM", defaultModelsUpstream),
		ClientHeaders:      loadClientHeaders(),
		LoginRequired:      loginReq,
		AdminUser:          adminUser,
		AdminPass:          adminPass,
		ExtraUsers:         extra,
		TokenTTL:           durationEnv("TOKEN_TTL", 24*time.Hour),
		DataDir:            dataDir,
		AccountsPath:       accountsPath,
		TokenSecretPath:    tokenSecretPath,
		UpstreamKeys:       keys,
		KeySelectMode:      mode,
		RateLimitCooldown:  cooldown,
		MaxKeyTries:        maxTries,
		ProxyPort:          getenv("PROXY_PORT", ""),
		ProxyUser:          getenv("PROXY_USER", ""),
		ProxyPass:          getenv("PROXY_PASS", ""),
		SocksPort:          getenv("SOCKS_PORT", ""),
		SocksUser:          getenv("SOCKS_USER", ""),
		SocksPass:          getenv("SOCKS_PASS", ""),
		ProxyManagedAll:    managedAll,
		PoolPort:           intEnv("PROXY_POOL_PORT", 0),
		PoolRangeStart:     rangeStart,
		PoolRangeEnd:       rangeEnd,
		PoolEntries:        parsePoolEntries(getenv("PROXY_POOL_ENTRIES", "")),
		ReqLogSize:         intEnv("REQ_LOG_SIZE", 1000),
		UsageDBPath:        usageDBPath,
		UsageRetentionDays: intEnv("USAGE_RETENTION_DAYS", 30),
		UsageMaxRecords:    intEnv("USAGE_MAX_RECORDS", 100000),
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
