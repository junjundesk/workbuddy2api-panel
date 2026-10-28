// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
const expiringVirtualSlots = 3

// scopeFor 把 (group, callPublic) 折成选号作用域，是「分组作用域」的唯一构造点：
//   - group==""            → 公共池（未分组 + shared 分组）；
//   - group!="", callPublic → 本组 ∪ 公共池（**并集**，同一候选集）；
//   - group!="", 否则       → 本组专属。
//
// 与旧实现的关键差别：callPublic 不是「本组无号时回源」的两段式，而是并集——
// 本组健康时公共池账号仍在同一权重池里参与抽签。
func (p *Pool) scopeFor(group string, callPublic bool) GroupScope {
	if group == "" {
		return GroupScope{Public: true}
	}
	return GroupScope{Group: group, Union: callPublic}
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
// AvailableUIDsForRealmScoped 同 AvailableUIDsForRealm，并叠加分组作用域
// （group/callPublic 语义见 scopeFor）。
func (p *Pool) AvailableUIDsForRealmScoped(realm, group string, callPublic bool) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	scope := p.scopeFor(group, callPublic)
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !p.scopeMatch(e, scope) {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// WeightedAvailableUIDsForModelRealmScoped 同 WeightedAvailableUIDsForModelRealm，
// 并叠加分组作用域；public 会话用它保持快过期账号虚拟实例权重，避免升级后
// 公共池会话分配口径变化。
func (p *Pool) WeightedAvailableUIDsForModelRealmScoped(model, realm, group string, callPublic bool) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	scope := p.scopeFor(group, callPublic)
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !p.scopeMatch(e, scope) {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealmScoped 同 AvailableUIDsForModelRealm，并叠加分组作用域。
// group=="" = 公共池（未分组 + shared 分组）；供公共池会话粘性可用集使用，
// 避免把未共享分组的私有账号绑定进公共会话。
func (p *Pool) AvailableUIDsForModelRealmScoped(model, realm, group string, callPublic bool) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	scope := p.scopeFor(group, callPublic)
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !p.scopeMatch(e, scope) {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
