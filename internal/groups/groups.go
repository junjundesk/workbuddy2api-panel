// Package groups 管理号池分组元数据、成员归属、分组密钥与分组代理配置。
//
// 账号归属（members）是权威来源，独立于 auth 文件与 pool 状态持久化到
// data/groups.json；auth 文件仍保持 workbuddy-<uid>.json 平铺，避免 refresh
// 写回时移动文件引发路径竞争。
package groups

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/junjundesk/workbuddy2api-panel/internal/prompt"
)

// Proxy 分组出站代理配置。Type 取 http / socks5 / socks5h。
type Proxy struct {
	Type     string `json:"type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Group 一个号池分组。
type Group struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Color      string `json:"color,omitempty"`
	SharedPool bool   `json:"shared_pool"`
	CallPublic bool   `json:"call_public"`
	// PromptEnabled / PromptText 分组专属系统提示词（replaces 语义：整体替换客户端
	// system / developer 消息，见 internal/prompt 与 internal/server 的 effectivePrompt）。
	// 关闭或文本空白时沿用全局 prompt 配置；改动经面板保存即对后续请求生效。
	//
	// PromptText 是**旧版单条文本**的兼容字段（面板老版本 / 手工编辑的 groups.json）；
	// 新版走 PromptEntries + PromptReplaces（多条系统/用户提示词 + 关键词替换，见
	// internal/prompt/rules.go）。两者同时存在时以条目为准。
	PromptEnabled  bool             `json:"prompt_enabled,omitempty"`
	PromptText     string           `json:"prompt_text,omitempty"`
	PromptEntries  []prompt.Entry   `json:"prompt_entries,omitempty"`
	PromptReplaces []prompt.Replace `json:"prompt_replaces,omitempty"`
	KeyHash        string           `json:"key_hash,omitempty"`
	KeyHint        string           `json:"key_hint,omitempty"`
	// Key 分组密钥明文（groups.json 0600 落盘，供面板手动查看）；旧分组可能为空。
	Key       string    `json:"key,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Proxy     *Proxy    `json:"proxy,omitempty"`
	Members   []string  `json:"members"`
}

// Meta 是 pool 选号需要的分组策略快照（不含密钥/代理等敏感配置）。
type Meta struct {
	Shared     bool
	CallPublic bool
}

type fileFormat struct {
	Version     int     `json:"version"`
	PublicProxy *Proxy  `json:"public_proxy,omitempty"`
	Groups      []Group `json:"groups"`
}

// Store 分组注册表。所有导出方法并发安全；写操作落盘 groups.json。
type Store struct {
	mu     sync.RWMutex
	path   string
	groups map[string]*Group
	byKey  map[string]string // key hash -> group id
	// publicProxy 公共池（未分组账号）出站代理；nil = 直连。
	publicProxy *Proxy
}

// Load 读取 groups.json；文件不存在时返回空注册表（不是错误）。
func Load(path string) (*Store, error) {
	s := &Store{path: path, groups: map[string]*Group{}, byKey: map[string]string{}}
	if strings.TrimSpace(path) == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse groups.json: %w", err)
	}
	s.publicProxy = normalizeProxy(f.PublicProxy)
	for i := range f.Groups {
		g := f.Groups[i]
		if strings.TrimSpace(g.ID) == "" || strings.TrimSpace(g.Name) == "" {
			continue
		}
		if g.Members == nil {
			g.Members = []string{}
		}
		g.Proxy = normalizeProxy(g.Proxy)
		s.groups[g.ID] = &g
		if g.KeyHash != "" {
			s.byKey[g.KeyHash] = g.ID
		}
	}
	return s, nil
}

func normalizeProxy(p *Proxy) *Proxy {
	if p == nil {
		return nil
	}
	t := strings.ToLower(strings.TrimSpace(p.Type))
	switch t {
	case "http", "socks5", "socks5h":
	default:
		return nil
	}
	if strings.TrimSpace(p.Host) == "" || p.Port < 1 || p.Port > 65535 {
		return nil
	}
	p.Type = t
	p.Host = strings.TrimSpace(p.Host)
	return p
}

// Groups 返回按创建时间排序的分组快照（副本，调用方可安全修改）。
func (s *Store) Groups() []Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Group, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, *cloneGroup(g))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Get 返回分组副本。
func (s *Store) Get(id string) (Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok {
		return Group{}, false
	}
	return *cloneGroup(g), true
}

func cloneGroup(g *Group) *Group {
	cp := *g
	cp.Members = append([]string(nil), g.Members...)
	if g.Proxy != nil {
		p := *g.Proxy
		cp.Proxy = &p
	}
	return &cp
}

// Create 新建分组并自动生成一次性分组密钥，返回分组和明文密钥。
func (s *Store) Create(name, color string, shared, callPublic bool) (Group, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, "", fmt.Errorf("group name is required")
	}
	id, err := newID()
	if err != nil {
		return Group{}, "", err
	}
	key, hash, hint, err := generateKey(id)
	if err != nil {
		return Group{}, "", err
	}
	g := &Group{
		ID: id, Name: name, Color: strings.TrimSpace(color), SharedPool: shared,
		CallPublic: callPublic, KeyHash: hash, KeyHint: hint, Key: key, CreatedAt: time.Now(),
		Members: []string{},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[g.ID] = g
	s.byKey[hash] = g.ID
	if err := s.saveLocked(); err != nil {
		delete(s.groups, g.ID)
		delete(s.byKey, hash)
		return Group{}, "", err
	}
	return *cloneGroup(g), key, nil
}

// Update 修改分组基础属性。
func (s *Store) Update(id, name, color string, shared, callPublic bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("group name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return os.ErrNotExist
	}
	g.Name = name
	g.Color = strings.TrimSpace(color)
	g.SharedPool = shared
	g.CallPublic = callPublic
	return s.saveLocked()
}

// Delete 删除分组并返回被删分组（members 供调用方处理账号去向）。
func (s *Store) Delete(id string) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return Group{}, os.ErrNotExist
	}
	out := *cloneGroup(g)
	delete(s.groups, id)
	if g.KeyHash != "" {
		delete(s.byKey, g.KeyHash)
	}
	if err := s.saveLocked(); err != nil {
		s.groups[id] = g
		if g.KeyHash != "" {
			s.byKey[g.KeyHash] = id
		}
		return Group{}, err
	}
	return out, nil
}

// SetProxy 设置/清除分组代理。p=nil 表示直连。
func (s *Store) SetProxy(id string, p *Proxy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return os.ErrNotExist
	}
	var np *Proxy
	if p != nil {
		cp := *p
		if np = normalizeProxy(&cp); np == nil {
			return fmt.Errorf("invalid proxy config")
		}
	}
	old := g.Proxy
	g.Proxy = np
	if err := s.saveLocked(); err != nil {
		g.Proxy = old
		return err
	}
	return nil
}

// PublicProxy 返回公共池代理副本；nil = 直连。
func (s *Store) PublicProxy() *Proxy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.publicProxy == nil {
		return nil
	}
	p := *s.publicProxy
	return &p
}

// SetPublicProxy 设置/清除公共池代理。p=nil 表示直连。
func (s *Store) SetPublicProxy(p *Proxy) error {
	var np *Proxy
	if p != nil {
		cp := *p
		if np = normalizeProxy(&cp); np == nil {
			return fmt.Errorf("invalid proxy config")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.publicProxy
	s.publicProxy = np
	if err := s.saveLocked(); err != nil {
		s.publicProxy = old
		return err
	}
	return nil
}

// MaxPromptBytes 单分组提示词配置总字节上限（64 KiB）。
//
// 规则由"多条提示词 + 关键词替换"组成，整段替换 system（2KB 级别就够用），上限
// 只用于挡住"误把整个语料粘进来"：groups.json 每次写操作都会整体重写、读路径也随
// 分组快照加载，无上限会让一次误操作长期拖慢所有分组的配置读写。
const MaxPromptBytes = 64 << 10

// 规则条数与单条关键词长度上限。规则是**每请求**都要线性遍历一次的热配置，
// 条数失控会抬高所有聊天请求的固定开销；上限只防误配置，不影响正常使用。
const (
	MaxPromptEntries     = 16
	MaxPromptReplaces    = 32
	MaxReplaceMatchBytes = 1 << 10
)

// Prompt 返回分组当前生效的**系统**提示词文本（未启用 / 无系统条目 / 分组不存在 → false）。
//
// 兼容旧字段：条目为空时回退 PromptText。这是每个聊天请求都会走一次的热路径，
// 只做 RLock + map 查找 + 字符串拼接，不 cloneGroup（不复制 Members 切片）。
func (s *Store) Prompt(id string) (string, bool) {
	rs, ok := s.Rules(id)
	if !ok {
		return "", false
	}
	text := rs.SystemText()
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// Rules 返回分组当前生效的提示词规则（未启用 / 规则为空 / 分组不存在 → false）。
//
// 兼容口径：旧版只有 PromptText（单条 system），读取时补成一条 system 条目；
// 新写入的 entries/replaces 优先。返回的 Rules 已 Normalize（空条目已丢弃）。
func (s *Store) Rules(id string) (prompt.Rules, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return prompt.Rules{}, false
	}
	s.mu.RLock()
	g, ok := s.groups[id]
	if !ok || !g.PromptEnabled {
		s.mu.RUnlock()
		return prompt.Rules{}, false
	}
	rs := prompt.Rules{Entries: g.PromptEntries, Replaces: g.PromptReplaces}
	if len(rs.Entries) == 0 && len(rs.Replaces) == 0 && strings.TrimSpace(g.PromptText) != "" {
		rs.Entries = []prompt.Entry{{Role: prompt.RoleSystem, Text: g.PromptText}}
	}
	s.mu.RUnlock()
	rs = rs.Normalize()
	if rs.Empty() {
		return prompt.Rules{}, false
	}
	return rs, true
}

// SetPrompt 设置旧版单条系统提示词（保留给旧面板请求与旧调用方）。
//
// 关闭开关时保留文本，下次开启无需重填，但不生效；写盘失败回滚内存值
// （与 SetProxy 同 pattern）。
func (s *Store) SetPrompt(id string, enabled bool, text string) error {
	var rs prompt.Rules
	if strings.TrimSpace(text) != "" {
		rs.Entries = []prompt.Entry{{Role: prompt.RoleSystem, Text: text}}
	}
	rs = rs.Normalize()
	if err := checkPromptRules(enabled, rs); err != nil {
		return err
	}
	return s.writePrompt(id, enabled, text, rs)
}

// SetPromptRules 设置多条提示词（system / user）+ 关键词替换规则。
//   - enabled 且规则全空 → 报错（避免"开关开着但等于没开"的静默状态）；
//   - 关闭开关时保留规则，下次开启无需重填，但不生效；
//   - 写盘失败回滚内存值。
func (s *Store) SetPromptRules(id string, enabled bool, rs prompt.Rules) error {
	rs = rs.Normalize()
	if err := checkPromptRules(enabled, rs); err != nil {
		return err
	}
	return s.writePrompt(id, enabled, "", rs)
}

// checkPromptRules 校验开关与规则的组合约束、数量上限与总字节上限。
// 关闭状态下同样校验字节上限：一次误粘就该被挡住，不该先写进 groups.json 再说。
func checkPromptRules(enabled bool, rs prompt.Rules) error {
	if enabled && rs.Empty() {
		return fmt.Errorf("prompt rules are required when enabled")
	}
	if len(rs.Entries) > MaxPromptEntries {
		return fmt.Errorf("too many prompt entries: %d (limit %d)", len(rs.Entries), MaxPromptEntries)
	}
	if len(rs.Replaces) > MaxPromptReplaces {
		return fmt.Errorf("too many keyword replacements: %d (limit %d)", len(rs.Replaces), MaxPromptReplaces)
	}
	total := 0
	for _, e := range rs.Entries {
		total += len(e.Text)
	}
	for _, rp := range rs.Replaces {
		if len(rp.Match) > MaxReplaceMatchBytes {
			return fmt.Errorf("keyword too long: %d bytes (limit %d)", len(rp.Match), MaxReplaceMatchBytes)
		}
		total += len(rp.Match) + len(rp.With)
	}
	if total > MaxPromptBytes {
		return fmt.Errorf("prompt text too large: %d bytes (limit %d)", total, MaxPromptBytes)
	}
	return nil
}

// writePrompt 两条写路径的共同实现：持锁写内存 + 落盘，失败回滚。
func (s *Store) writePrompt(id string, enabled bool, text string, rs prompt.Rules) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return os.ErrNotExist
	}
	oldEnabled, oldText := g.PromptEnabled, g.PromptText
	oldEntries, oldReplaces := g.PromptEntries, g.PromptReplaces
	g.PromptEnabled, g.PromptText = enabled, text
	g.PromptEntries, g.PromptReplaces = rs.Entries, rs.Replaces
	if err := s.saveLocked(); err != nil {
		g.PromptEnabled, g.PromptText = oldEnabled, oldText
		g.PromptEntries, g.PromptReplaces = oldEntries, oldReplaces
		return err
	}
	return nil
}

// RevealKey 返回分组密钥明文；旧分组（只存哈希）返回 false。
func (s *Store) RevealKey(id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok || g.Key == "" {
		return "", false
	}
	return g.Key, true
}

// RotateKey 轮换分组密钥，返回新明文密钥。
func (s *Store) RotateKey(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return "", os.ErrNotExist
	}
	key, hash, hint, err := generateKey(id)
	if err != nil {
		return "", err
	}
	oldHash, oldHint, oldKey := g.KeyHash, g.KeyHint, g.Key
	g.KeyHash, g.KeyHint, g.Key = hash, hint, key
	s.byKey[hash] = id
	if oldHash != "" {
		delete(s.byKey, oldHash)
	}
	if err := s.saveLocked(); err != nil {
		g.KeyHash, g.KeyHint, g.Key = oldHash, oldHint, oldKey
		delete(s.byKey, hash)
		if oldHash != "" {
			s.byKey[oldHash] = id
		}
		return "", err
	}
	return key, nil
}

// AddMember 把账号加入分组；若已在其他分组则先移出（一个账号只能属于一个分组）。
func (s *Store) AddMember(id, uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return fmt.Errorf("uid is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return os.ErrNotExist
	}
	for _, other := range s.groups {
		if other.ID == id {
			continue
		}
		other.Members = removeString(other.Members, uid)
	}
	if !contains(g.Members, uid) {
		g.Members = append(g.Members, uid)
	}
	return s.saveLocked()
}

// RemoveMember 从分组移除账号；不存在时无操作。
func (s *Store) RemoveMember(id, uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return os.ErrNotExist
	}
	g.Members = removeString(g.Members, uid)
	return s.saveLocked()
}

// Membership 返回 uid -> group id 的权威归属快照。
func (s *Store) Membership() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	for id, g := range s.groups {
		for _, uid := range g.Members {
			out[uid] = id
		}
	}
	return out
}

// Meta 返回 pool 选号所需的分组策略快照。
func (s *Store) Meta() map[string]Meta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]Meta{}
	for id, g := range s.groups {
		out[id] = Meta{Shared: g.SharedPool, CallPublic: g.CallPublic}
	}
	return out
}

// ResolveKey 按明文分组密钥解析分组。只做 SHA-256 + map 查找，不保存明文。
func (s *Store) ResolveKey(key string) (Group, bool) {
	key = strings.TrimSpace(key)
	if key == "" || !strings.HasPrefix(key, "sk-grp-") {
		return Group{}, false
	}
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])
	s.mu.RLock()
	id, ok := s.byKey[hash]
	var g *Group
	if ok {
		g = s.groups[id]
	}
	s.mu.RUnlock()
	if !ok || g == nil {
		return Group{}, false
	}
	return *cloneGroup(g), true
}

// GroupOf 返回账号当前所属分组 ID；未分组返回空串。
func (s *Store) GroupOf(uid string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, g := range s.groups {
		for _, m := range g.Members {
			if m == uid {
				return id
			}
		}
	}
	return ""
}

// CountMembers 返回分组账号数。
func (s *Store) CountMembers(id string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if g := s.groups[id]; g != nil {
		return len(g.Members)
	}
	return 0
}

func (s *Store) saveLocked() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	f := fileFormat{Version: 1, PublicProxy: s.publicProxy, Groups: make([]Group, 0, len(s.groups))}
	for _, g := range s.groups {
		f.Groups = append(f.Groups, *cloneGroup(g))
	}
	sort.Slice(f.Groups, func(i, j int) bool { return f.Groups[i].ID < f.Groups[j].ID })
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func newID() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "g_" + hex.EncodeToString(b), nil
}

func generateKey(id string) (key, hash, hint string, err error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", err
	}
	short := strings.TrimPrefix(id, "g_")
	if short == "" {
		short = "new"
	}
	key = "sk-grp-" + short + "_" + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(key))
	hash = hex.EncodeToString(sum[:])
	hint = key[:min(15, len(key))] + "..." + key[max(0, len(key)-4):]
	return key, hash, hint, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
func removeString(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
