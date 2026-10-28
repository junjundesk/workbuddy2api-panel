// 分组作用域：账号归属（group）+ 选号策略元数据（shared/call_public）。
package pool

// GroupMeta 是分组对 pool 可见的策略快照（不包含密钥/代理等敏感配置）。
type GroupMeta struct {
	Shared     bool // 本组账号进入公共池
	CallPublic bool // 本组与公共池**共用候选集**（同一权重池参与抽签，不是"无号时回源"）
}

// GroupScope 选号作用域。零值 = 全部账号（兼容旧 Pick 语义）。
// Group 非空 = 分组专属；Public = 公共池（未分组 + shared 分组）；
// Union 与 Group 同用 = 本组 ∪ 公共池（两组账号进同一个候选集，一次加权抽签）。
type GroupScope struct {
	Group  string
	Public bool
	Union  bool
}

// SetGroups 用 uid→group 的权威快照覆盖池内账号归属，并同步到 auth 运行期字段。
// 未出现在 membership 的账号归公共池（group=""）。
func (p *Pool) SetGroups(membership map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, e := range p.byUID {
		g := membership[uid]
		e.group = g
		if e.a != nil {
			e.a.SetGroup(g)
		}
	}
	p.dirty.Store(true)
}

// SetGroup 设置单账号归属（空 = 公共池）。
func (p *Pool) SetGroup(uid, group string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.group = group
	if e.a != nil {
		e.a.SetGroup(group)
	}
	p.dirty.Store(true)
	return true
}

// GroupOf 返回账号当前分组 ID（空 = 公共池）。
func (p *Pool) GroupOf(uid string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.group
	}
	return ""
}

// SetGroupMeta 覆盖分组策略快照（shared / call_public）。
func (p *Pool) SetGroupMeta(meta map[string]GroupMeta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.groupMeta = meta
}

// publicMember 报告账号是否属于公共池（未分组账号 + 开启了 shared 的分组账号）。
// 调用方必须已持 p.mu。
func (p *Pool) publicMember(e *entry) bool {
	if e.group == "" {
		return true
	}
	if m, ok := p.groupMeta[e.group]; ok {
		return m.Shared
	}
	return false
}

// scopeMatch 判断账号是否属于选号作用域。调用方必须已持 p.mu。
//
// Union 的语义是**并集**（本组 ∪ 公共池），不是「本组优先、无号再回源」的两段式：
// 一次加权抽签在并集内进行，公共池账号在本组健康时同样可能被选中。
func (p *Pool) scopeMatch(e *entry, scope GroupScope) bool {
	if e == nil {
		return false
	}
	if scope.Group != "" {
		if e.group == scope.Group {
			return true
		}
		return scope.Union && p.publicMember(e)
	}
	if scope.Public {
		return p.publicMember(e)
	}
	return true
}

// publicScopeMatch 公共池候选：未分组 + shared 分组。调用方必须已持 p.mu。
func (p *Pool) publicScopeMatch(e *entry) bool {
	return p.scopeMatch(e, GroupScope{Public: true})
}
