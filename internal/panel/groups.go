// groups.go 面板分组管理 API：CRUD、密钥轮换、代理、账号归组。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

type proxyView struct {
	Type        string `json:"type"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"has_password,omitempty"`
}

type groupView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Color       string     `json:"color,omitempty"`
	SharedPool  bool       `json:"shared_pool"`
	CallPublic  bool       `json:"call_public"`
	HasKey      bool       `json:"has_key"`
	KeyHint     string     `json:"key_hint,omitempty"`
	Proxy       *proxyView `json:"proxy,omitempty"`
	MemberCount int        `json:"member_count"`
	Healthy     int        `json:"healthy"`
	Cooling     int        `json:"cooling"`
	Disabled    int        `json:"disabled"`
	Members     []string   `json:"members,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (p *Panel) groupViews() []groupView {
	all := p.cfg.Pool.List()
	out := make([]groupView, 0)
	if p.cfg.Groups == nil {
		return out
	}
	for _, g := range p.cfg.Groups.Groups() {
		v := groupView{
			ID: g.ID, Name: g.Name, Color: g.Color, SharedPool: g.SharedPool,
			CallPublic: g.CallPublic, HasKey: g.KeyHash != "", KeyHint: g.KeyHint,
			MemberCount: len(g.Members), Members: g.Members, CreatedAt: g.CreatedAt,
		}
		v.Proxy = toProxyView(g.Proxy)
		for _, a := range all {
			if a.Group != g.ID {
				continue
			}
			if a.Disabled {
				v.Disabled++
			} else if a.Cooling {
				v.Cooling++
			} else if !a.Paused {
				v.Healthy++
			}
		}
		out = append(out, v)
	}
	return out
}

func (p *Panel) applyGroupMeta() {
	if p.cfg.Groups == nil {
		return
	}
	meta := map[string]pool.GroupMeta{}
	for id, m := range p.cfg.Groups.Meta() {
		meta[id] = pool.GroupMeta{Shared: m.Shared, CallPublic: m.CallPublic}
	}
	p.cfg.Pool.SetGroupMeta(meta)
}

// publicGroupID 面板 API 中代表公共池的保留 id（真实分组 id 恒为 g_ 前缀，不冲突）。
const publicGroupID = "public"

func toProxyView(px *groups.Proxy) *proxyView {
	if px == nil {
		return nil
	}
	return &proxyView{Type: px.Type, Host: px.Host, Port: px.Port, Username: px.Username, HasPassword: px.Password != ""}
}

func (p *Panel) groupsList(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"groups": p.groupViews()}
	if p.cfg.Groups != nil {
		if v := toProxyView(p.cfg.Groups.PublicProxy()); v != nil {
			out["public_proxy"] = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *Panel) groupCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	var req struct {
		Name       string `json:"name"`
		Color      string `json:"color"`
		SharedPool bool   `json:"shared_pool"`
		CallPublic bool   `json:"call_public"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	g, key, err := p.cfg.Groups.Create(req.Name, req.Color, req.SharedPool, req.CallPublic)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p.applyGroupMeta()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": p.groupView(g.ID), "key": key})
}

func (p *Panel) groupUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	id := r.PathValue("id")
	var req struct {
		Name       string `json:"name"`
		Color      string `json:"color"`
		SharedPool bool   `json:"shared_pool"`
		CallPublic bool   `json:"call_public"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := p.cfg.Groups.Update(id, req.Name, req.Color, req.SharedPool, req.CallPublic); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p.applyGroupMeta()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": p.groupView(id)})
}

func (p *Panel) groupView(id string) groupView {
	for _, v := range p.groupViews() {
		if v.ID == id {
			return v
		}
	}
	return groupView{ID: id}
}

func (p *Panel) groupDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	id := r.PathValue("id")
	action := strings.TrimSpace(r.URL.Query().Get("account_action"))
	if action == "" {
		action = "move"
	}
	if action != "move" && action != "delete" {
		writeErr(w, http.StatusBadRequest, "account_action must be move or delete")
		return
	}
	g, err := p.cfg.Groups.Delete(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	for _, uid := range g.Members {
		if action == "delete" {
			if a := p.cfg.Pool.AuthByUID(uid); a != nil && strings.TrimSpace(a.FilePath) != "" {
				if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
					log.Printf("panel: delete auth file uid=%s: %v", uid, err)
				}
			}
			p.cfg.Pool.Remove(uid)
			continue
		}
		p.cfg.Pool.SetGroup(uid, "")
	}
	p.applyGroupMeta()
	if p.cfg.Upstream != nil {
		p.cfg.Upstream.InvalidateGroup(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id, "accounts": len(g.Members), "action": action})
}

func (p *Panel) groupRotateKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	key, err := p.cfg.Groups.RotateKey(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
}

func proxyFromRequest(r *http.Request) (*groups.Proxy, error) {
	var req struct {
		Type     string `json:"type"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Host) == "" {
		return nil, fmt.Errorf("proxy host is required")
	}
	t := strings.ToLower(strings.TrimSpace(req.Type))
	if t != "http" && t != "socks5" && t != "socks5h" {
		return nil, fmt.Errorf("proxy type must be http/socks5/socks5h")
	}
	if req.Port < 1 || req.Port > 65535 {
		return nil, fmt.Errorf("proxy port invalid")
	}
	return &groups.Proxy{Type: t, Host: strings.TrimSpace(req.Host), Port: req.Port, Username: strings.TrimSpace(req.Username), Password: req.Password}, nil
}

func (p *Panel) groupSetProxy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	id := r.PathValue("id")
	proxy, err := proxyFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 空密码保留旧密码，避免前端编辑时只显示掩码导致密码被清空。
	if id == publicGroupID {
		if old := p.cfg.Groups.PublicProxy(); proxy.Password == "" && old != nil {
			proxy.Password = old.Password
		}
		if err := p.cfg.Groups.SetPublicProxy(proxy); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if p.cfg.Upstream != nil {
			p.cfg.Upstream.InvalidateGroup("")
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "public_proxy": toProxyView(p.cfg.Groups.PublicProxy())})
		return
	}
	old, ok := p.cfg.Groups.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	if proxy.Password == "" && old.Proxy != nil {
		proxy.Password = old.Proxy.Password
	}
	if err := p.cfg.Groups.SetProxy(id, proxy); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.cfg.Upstream != nil {
		p.cfg.Upstream.InvalidateGroup(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": p.groupView(id)})
}

// groupDeleteProxy 清除代理，恢复直连。id=public 表示公共池。
func (p *Panel) groupDeleteProxy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	id := r.PathValue("id")
	var err error
	cacheKey := id
	if id == publicGroupID {
		err = p.cfg.Groups.SetPublicProxy(nil)
		cacheKey = ""
	} else {
		err = p.cfg.Groups.SetProxy(id, nil)
	}
	if err != nil {
		status := http.StatusBadRequest
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	if p.cfg.Upstream != nil {
		p.cfg.Upstream.InvalidateGroup(cacheKey)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// groupRevealKey 返回分组密钥明文；旧分组只存哈希时返回 409，需轮换后才能查看。
func (p *Panel) groupRevealKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	id := r.PathValue("id")
	if _, ok := p.cfg.Groups.Get(id); !ok {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	key, ok := p.cfg.Groups.RevealKey(id)
	if !ok {
		writeErr(w, http.StatusConflict, "旧分组只保存了密钥哈希，无法查看明文；请轮换密钥后再查看")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
}

func (p *Panel) groupTestProxy(w http.ResponseWriter, r *http.Request) {
	proxy, err := proxyFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := upstream.ProxyConfig{Type: proxy.Type, Host: proxy.Host, Port: proxy.Port, Username: proxy.Username, Password: proxy.Password}
	targets := []string{"https://copilot.tencent.com/", "https://www.workbuddy.ai/"}
	var lastErr error
	for _, target := range targets {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		latency, status, err := upstream.TestProxy(ctx, cfg, target, 10*time.Second)
		cancel()
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": latency.Milliseconds(), "status": status, "target": target})
			return
		}
		lastErr = err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": lastErr.Error()})
}

func (p *Panel) accountSetGroup(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	uid := r.PathValue("uid")
	var req struct {
		Group string `json:"group"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.cfg.Pool.AuthByUID(uid) == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	groupID := strings.TrimSpace(req.Group)
	if groupID == publicGroupID {
		groupID = ""
	}
	if p.cfg.Groups.GroupOf(uid) == groupID {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "group": groupID, "unchanged": true})
		return
	}
	if groupID != "" {
		if _, ok := p.cfg.Groups.Get(groupID); !ok {
			writeErr(w, http.StatusBadRequest, "unknown group")
			return
		}
		if err := p.cfg.Groups.AddMember(groupID, uid); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		if old := p.cfg.Groups.GroupOf(uid); old != "" {
			if err := p.cfg.Groups.RemoveMember(old, uid); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	p.cfg.Pool.SetGroup(uid, groupID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "group": groupID})
}

// exportAccount 面板导出格式（workbuddy-accounts v1，camelCase），可被 import/cockpit 原样导入。
type exportAccount struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	Realm        string `json:"realm,omitempty"`
	Domain       string `json:"domain,omitempty"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Group        string `json:"group,omitempty"`
}

// accountsExport 导出所选账号凭证（含 access/refresh token，属敏感数据，仅鉴权后可用）。
//
//	POST /panel/api/accounts/export  {"uids":["..."]}
func (p *Panel) accountsExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs []string `json:"uids"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids is required")
		return
	}
	seen := map[string]bool{}
	out := make([]exportAccount, 0, len(req.UIDs))
	var missing []string
	for _, uid := range req.UIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		a := p.cfg.Pool.AuthByUID(uid)
		if a == nil {
			missing = append(missing, uid)
			continue
		}
		out = append(out, exportAccount{
			UID: a.UID, Nickname: a.Nickname, Realm: a.Realm(), Domain: a.Domain,
			EnterpriseID: a.EnterpriseID, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken,
			ExpiresAt: a.ExpiresAt, Group: a.Group(),
		})
	}
	if len(out) == 0 {
		writeErr(w, http.StatusNotFound, "no matching accounts")
		return
	}
	log.Printf("panel: export accounts count=%d missing=%d", len(out), len(missing))
	name := fmt.Sprintf("workbuddy-accounts-%s.json", time.Now().Format("20060102-150405"))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	writeJSON(w, http.StatusOK, map[string]any{
		"format": "workbuddy-accounts", "version": 1, "exported_at": time.Now().Unix(),
		"accounts": out, "missing": missing,
	})
}

func decodeJSONBody(r *http.Request, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(raw) == 0 {
		return fmt.Errorf("empty body")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return nil
}
