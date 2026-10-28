package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
)

// authScope 一次请求的号池作用域。Main=true 表示主密钥/匿名公共池。
type authScope struct {
	GroupID    string
	CallPublic bool
	Main       bool
}

type scopeCtxKey struct{}

func scopeOf(ctx context.Context) authScope {
	if v, ok := ctx.Value(scopeCtxKey{}).(authScope); ok {
		return v
	}
	return authScope{Main: true}
}

func scopeFrom(r *http.Request) authScope { return scopeOf(r.Context()) }

// withScope 把解析后的作用域挂到请求 context，供 handler 选号/状态过滤使用。
func withScope(r *http.Request, s authScope) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), scopeCtxKey{}, s))
}

// groupAllowsAccount 报告账号是否属于请求作用域。公共池 = 未分组 + shared 分组。
func (h *Handler) groupAllowsAccount(scope authScope, a *auth.Auth) bool {
	if a == nil {
		return false
	}
	gid := a.Group()
	if scope.GroupID != "" {
		if gid == scope.GroupID {
			return true
		}
		if !scope.CallPublic {
			return false
		}
		// 开启「调用公共池」时接受公共池/共享组的粘性账号：语义是**共用候选集**
		// （本组 ∪ 公共池的并集），不是"本组无号才回源"——本组健康时公共池号
		// 同样在同一次加权抽签里被选中，粘性绑定也照常保持。
	}
	if gid == "" {
		return true
	}
	return h.groupShared(gid)
}

func (h *Handler) groupShared(gid string) bool {
	if gid == "" || h.cfg.Groups == nil {
		return false
	}
	g, ok := h.cfg.Groups.Get(gid)
	return ok && g.SharedPool
}

func bearerToken(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
}

// countStatusList 按 /status 口径统计一组账号状态（in_flight_full 无上限信息，记 0）。
func countStatusList(list []pool.Status) (total, healthy, cooling, disabled, paused int) {
	total = len(list)
	for _, a := range list {
		if a.Disabled {
			disabled++
			continue
		}
		if a.Paused {
			paused++
		}
		if a.Cooling {
			cooling++
			continue
		}
		if !a.Paused {
			healthy++
		}
	}
	return total, healthy, cooling, disabled, paused
}

func realmCountsMap(list []pool.Status) map[string]map[string]int {
	out := map[string]map[string]int{
		"cn":     {"total": 0, "healthy": 0, "cooling": 0, "disabled": 0, "in_flight_full": 0},
		"global": {"total": 0, "healthy": 0, "cooling": 0, "disabled": 0, "in_flight_full": 0},
	}
	for _, a := range list {
		realm := a.Realm
		if realm != "global" {
			realm = "cn"
		}
		m := out[realm]
		m["total"]++
		if a.Disabled {
			m["disabled"]++
		} else if a.Cooling {
			m["cooling"]++
		} else if !a.Paused {
			m["healthy"]++
		}
	}
	return out
}
