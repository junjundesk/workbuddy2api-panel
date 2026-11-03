package panel

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/prompt"
)

// 单分组配置文件的格式标识与版本。
//
// 之所以单独定义格式而不是直接导出 groups.json 里的 Group：Group 里混着"这台实例的
// 身份"（id、分组密钥、成员、创建时间）与"可以搬走的策略"（代理、共享开关、提示词）。
// 配置文件只装后者，格式与版本号一起给未来留升级位；导入侧对未知格式直接拒绝，
// 避免把别的工具的 JSON 当成配置静默写进 groups.json。
const (
	groupConfigFormat  = "workbuddy-group-config"
	groupConfigVersion = 1
)

// groupConfigPrompt 分组提示词规则的导出形。
//
// Text 是旧版单条系统提示词的兼容入口：实时导出一律走 entries/replaces（旧分组由
// PromptText 折算成一条 system 条目），Text 只在导入比 v1 更老的配置文件时兜底。
type groupConfigPrompt struct {
	Enabled  bool             `json:"enabled"`
	Text     string           `json:"text,omitempty"`
	Entries  []prompt.Entry   `json:"entries"`
	Replaces []prompt.Replace `json:"replaces"`
}

// groupConfigPayload 配置文件里的分组主体。
//
// Name/Color 只作为导出时的来源标注（人看文件时能认出是哪组），导入**不**采用：
// 导入到已存在的分组时改名/改色属于意外副作用，前端确认弹窗也只展示来源分组名。
// Proxy 不带 omitempty：null 是有意义的取值（直连），有它导入才能把目标分组的
// 代理清掉——缺字段与"显式直连"在导入侧是同一语义，但导出必须把话说满。
type groupConfigPayload struct {
	Name       string            `json:"name,omitempty"`
	Color      string            `json:"color,omitempty"`
	SharedPool bool              `json:"shared_pool"`
	CallPublic bool              `json:"call_public"`
	Proxy      *groups.Proxy     `json:"proxy"`
	Prompt     groupConfigPrompt `json:"prompt"`
}

type groupConfigFile struct {
	Format     string              `json:"format"`
	Version    int                 `json:"version"`
	ExportedAt int64               `json:"exported_at,omitempty"`
	Group      *groupConfigPayload `json:"group"`
}

// groupExportConfig 导出单个分组的可搬配置（网络代理 + 共享开关 + 提示词规则）。
//
// 代理密码**按原样导出**：导出的目的就是把分组原封搬到另一台实例，脱敏后的文件
// 到了对面还得手工补密码。前端下载前有明确提示，README 也标注为敏感文件。
// 分组密钥/成员不在导出范围，见 groupConfigPayload 的说明。
func (p *Panel) groupExportConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	id := r.PathValue("id")
	g, ok := p.cfg.Groups.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	rs := prompt.Rules{Entries: g.PromptEntries, Replaces: g.PromptReplaces}.Normalize()
	if len(rs.Entries) == 0 && strings.TrimSpace(g.PromptText) != "" {
		rs.Entries = []prompt.Entry{{Role: prompt.RoleSystem, Text: g.PromptText}}
	}
	if rs.Entries == nil {
		rs.Entries = []prompt.Entry{}
	}
	if rs.Replaces == nil {
		rs.Replaces = []prompt.Replace{}
	}
	var px *groups.Proxy
	if g.Proxy != nil {
		cp := *g.Proxy
		px = &cp
	}
	out := groupConfigFile{
		Format:     groupConfigFormat,
		Version:    groupConfigVersion,
		ExportedAt: time.Now().Unix(),
		Group: &groupConfigPayload{
			Name:       g.Name,
			Color:      g.Color,
			SharedPool: g.SharedPool,
			CallPublic: g.CallPublic,
			Proxy:      px,
			Prompt:     groupConfigPrompt{Enabled: g.PromptEnabled, Entries: rs.Entries, Replaces: rs.Replaces},
		},
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
	log.Printf("panel: 分组配置已导出 id=%s proxy=%v prompt=%v entries=%d replaces=%d（含代理密码）",
		id, px != nil, g.PromptEnabled, len(rs.Entries), len(rs.Replaces))
}

// groupImportConfig 把配置文件原子地覆盖到目标分组。
//
// 覆盖语义：文件里没写的字段取零值（proxy=null → 直连、prompt 为空 → 关闭但保留
// 空规则、shared_pool/call_public=false），即"目标分组变成文件描述的样子"，而不是
// 与现有配置合并——合并会让人没法靠导入把分组还原成另一组的配置。
//
// 校验顺序：格式/版本/主体 → 规则归一 → Store.SetConfig（内部再校验代理与提示词
// 上限，并保证落盘失败时全量回滚）。分组不存在映射 404，其余 400 由前端展示。
func (p *Panel) groupImportConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Groups == nil {
		writeErr(w, http.StatusNotImplemented, "groups not available")
		return
	}
	id := r.PathValue("id")
	var f groupConfigFile
	if err := decodeJSONBody(r, &f); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(f.Format) != "" && f.Format != groupConfigFormat {
		writeErr(w, http.StatusBadRequest, "unsupported config format: "+f.Format)
		return
	}
	if f.Version < 0 || f.Version > groupConfigVersion {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unsupported config version %d (panel supports up to %d)", f.Version, groupConfigVersion))
		return
	}
	if f.Group == nil {
		writeErr(w, http.StatusBadRequest, "config file has no group object")
		return
	}
	rs := prompt.Rules{Entries: f.Group.Prompt.Entries, Replaces: f.Group.Prompt.Replaces}.Normalize()
	if rs.Empty() && strings.TrimSpace(f.Group.Prompt.Text) != "" {
		rs.Entries = []prompt.Entry{{Role: prompt.RoleSystem, Text: f.Group.Prompt.Text}}
	}
	err := p.cfg.Groups.SetConfig(id, groups.Config{
		SharedPool:    f.Group.SharedPool,
		CallPublic:    f.Group.CallPublic,
		Proxy:         f.Group.Proxy,
		PromptEnabled: f.Group.Prompt.Enabled,
		Rules:         rs,
	})
	if err != nil {
		status := http.StatusBadRequest
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	// 共享池/公共池开关会改变选号范围，必须同步给 pool（否则导入后要重启才生效）。
	p.applyGroupMeta()
	if p.cfg.Upstream != nil {
		p.cfg.Upstream.InvalidateGroup(id)
	}
	log.Printf("panel: 分组配置已导入 id=%s proxy=%v shared=%v call_public=%v prompt=%v entries=%d replaces=%d",
		id, f.Group.Proxy != nil, f.Group.SharedPool, f.Group.CallPublic, f.Group.Prompt.Enabled, len(rs.Entries), len(rs.Replaces))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": p.groupView(id)})
}
