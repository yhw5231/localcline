// 网关配置存储：渠道（上游供应商）、渠道内多账号 key（每个 key 可绑定独立代理）、
// 下游通用 key。持久化为 gateway.json（原子写入），供 WebUI 与 Admin API 读写。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProxySpec 渠道 key 的代理配置。
type ProxySpec struct {
	Kind string `json:"kind"` // "" / "none" | "static" | "ipv6pool"

	// static：固定代理 URL
	URL string `json:"url,omitempty"` // http(s)://user:pass@host:port 或 socks5://...

	// ipv6pool：对接 ipv6-proxy-pool 管理端
	PoolURL   string `json:"pool_url,omitempty"`   // 管理端基地址，如 http://1.2.3.4:8080
	PoolToken string `json:"pool_token,omitempty"` // Bearer token（可空）
	LeaseID   string `json:"lease_id,omitempty"`   // 租约 ID；空则自动 "gw-<keyID>"
	SocksHost string `json:"socks_host,omitempty"` // SOCKS5 服务地址（默认取池管理端 host）
	Share     bool   `json:"share,omitempty"`      // 跨渠道复用：同「池+BaseURL」分组的 key 共用同一租约/IP

	// 租约行为
	Persistent      bool  `json:"persistent,omitempty"`          // 常驻租约（免空闲回收）
	RotateOnNetErr  bool  `json:"rotate_on_net_err,omitempty"`   // 网络失败自动换 IP
	RotateStatuses  []int `json:"rotate_statuses,omitempty"`     // 上游返回这些状态码时换 IP（如 403,429）
	RotateIntervalS int   `json:"rotate_interval_sec,omitempty"` // 每 N 秒自动换 IP（0 关闭）
	RotateRequests  int   `json:"rotate_requests,omitempty"`     // 每 N 次请求自动换 IP（0 关闭）
}

// normalize 清理代理配置并校验。
func (p *ProxySpec) normalize() error {
	if p == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(p.Kind)) {
	case "", "none", "direct", "-":
		p.Kind = ""
		p.Share = false
		return nil
	case "static", "url":
		p.Kind = "static"
		p.Share = false
		if strings.TrimSpace(p.URL) == "" {
			return errors.New("static proxy requires url")
		}
		if _, err := parseProxyURL(p.URL); err != nil {
			return err
		}
		return nil
	case "ipv6pool", "pool":
		p.Kind = "ipv6pool"
		p.PoolURL = strings.TrimSpace(p.PoolURL)
		if p.PoolURL == "" {
			return errors.New("ipv6pool proxy requires pool_url")
		}
		if !strings.Contains(p.PoolURL, "://") {
			p.PoolURL = "http://" + p.PoolURL
		}
		p.PoolURL = strings.TrimRight(p.PoolURL, "/")
		return nil
	default:
		return fmt.Errorf("unsupported proxy kind %q", p.Kind)
	}
}

// UpKey 渠道内的一个上游账号。
type UpKey struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	APIKey  string     `json:"api_key"`
	BaseURL string     `json:"base_url,omitempty"` // 覆盖渠道 BaseURL
	Enabled bool       `json:"enabled"`
	Proxy   *ProxySpec `json:"proxy,omitempty"`
}

// Channel 一个上游渠道（OpenAI 兼容供应商）。
type Channel struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	BaseURL       string            `json:"base_url"`             // 如 https://api.cline.bot/api/v1
	ModelsURL     string            `json:"models_url,omitempty"` // 模型列表端点；默认 BaseURL + /models
	Models        []string          `json:"models,omitempty"`     // 静态模型列表（用于 /v1/models 聚合与路由过滤）
	Headers       map[string]string `json:"headers,omitempty"`    // 渠道级自定义请求头
	Rewrite       bool              `json:"rewrite_reasoning"`    // reasoning -> reasoning_content 改写（Cline 等需要）
	CooldownScope string            `json:"cooldown_scope,omitempty"` // 冷却粒度："" / "key" 按 key 跨模型共享（默认）；"key_model" 按 (key,model)
	Enabled       bool              `json:"enabled"`
	Keys          []*UpKey          `json:"keys"`
}

// cooldownScopeKeyModel 冷却粒度：按 (key, model) 独立冷却（显式选择时使用）。
const cooldownScopeKeyModel = "key_model"

// normalizeCooldownScope 归一化冷却粒度（"" 视为默认 key）。
func normalizeCooldownScope(scope string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case cooldownScopeKeyModel, "key-model":
		return cooldownScopeKeyModel, nil
	case "", "key":
		return "key", nil
	default:
		return "", fmt.Errorf("unsupported cooldown_scope %q (want \"key\" / \"key_model\")", scope)
	}
}

// chatURL 返回该渠道的 chat/completions 端点。
func (c *Channel) chatURL() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// modelsURL 返回该渠道的模型列表端点。
func (c *Channel) modelsURL() string {
	if u := strings.TrimSpace(c.ModelsURL); u != "" {
		return u
	}
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/models"
}

// allowsModel 判断渠道是否声明支持该模型：无模型信息（静态列表与已拉取列表皆空）时放行所有。
func (c *Channel) allowsModel(model string) bool {
	if model == "" {
		return true
	}
	if len(c.Models) == 0 {
		return true // 无声明，不设限
	}
	for _, m := range c.Models {
		if m == model {
			return true
		}
	}
	return false
}

// keyByID 查找渠道内的上游 key。
func (c *Channel) keyByID(id string) *UpKey {
	for _, k := range c.Keys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// GWKey 下游通用 key（供客户端调用本网关）。
type GWKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// gatewayConfig gateway.json 的持久化格式。
type gatewayConfig struct {
	Channels []*Channel `json:"channels"`
	GWKeys   []*GWKey   `json:"gateway_keys"`
}

// GatewayStore 配置存储（进程内单例，mutex 保护）。
type GatewayStore struct {
	mu   sync.RWMutex
	path string
	data gatewayConfig
}

var store *GatewayStore

func newGatewayStore(path string) *GatewayStore {
	return &GatewayStore{path: path}
}

// load 从磁盘读取；文件不存在时初始化空配置并落盘。
func (s *GatewayStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = gatewayConfig{Channels: []*Channel{}, GWKeys: []*GWKey{}}
		return s.saveLocked()
	}
	if err != nil {
		return fmt.Errorf("read gateway config: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		s.data = gatewayConfig{Channels: []*Channel{}, GWKeys: []*GWKey{}}
		return nil
	}
	var data gatewayConfig
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("decode gateway config: %w", err)
	}
	if data.Channels == nil {
		data.Channels = []*Channel{}
	}
	if data.GWKeys == nil {
		data.GWKeys = []*GWKey{}
	}
	for _, ch := range data.Channels {
		if ch.Keys == nil {
			ch.Keys = []*UpKey{}
		}
	}
	s.data = data
	return nil
}

// saveLocked 原子落盘（调用方持有写锁）。
func (s *GatewayStore) saveLocked() error {
	dir := filepath.Dir(s.path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
	}
	body, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("write config tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Snapshot 返回配置深拷贝（含全部敏感字段，仅 Admin API 使用）。
func (s *GatewayStore) Snapshot() gatewayConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	body, _ := json.Marshal(s.data)
	var out gatewayConfig
	_ = json.Unmarshal(body, &out)
	return out
}

// PutChannel 新增或整体替换渠道（含内嵌 keys）。id 为空时生成。
func (s *GatewayStore) PutChannel(ch *Channel) error {
	if err := normalizeChannel(ch); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch.ID == "" {
		ch.ID = randomHex(8)
	}
	replaced := false
	for i, cur := range s.data.Channels {
		if cur.ID == ch.ID {
			s.data.Channels[i] = ch
			replaced = true
			break
		}
	}
	if !replaced {
		s.data.Channels = append(s.data.Channels, ch)
	}
	return s.saveLocked()
}

func normalizeChannel(ch *Channel) error {
	ch.Name = strings.TrimSpace(ch.Name)
	ch.BaseURL = strings.TrimSpace(ch.BaseURL)
	if ch.Name == "" {
		return errors.New("channel name required")
	}
	if ch.BaseURL == "" {
		return errors.New("channel base_url required")
	}
	scope, err := normalizeCooldownScope(ch.CooldownScope)
	if err != nil {
		return err
	}
	ch.CooldownScope = scope
	if ch.Keys == nil {
		ch.Keys = []*UpKey{}
	}
	for _, k := range ch.Keys {
		if err := normalizeUpKey(k); err != nil {
			return fmt.Errorf("key %q: %w", k.Name, err)
		}
		if k.ID == "" {
			k.ID = randomHex(8)
		}
	}
	return nil
}

func normalizeUpKey(k *UpKey) error {
	k.Name = strings.TrimSpace(k.Name)
	k.APIKey = strings.TrimSpace(k.APIKey)
	k.BaseURL = strings.TrimSpace(k.BaseURL)
	return k.Proxy.normalize()
}

// DeleteChannel 删除渠道。
func (s *GatewayStore) DeleteChannel(id string) (*Channel, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ch := range s.data.Channels {
		if ch.ID == id {
			removed := ch
			s.data.Channels = append(s.data.Channels[:i], s.data.Channels[i+1:]...)
			_ = s.saveLocked()
			return removed, true
		}
	}
	return nil, false
}

// PutGWKey 新增或更新下游 key。
func (s *GatewayStore) PutGWKey(k *GWKey) error {
	k.Name = strings.TrimSpace(k.Name)
	k.Key = strings.TrimSpace(k.Key)
	if k.Name == "" {
		return errors.New("gateway key name required")
	}
	if k.Key == "" {
		return errors.New("gateway key required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if k.ID == "" {
		k.ID = randomHex(8)
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now()
	}
	replaced := false
	for i, cur := range s.data.GWKeys {
		if cur.ID == k.ID {
			k.CreatedAt = cur.CreatedAt
			s.data.GWKeys[i] = k
			replaced = true
			break
		}
	}
	if !replaced {
		s.data.GWKeys = append(s.data.GWKeys, k)
	}
	return s.saveLocked()
}

// DeleteGWKey 删除下游 key。
func (s *GatewayStore) DeleteGWKey(id string) (*GWKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.data.GWKeys {
		if k.ID == id {
			removed := k
			s.data.GWKeys = append(s.data.GWKeys[:i], s.data.GWKeys[i+1:]...)
			_ = s.saveLocked()
			return removed, true
		}
	}
	return nil, false
}

// FindUpKey 按 keyID 全局查找（返回渠道 + key 快照）。
func (s *GatewayStore) FindUpKey(keyID string) (*Channel, *UpKey, bool) {
	snap := s.Snapshot()
	for _, ch := range snap.Channels {
		if k := ch.keyByID(keyID); k != nil {
			return ch, k, true
		}
	}
	return nil, nil, false
}

// FindUpKey2 按 (channelID, keyID) 精确查找（返回快照）。
func (s *GatewayStore) FindUpKey2(channelID, keyID string) (*Channel, *UpKey, bool) {
	snap := s.Snapshot()
	for _, ch := range snap.Channels {
		if ch.ID != channelID {
			continue
		}
		if k := ch.keyByID(keyID); k != nil {
			return ch, k, true
		}
	}
	return nil, nil, false
}

// newGWKeyValue 生成下游 key：sk-gw-<32hex>。
func newGWKeyValue() string {
	return "sk-gw-" + randomHex(16)
}
