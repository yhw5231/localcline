// 用量数据库：SQLite（modernc.org/sqlite 纯 Go 驱动，无 CGO）记录每次请求的
// 输入/输出 token（来自上游 usage），按用户/模型/上游 key 记账，
// 支持 今日/24h/7天/30天/全部 窗口与条件过滤。
package main

import (
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动
)

// UsageEvent 一条用量事件（对应 usage_events 表一行）。
type UsageEvent struct {
	ID               string    `json:"id"`
	Time             time.Time `json:"time"`
	User             string    `json:"user,omitempty"`
	Channel          string    `json:"channel,omitempty"`
	Model            string    `json:"model,omitempty"`
	Key              string    `json:"key,omitempty"` // 上游 key（原始，内部记账用）
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	Status           int       `json:"status"`
	BytesOut         int64     `json:"bytes_out"`
}

// UsageDB SQLite 用量库。
type UsageDB struct {
	mu            sync.Mutex
	path          string // 空 = :memory:
	retentionDays int
	maxRecords    int
	db            *sql.DB
	appendCount   int
}

const usageSchema = `
CREATE TABLE IF NOT EXISTS usage_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	time INTEGER NOT NULL,
	user TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	key TEXT NOT NULL DEFAULT '',
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	status INTEGER NOT NULL DEFAULT 0,
	bytes_out INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_time ON usage_events(time);
CREATE INDEX IF NOT EXISTS idx_usage_user ON usage_events(user);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_events(model);
CREATE INDEX IF NOT EXISTS idx_usage_key ON usage_events(key);
CREATE INDEX IF NOT EXISTS idx_usage_channel ON usage_events(channel);
`

// usageMigrations 老库补列（ALTER TABLE 幂等性由检查保证）。
var usageMigrations = []string{
	`ALTER TABLE usage_events ADD COLUMN channel TEXT NOT NULL DEFAULT ''`,
}

func newUsageDB(path string, retentionDays, maxRecords int) *UsageDB {
	db := &UsageDB{
		path:          path,
		retentionDays: retentionDays,
		maxRecords:    maxRecords,
	}
	if retentionDays <= 0 {
		db.retentionDays = 30
	}
	if maxRecords <= 0 {
		db.maxRecords = 100000
	}
	dsn := path
	if dsn == "" {
		dsn = ":memory:"
	}
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return db // 打开失败则保持 nil db，Append/Query 均为空操作
	}
	// 单连接：保证 :memory: 语义一致，也避免 SQLite 写锁冲突
	sqldb.SetMaxOpenConns(1)
	db.db = sqldb
	if _, err := sqldb.Exec(usageSchema); err == nil {
		db.migrate(sqldb)
		db.cleanupLocked()
	}
	return db
}

// migrate 执行增量迁移（列已存在时忽略错误）。
func (db *UsageDB) migrate(sqldb *sql.DB) {
	for _, stmt := range usageMigrations {
		_, _ = sqldb.Exec(stmt)
	}
}

// Close 关闭底层连接。
func (db *UsageDB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.db != nil {
		return db.db.Close()
	}
	return nil
}

// Append 记录一条事件（INSERT）。
func (db *UsageDB) Append(ev UsageEvent) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.db == nil {
		return
	}
	_, err := db.db.Exec(`INSERT INTO usage_events
		(time, user, channel, model, key, prompt_tokens, completion_tokens, status, bytes_out)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		ev.Time.UnixNano(), ev.User, ev.Channel, ev.Model, ev.Key,
		ev.PromptTokens, ev.CompletionTokens, ev.Status, ev.BytesOut)
	if err != nil {
		return
	}
	db.appendCount++
	if db.appendCount%512 == 0 {
		db.cleanupLocked() // 定期清理，避免大表膨胀
	}
}

// Cleanup 主动清理过期事件并裁剪到 maxRecords（可被定时任务调用）。
func (db *UsageDB) Cleanup() {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.cleanupLocked()
}

// cleanupLocked 丢弃保留期外事件，并裁剪到 maxRecords（调用方需持锁）。
func (db *UsageDB) cleanupLocked() {
	if db.db == nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -db.retentionDays).UnixNano()
	_, _ = db.db.Exec(`DELETE FROM usage_events WHERE time < ?`, cutoff)
	// 只保留最近 maxRecords 条（按 time 降序取前 N，删除其余）
	if db.maxRecords > 0 {
		_, _ = db.db.Exec(`DELETE FROM usage_events WHERE id NOT IN (
			SELECT id FROM usage_events ORDER BY time DESC, id DESC LIMIT ?)`, db.maxRecords)
	}
}

// Count 返回当前事件总数。
func (db *UsageDB) Count() int64 {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.db == nil {
		return 0
	}
	var n int64
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n)
	return n
}

// UsageFilter 查询条件。
type UsageFilter struct {
	Window  string // "today" | "24h" | "7d" | "30d" | "all"（默认 all）
	Start   time.Time
	End     time.Time
	User    string
	Channel string
	Model   string
	Key     string
}

// UsageBreakdown 单维度聚合。
type UsageBreakdown struct {
	Name             string `json:"name"`
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	BytesOut         int64  `json:"bytes_out"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
}

// UsageResult 聚合结果。
type UsageResult struct {
	Window           string           `json:"window"`
	Start            time.Time        `json:"start"`
	End              time.Time        `json:"end"`
	Requests         int64            `json:"requests"`
	Errors           int64            `json:"errors"`
	BytesOut         int64            `json:"bytes_out"`
	PromptTokens     int64            `json:"prompt_tokens"`
	CompletionTokens int64            `json:"completion_tokens"`
	TotalTokens      int64            `json:"total_tokens"`
	ByUser           []UsageBreakdown `json:"by_user,omitempty"`
	ByChannel        []UsageBreakdown `json:"by_channel,omitempty"`
	ByModel          []UsageBreakdown `json:"by_model,omitempty"`
	ByKey            []UsageBreakdown `json:"by_key,omitempty"`
}

// resolveWindow 把窗口字符串解析成 [start,end)。
func resolveWindow(win string, now time.Time) (time.Time, time.Time) {
	switch win {
	case "today":
		y, m, d := now.Date()
		start := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		return start, now
	case "24h":
		return now.Add(-24 * time.Hour), now
	case "7d":
		return now.Add(-7 * 24 * time.Hour), now
	case "30d":
		return now.Add(-30 * 24 * time.Hour), now
	default: // all / 空
		return time.Time{}, time.Time{}
	}
}

// buildWhere 构建 WHERE 子句与参数（时间用 UnixNano，保证亚秒精度）。
func buildWhere(f UsageFilter) (string, []any) {
	now := time.Now()
	start, end := resolveWindow(f.Window, now)
	if !f.Start.IsZero() {
		start = f.Start
	}
	if !f.End.IsZero() {
		end = f.End
	}
	conds := []string{"1=1"}
	args := []any{}
	if !start.IsZero() {
		conds = append(conds, "time >= ?")
		args = append(args, start.UnixNano())
	}
	if !end.IsZero() {
		conds = append(conds, "time < ?")
		args = append(args, end.UnixNano())
	}
	if f.User != "" {
		conds = append(conds, "user = ?")
		args = append(args, f.User)
	}
	if f.Channel != "" {
		conds = append(conds, "channel = ?")
		args = append(args, f.Channel)
	}
	if f.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, f.Model)
	}
	if f.Key != "" {
		conds = append(conds, "key = ?")
		args = append(args, f.Key)
	}
	return strings.Join(conds, " AND "), args
}

// Query 按条件聚合用量（SQL 聚合，含 by_user/by_model/by_key 分解）。
func (db *UsageDB) Query(f UsageFilter) UsageResult {
	now := time.Now()
	start, end := resolveWindow(f.Window, now)
	if !f.Start.IsZero() {
		start = f.Start
	}
	if !f.End.IsZero() {
		end = f.End
	}
	res := UsageResult{Window: f.Window, Start: start, End: end}

	db.mu.Lock()
	defer db.mu.Unlock()
	if db.db == nil {
		return res
	}

	where, args := buildWhere(f)

	// 总量
	var promptTok, compTok, bytesOut, errors int64
	err := db.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(bytes_out),0),
		COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0)
		FROM usage_events WHERE `+where, args...).
		Scan(&res.Requests, &promptTok, &compTok, &bytesOut, &errors)
	if err != nil {
		return res
	}
	res.PromptTokens = promptTok
	res.CompletionTokens = compTok
	res.BytesOut = bytesOut
	res.Errors = errors
	res.TotalTokens = promptTok + compTok

	res.ByUser = db.queryBreakdown(where, args, "user")
	res.ByChannel = db.queryBreakdown(where, args, "channel")
	res.ByModel = db.queryBreakdown(where, args, "model")
	res.ByKey = db.queryBreakdown(where, args, "key")
	return res
}

// queryBreakdown 按某列分组聚合（col 为 user/model/key）。
func (db *UsageDB) queryBreakdown(where string, args []any, col string) []UsageBreakdown {
	rows, err := db.db.Query(`SELECT COALESCE(NULLIF(`+col+`,''),'unknown') AS name,
		COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(bytes_out),0),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0)
		FROM usage_events WHERE `+where+` GROUP BY name`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []UsageBreakdown
	for rows.Next() {
		var b UsageBreakdown
		if err := rows.Scan(&b.Name, &b.Requests, &b.Errors, &b.BytesOut, &b.PromptTokens, &b.CompletionTokens); err != nil {
			continue
		}
		b.TotalTokens = b.PromptTokens + b.CompletionTokens
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalTokens > out[j].TotalTokens })
	return out
}

// ---- 全局实例 ----

var usageDB *UsageDB

// initUsageDB 用配置初始化用量库（幂等，reloadConfig 时调用）。
func initUsageDB() {
	usageDB = newUsageDB(cfg.UsageDBPath, cfg.UsageRetentionDays, cfg.UsageMaxRecords)
}

// handleAdminUsage 返回窗口化用量统计（?window=&user=&model=&key=）。
func handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	f := UsageFilter{
		Window:  r.URL.Query().Get("window"),
		User:    r.URL.Query().Get("user"),
		Channel: r.URL.Query().Get("channel"),
		Model:   r.URL.Query().Get("model"),
		Key:     r.URL.Query().Get("key"),
	}
	if f.Window == "" {
		f.Window = "today"
	}
	if f.Window != "today" && f.Window != "24h" && f.Window != "7d" &&
		f.Window != "30d" && f.Window != "all" {
		writeJSONError(w, http.StatusBadRequest, "invalid window; use today|24h|7d|30d|all", "bad_request")
		return
	}
	res := usageDB.Query(f)
	writeJSON(w, http.StatusOK, res)
}
