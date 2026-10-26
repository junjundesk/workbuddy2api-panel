// wafip.go WAF IP 级拦截 fail-fast 状态机（任务书 waf-ip-failfast）。
//
// 背景（fork-scan-absorb T-1 / BulidH 实测）：WAF 403 拦的是网关出口 IP 而非
// 账号——3 个账号 1 秒内全 403。既有 ErrWafBlock 账号级软冷却（wafCooldownBase
// 起的有界退避）在 IP 级拦截时不够：轮转会把一次客户端请求放大 MaxRotate 倍，
// 同一出口 IP 继续打上游只会加重风控。
//
// 判定：ErrWafBlock 基础上的短窗多号计数——wafIPWindow（60s 滑动窗）内
// ≥ wafIPThreshold 个**不同** UID 接连命中 WAF 403 → 判定 IP 级拦截，激活至
// now+wafIPWindow。单号反复 403（账号级偶发）永不触发：只数不同号。
// 激活期内新命中不续期（保守：不做主动探测，窗口自然解除）。
//
// 归属层评估：放 server（Handler 局部）而非 pool——IP 级状态唯一消费者是
// chatCompletions 轮转循环（是否继续轮转），pool 是账号级记账层，跨 UID 语义
// 不属于任何账号；server 已有进程级状态先例 degradeGate（mu+until 同风格）。
// 账号级软冷却照常记账（applyErrorPolicy 不变），IP 级状态只改变「是否继续
// 轮转」——协同不叠加。进程内状态、重启清零（窗口 60s，重建成本极低）。
package server

import (
	"log"
	"sync"
	"time"
)

// wafIPWindow IP 级判定滑动窗 + 激活时长：窗内不同账号命中 WAF 403 达阈值即
// 判 IP 级拦截，激活同样长（到期自然解除）。var 仅供测试注入短窗（生产恒 60s，
// 任务书「如 60s 滑动窗」口径）。
var wafIPWindow = 60 * time.Second

// wafIPThreshold 判定阈值：窗内不同 UID 数达到该值激活。取 2——「多号」的最小
// 定义：单号反复 403 永不触发（账号级偶发归软冷却管），两个不同号在 60s 内接连
// 被拦（同一出口 IP）已是 IP 级证据（BulidH 实测 3 号 1s 全拦，阈值 2 更早止损，
// 少放大一次轮转）。
const wafIPThreshold = 2

// wafIPGate WAF IP 级拦截状态机（Handler 内嵌，零值可用）。
type wafIPGate struct {
	mu     sync.Mutex
	scopes map[string]*wafIPScope
}

// wafIPScope 单个作用域（公共池或某个分组）的 IP 级状态。不同分组代理出口不同，
// 必须隔离，否则 A 组代理被 WAF 拦截会误伤 B 组。
type wafIPScope struct {
	hits  map[string]time.Time // uid → 最近一次 WAF 403 时刻（判定窗内，惰性剪枝）
	until time.Time            // IP 级拦截激活截止；零值 = 未激活
}

func (g *wafIPGate) scopeLocked(name string) *wafIPScope {
	if g.scopes == nil {
		g.scopes = map[string]*wafIPScope{}
	}
	s := g.scopes[name]
	if s == nil {
		s = &wafIPScope{}
		g.scopes[name] = s
	}
	return s
}

func (g *wafIPGate) noteWaf(scope, uid string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.scopeLocked(scope)
	if now.Before(s.until) {
		return true
	}
	if s.hits == nil {
		s.hits = map[string]time.Time{}
	}
	s.hits[uid] = now
	for u, t := range s.hits {
		if now.Sub(t) > wafIPWindow {
			delete(s.hits, u)
		}
	}
	if len(s.hits) >= wafIPThreshold {
		s.until = now.Add(wafIPWindow)
		log.Printf("WARN: [server] waf ip-level block scope=%q: %d accounts hit waf 403 within %s, rotate fail-fast until %s", scope, len(s.hits), wafIPWindow, s.until.Format(time.RFC3339))
		s.hits = map[string]time.Time{}
		return true
	}
	return false
}

func (g *wafIPGate) active(scope string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.scopeLocked(scope)
	return time.Now().Before(s.until)
}
