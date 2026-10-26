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
	KeyHash    string `json:"key_hash,omitempty"`
	KeyHint    string `json:"key_hint,omitempty"`
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
