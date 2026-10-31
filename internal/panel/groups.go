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
	"unicode/utf8"

	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/prompt"
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
	// 分组专属系统提示词：列表只回元数据（开关 / 字数 / 首 60 字预览），
	// 正文按需 GET /panel/api/groups/{id}/prompt —— 分组页每次操作都会重拉列表，
	// 把最长 64 KiB 的正文塞进列表没有展示价值。
	PromptEnabled  bool   `json:"prompt_enabled"`
	PromptChars    int    `json:"prompt_chars,omitempty"`
	PromptPreview  string `json:"prompt_preview,omitempty"`
	PromptEntries  int    `json:"prompt_entries,omitempty"`
	PromptReplaces int    `json:"prompt_replaces,omitempty"`
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
		v.PromptEnabled = g.PromptEnabled
		if g.PromptEnabled {
			v.PromptChars = utf8.RuneCountInString(groupPromptText(g))
			v.PromptPreview = promptPreview(groupPromptText(g))
			v.PromptEntries = len(g.PromptEntries)
			if v.PromptEntries == 0 && strings.TrimSpace(g.PromptText) != "" {
				v.PromptEntries = 1 // 旧版单条文本也算一条
			}
			v.PromptReplaces = len(g.PromptReplaces)
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

// groupPromptText 返回分组配置里的系统提示词正文（新版条目优先，旧版单条回退）。
// 只用于面板展示与字数统计，不参与请求改写（请求侧走 groups.Rules）。
func groupPromptText(g groups.Group) string {
	if len(g.PromptEntries) == 0 {
		return g.PromptText
	}
	rs := prompt.Rules{Entries: g.PromptEntries}
	if text := rs.SystemText(); text != "" {
		return text
	}
	return g.PromptText
}

// promptPreviewRunes 分组列表里提示词预览的字符数上限。
const promptPreviewRunes = 60

// promptPreview 返回提示词预览：首 promptPreviewRunes 个字符，换行/制表压成空格，
// 截断时补省略号。按 rune 遍历，多字节/emoji 不会被切坏。
func promptPreview(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range text {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			r = ' '
		}
		b.WriteRune(r)
		n++
		if n >= promptPreviewRunes {
			b.WriteString("…")
			break
		}
	}
	return b.String()
}

// groupGetPrompt 返回分组提示词规则的全文（分组页打开编辑器时按需拉取）。
//
// 同时返回三段口径，前端按需使用：
//   - entries / replaces：新版多条规则（编辑器回填的唯一来源）；
//   - text / chars：系统提示词合并后的文本与字数（旧前端与卡片预览仍可用）。
func (p *Panel) groupGetPrompt(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	g, ok := p.cfg.Groups.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	rs := prompt.Rules{Entries: g.PromptEntries, Replaces: g.PromptReplaces}.Normalize()
	if len(g.PromptEntries) == 0 && strings.TrimSpace(g.PromptText) != "" {
		rs.Entries = []prompt.Entry{{Role: prompt.RoleSystem, Text: g.PromptText}}
	}
	if rs.Entries == nil {
		rs.Entries = []prompt.Entry{}
	}
	if rs.Replaces == nil {
		rs.Replaces = []prompt.Replace{}
	}
	text := rs.SystemText()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"enabled":  g.PromptEnabled,
		"text":     text,
		"chars":    utf8.RuneCountInString(text),
		"entries":  rs.Entries,
		"replaces": rs.Replaces,
	})
}

// groupSetPrompt 保存分组提示词规则（开关 + 多条提示词 + 关键词替换）。
//
// 兼容两种请求体：
//   - {"enabled":true,"text":"..."}：旧版单条系统提示词（旧面板 / 脚本）；
//   - {"enabled":true,"entries":[...],"replaces":[...]}：新版规则（只要出现任一新
//     字段就按新规则处理，text 不再写盘——避免新旧两套正文并存后口径漂移）。
//
// 生效时机：groups.Store 每次写操作同步落盘并更新内存，请求侧每个请求读一次
// Store.Rules —— 保存后的下一个请求即生效，无需重启（与分组代理同理）。
func (p *Panel) groupSetPrompt(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	id := r.PathValue("id")
	var req struct {
		Enabled  bool             `json:"enabled"`
		Text     *string          `json:"text"`
		Entries  []prompt.Entry   `json:"entries"`
		Replaces []prompt.Replace `json:"replaces"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var err error
	rulesMode := req.Entries != nil || req.Replaces != nil
	if rulesMode {
		err = p.cfg.Groups.SetPromptRules(id, req.Enabled, prompt.Rules{Entries: req.Entries, Replaces: req.Replaces})
	} else {
		text := ""
		if req.Text != nil {
			text = *req.Text
		}
		err = p.cfg.Groups.SetPrompt(id, req.Enabled, text)
	}
	if err != nil {
		status := http.StatusBadRequest
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	g, _ := p.cfg.Groups.Get(id)
	rs := prompt.Rules{}
	if g.ID != "" {
		rs = prompt.Rules{Entries: g.PromptEntries, Replaces: g.PromptReplaces}.Normalize()
	}
	log.Printf("panel: 分组提示词规则已保存 id=%s enabled=%v entries=%d replaces=%d chars=%d（下一请求生效）",
		id, req.Enabled, len(rs.Entries), len(rs.Replaces), utf8.RuneCountInString(rs.SystemText()))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": p.groupView(id)})
}

// groupTestProxy 面板「测试连接」：并行验证两件事——
//  1. 这条代理能不能连通上游白名单站点（耗时 + HTTP 状态）；
//  2. 它实际从哪个 IP、哪个地区出去（经 ipip.net 探测）。
//
// 并行是因为串行会把单次测试拖到 20 秒以上；两者互不依赖，任一侧失败都不影响
// 另一侧的结论——代理只对部分域名生效时，这个差异正是用户要看到的信息。
func (p *Panel) groupTestProxy(w http.ResponseWriter, r *http.Request) {
	proxy, err := proxyFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := upstream.ProxyConfig{Type: proxy.Type, Host: proxy.Host, Port: proxy.Port, Username: proxy.Username, Password: proxy.Password}
	targets := []string{"https://copilot.tencent.com/", "https://www.workbuddy.ai/"}

	type connResult struct {
		latency time.Duration
		status  int
		target  string
		err     error
	}
	type ipResult struct {
		info    upstream.ExitInfo
		latency time.Duration
		err     error
	}
	connCh := make(chan connResult, 1)
	ipCh := make(chan ipResult, 1)

	go func() {
		var lastErr error
		for _, target := range targets {
			ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
			latency, status, err := upstream.TestProxy(ctx, cfg, target, 10*time.Second)
			cancel()
			if err == nil {
				connCh <- connResult{latency: latency, status: status, target: target}
				return
			}
			lastErr = err
		}
		connCh <- connResult{err: lastErr}
	}()

	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		start := time.Now()
		info, err := upstream.ProbeExitIP(ctx, cfg, 8*time.Second)
		ipCh <- ipResult{info: info, latency: time.Since(start), err: err}
	}()

	conn := <-connCh
	ip := <-ipCh

	out := map[string]any{"ip_latency_ms": ip.latency.Milliseconds()}
	if ip.err == nil {
		out["exit_ip"] = ip.info.IP
		if ip.info.Region != "" {
			out["exit_region"] = ip.info.Region
		}
	} else {
		// 探测失败只影响这一行提示，不改变连通性结论。
		out["ip_error"] = ip.err.Error()
	}
	if conn.err != nil {
		out["ok"] = false
		out["error"] = conn.err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["ok"] = true
	out["latency_ms"] = conn.latency.Milliseconds()
	out["status"] = conn.status
	out["target"] = conn.target
	writeJSON(w, http.StatusOK, out)
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
	// GroupName 分组显示名（导出时快照，便于人工核对；导入侧忽略）。
	GroupName string `json:"groupName,omitempty"`
}

// accountsExport 导出账号凭证（含 access/refresh token，属敏感数据，仅鉴权后可用）。
//
//	POST /panel/api/accounts/export  {"uids":["..."]}
//	POST /panel/api/accounts/export  {"groups":["g_xxx","public"]}
//
// uids 与 groups 可同时给，两者取并集（uid 去重）。groups 支持真实分组 id 与保留
// id "public"（= 未分组账号 + 开启共享的分组账号，与面板「公共号池」口径一致），
// 用来支持「按分组导出」与「跨分组多选导出」。
func (p *Panel) accountsExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs   []string `json:"uids"`
		Groups []string `json:"groups"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 分组 id 先校验：未知分组直接报错（否则会静默导出空集，让人以为"没账号"）。
	if bad := p.unknownGroupIDs(req.Groups); len(bad) > 0 {
		writeErr(w, http.StatusBadRequest, "unknown group: "+strings.Join(bad, ", "))
		return
	}
	uids := make([]string, 0, len(req.UIDs)+16)
	uids = append(uids, req.UIDs...)
	uids = append(uids, p.exportUIDsForGroups(req.Groups)...)
	if len(uids) == 0 {
		writeErr(w, http.StatusBadRequest, "uids or groups is required")
		return
	}
	seen := map[string]bool{}
	out := make([]exportAccount, 0, len(uids))
	var missing []string
	for _, uid := range uids {
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
			ExpiresAt: a.ExpiresAt, Group: a.Group(), GroupName: p.groupNameOf(a.Group()),
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
		"accounts": out, "missing": missing, "groups": req.Groups,
	})
}

// unknownGroupIDs 找出请求里不存在的分组 id（"public" 视为合法保留 id）。
func (p *Panel) unknownGroupIDs(ids []string) []string {
	out := []string{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || id == publicGroupID {
			continue
		}
		if p.cfg.Groups == nil {
			out = append(out, id)
			continue
		}
		if _, ok := p.cfg.Groups.Get(id); !ok {
			out = append(out, id)
		}
	}
	return out
}

// exportUIDsForGroups 按分组 id 展开账号 uid 列表。保留 id "public" 表示
// 公共号池：未分组账号 + 开启「进入公共池」的分组账号（与面板口径一致）。
// 未知分组由调用方 unknownGroupIDs 事先挡掉，这里的跳过分支只是防御。
func (p *Panel) exportUIDsForGroups(ids []string) []string {
	out := []string{}
	if len(ids) == 0 {
		return out
	}
	shared := map[string]bool{}
	if p.cfg.Groups != nil {
		for _, g := range p.cfg.Groups.Groups() {
			if g.SharedPool {
				shared[g.ID] = true
			}
		}
	}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if id == publicGroupID {
			for _, a := range p.cfg.Pool.List() {
				if a.Group == "" || shared[a.Group] {
					out = append(out, a.UID)
				}
			}
			continue
		}
		if p.cfg.Groups == nil {
			continue
		}
		if _, ok := p.cfg.Groups.Get(id); !ok {
			continue
		}
		// 用池内实际分组归属（SetGroup 写入），而不是 groups.json 的 members 列表：
		// 后者可能与池状态短暂不同步（移动账号时先改池再改存储）。
		for _, a := range p.cfg.Pool.List() {
			if a.Group == id {
				out = append(out, a.UID)
			}
		}
	}
	return out
}

// groupNameOf 分组显示名；未分组账号记为「公共号池（未分组）」。
func (p *Panel) groupNameOf(id string) string {
	if id == "" {
		return "公共号池（未分组）"
	}
	if p.cfg.Groups != nil {
		if g, ok := p.cfg.Groups.Get(id); ok && g.Name != "" {
			return g.Name
		}
	}
	return id
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
