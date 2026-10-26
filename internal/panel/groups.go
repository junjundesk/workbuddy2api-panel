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

	"github.com/linguo2625469/workbuddy2api-panel/internal/groups"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
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
		if g.Proxy != nil {
			v.Proxy = &proxyView{Type: g.Proxy.Type, Host: g.Proxy.Host, Port: g.Proxy.Port, Username: g.Proxy.Username, HasPassword: g.Proxy.Password != ""}
		}
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

func (p *Panel) groupsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"groups": p.groupViews()})
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
	groupID := strings.TrimSpace(req.Group)
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
