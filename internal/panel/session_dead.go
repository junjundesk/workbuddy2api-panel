// session_dead.go 上游凭证失效（HTTP 401 / ErrSessionDead，常见 body
// {"code":12153,"msg":"Offline user session not found"}）的实时归组：
// 命中时把账号移入隔离分组「失效池」（shared_pool=false、call_public=false），
// 接口响应带 dead / moved_to 标记供前端逐行展示。
//
// 与「11140 封号 → 封号池」（patrol.NotifyAccountBanned）同一范式，区别是
// 凭证失效属于确定性终态：同步执行、不挂巡检开关、不禁用账号（「失效池」可人工搬回）。
package panel

import (
	"errors"
	"log"
	"net/http"
	"sync"

	"github.com/junjundesk/workbuddy2api-panel/internal/logfmt"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

const (
	// deadGroupName 凭证失效账号的隔离分组名（不存在时自动创建）。
	deadGroupName = "失效池"
	// deadGroupColor 「失效池」标识色：深红，与封号池 #e5484d、无积分 #f5a623 区分。
	deadGroupColor = "#b91c1c"
)

// deadMoveMu 串行化「查归属 + 建组 + 搬移」：并发命中同一账号时只落盘一次；
// 同名组防重由 ensurePatrolGroup 的 patrolGroupMu 负责。
var deadMoveMu sync.Mutex

// isSessionDeadErr 判定上游凭证失效：errors.As 到 *upstream.Error 后要求
// HTTP 401 或分类为 ErrSessionDead。依赖结构化错误分类，不做裸字符串匹配，
// 避免把 500 / 限流等其他失败误判为失效。
func isSessionDeadErr(err error) bool {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return false
	}
	return ue.Status == http.StatusUnauthorized || ue.Kind == upstream.ErrSessionDead
}

// NotifyAccountDead 凭证失效实时归组：把 uid 移入「失效池」（分组不存在则自动创建，
// 隔离分组 shared_pool=false、call_public=false）。返回被移入的分组名。
//
// 幂等：账号已在「失效池」时不重复建组、不重复搬移、不重复落盘。返回值语义——
// 调用后账号位于「失效池」（刚搬入或本就在，幂等命中）返回 deadGroupName，
// 让响应的 moved_to 标记对前端稳定；仅在不适用/失败（Panel/Groups/Pool 未装配、
// uid 为空、建组或搬移失败）返回空串。
//
// 同步执行（单号操作，一次 groups.json 小文件写）；批量/主链路回调路径请自行异步化。
func (p *Panel) NotifyAccountDead(uid, reason string) string {
	if p == nil || p.cfg.Groups == nil || p.cfg.Pool == nil || uid == "" {
		return ""
	}
	deadMoveMu.Lock()
	defer deadMoveMu.Unlock()
	gid := p.ensurePatrolGroup(deadGroupName, deadGroupColor)
	if gid == "" {
		log.Printf("panel: 凭证失效归组失败（创建「%s」失败） uid=%s", deadGroupName, logfmt.Label(uid, ""))
		return ""
	}
	if p.cfg.Groups.GroupOf(uid) == gid {
		return deadGroupName // 幂等：已在「失效池」，不重复搬移/落盘
	}
	nickname := ""
	if a := p.cfg.Pool.AuthByUID(uid); a != nil {
		nickname = a.Nickname
	}
	if err := p.patrolMove(uid, gid); err != nil {
		log.Printf("panel: 账号 %s 凭证失效，移入「%s」失败: %v", logfmt.Label(uid, nickname), deadGroupName, err)
		return ""
	}
	detail := ""
	if reason != "" {
		detail = "（" + reason + "）"
	}
	log.Printf("panel: 账号 %s 凭证失效%s，已实时移入「%s」", logfmt.Label(uid, nickname), detail, deadGroupName)
	return deadGroupName
}
