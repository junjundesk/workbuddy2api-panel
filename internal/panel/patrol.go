// patrol.go 账号巡检：批量探测号池账号，把「封号」（上游业务码 11140）与
// 「无积分」（积分剩余 0）的账号自动归入「封号池」「无积分」两个隔离分组。
//
// 分组语义（与 groups 包一致）：
//   - 分组不存在则自动创建（shared_pool=false / call_public=false），存在则直接移动；
//   - 隔离组不进入公共池、也不与公共池共用候选集，账号搬进去即停止接公共流量
//     （分组密钥仍可单独调用，运维可在分组页查看/删除）；
//   - 一个账号只能属于一个分组，移动由 groups.AddMember 完成（自动移出原分组）。
//
// 探测方式（settings.mode）：
//   - balance：只查余额（零对话消耗）。能判「无积分」，封号只能靠 billing 侧
//     返回的业务码暴露；
//   - chat：只发一句 hi（真实对话链路，消耗极少积分）。能判 11140 封号；
//   - both（缺省）：先余额再对话，一次巡检同时判定两件事。
//
// 与池状态机的边界：巡检**不**记成功/失败、不改冷却/禁用/暂停/积分快照——
// 结论只体现为「分组归属」这一件事，与「测活」同为人工/运维动作，不污染路由统计。
// 单号探测失败（网络 / 5xx / 模型目录为空）一律不移动：误把健康号扔进封号池
// 比漏检更贵（漏检下次巡检还能补，误判要人工搬回来）。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/logfmt"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

const (
	// 巡检目标分组：名称是唯一标识（面板分组按名创建，运维看到的名字必须稳定）。
	patrolGroupBannedName    = "封号池"
	patrolGroupBannedColor   = "#e5484d"
	patrolGroupNoCreditName  = "无积分"
	patrolGroupNoCreditColor = "#f5a623"

	// 巡检周期单位：面板可选「分 / 时」两档，后端统一折算成 Duration。
	patrolUnitMinute = "minute"
	patrolUnitHour   = "hour"

	patrolDefaultIntervalHour = 6 // 6h：一天 4 轮，足够发现封号/积分耗尽
	patrolMinIntervalMinutes  = 1 // 下限 1 分钟：允许密集巡检（上游压力由使用者自担）
	patrolMaxIntervalMinutes  = 7 * 24 * 60
	patrolDefaultConcurrency  = 3 // 缺省并发；0 = 不限（= 账号数，面板可填 0）
	// patrolProbeTimeout 单号探测总预算（余额 + 对话两次上游调用）。
	patrolProbeTimeout = 60 * time.Second
	// patrolKeepResults 内存里保留的最近结果条数（面板展示用）。
	patrolKeepResults = 200
	// patrolSavedResults 落盘保留条数（重启后仍能看到上一轮结论，不必全量）。
	patrolSavedResults = 100
)

// 巡检结论（对外的稳定字符串）。
const (
	patrolVerdictOK          = "ok"           // 正常
	patrolVerdictBanned      = "banned"       // 封号（11140 request illegal）
	patrolVerdictNoCredit    = "no_credit"    // 积分剩余 0
	patrolVerdictRateLimited = "rate_limited" // 11140 的模型级限流变体（非封号，不动）
	patrolVerdictFailed      = "failed"       // 探测失败（网络 / 5xx / 模型目录），不动
)

// patrolSettings 巡检设置（持久化到 data/patrol.json）。
type patrolSettings struct {
	// Enabled 自动巡检总开关：关掉后定时巡检不再触发（面板仍可手动巡检）。
	Enabled bool `json:"enabled"`
	// IntervalValue / IntervalUnit 巡检周期（数值 + 单位「分 / 时」）。存两份而不是
	// 单一分钟数：面板要按用户选择的单位回显（90 分钟显示成 90 分钟、2 小时显示成 2 小时）。
	IntervalValue int    `json:"interval_value"`
	IntervalUnit  string `json:"interval_unit"`
	// CheckBan 封号检测：主链路请求命中 11140 时实时归组 + 巡检时对话探针判 11140。
	CheckBan bool `json:"check_ban"`
	// CheckCredit 积分检测：巡检时调余额查询接口，剩余 0 判无积分。
	// 纯后台调用，不消耗对话积分。
	CheckCredit bool `json:"check_credit"`
	Concurrency int  `json:"concurrency"`
	// SkipDisabled 跳过已禁用账号（禁用号不接流量，巡检价值低）。缺省 false：
	// 封号与禁用是两件事，禁用号同样可能被封，运维想看到结论。
	SkipDisabled bool `json:"skip_disabled"`
	// DryRun 只报告不改分组（先观察一轮再打开自动归组时用）。
	DryRun bool `json:"dry_run"`
}

// patrolResult 单账号巡检结论（面板列表 / 落盘共用）。
type patrolResult struct {
	UID          string    `json:"uid"`
	Nickname     string    `json:"nickname,omitempty"`
	Realm        string    `json:"realm,omitempty"`
	Verdict      string    `json:"verdict"`
	Code         int       `json:"code,omitempty"` // 上游业务码（11140 等）
	HTTPStatus   int       `json:"http_status,omitempty"`
	Credits      *int64    `json:"credits,omitempty"`
	CreditsTotal *int64    `json:"credits_total,omitempty"`
	Model        string    `json:"model,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	FromGroup    string    `json:"from_group,omitempty"` // 巡检前分组（空 = 公共池）
	Group        string    `json:"group,omitempty"`      // 巡检后分组
	Moved        bool      `json:"moved,omitempty"`
	Error        string    `json:"error,omitempty"`
	At           time.Time `json:"at"`
}

// patrolFile patrol.json 的落盘形状。
type patrolFile struct {
	Version  int            `json:"version"`
	Settings patrolSettings `json:"settings"`
	LastRun  time.Time      `json:"last_run,omitempty"`
	Results  []patrolResult `json:"results,omitempty"`
}

// patrolRunner 巡检运行器：设置 + 最近结果 + 定时循环 + 单飞（同一时刻只跑一轮）。
type patrolRunner struct {
	p    *Panel
	path string

	mu       sync.Mutex
	settings patrolSettings
	results  []patrolResult
	lastRun  time.Time
	running  bool
	done     int
	total    int
	// skippedBanned 本轮跳过（在封号池分组里）的账号数，仅展示用。
	skippedBanned int
	// cancel 当前这一轮的取消函数（nil = 没在跑）。面板「停止」用它中断进行中的
	// 探测；上一轮的 cancel 在下轮开始时被替换，execute 结束时置 nil。
	cancel  context.CancelFunc
	rootCtx context.Context
	kick    chan struct{}
}

func newPatrolRunner(p *Panel, path string) *patrolRunner {
	r := &patrolRunner{p: p, path: strings.TrimSpace(path), rootCtx: context.Background(), kick: make(chan struct{}, 1)}
	r.settings = defaultPatrolSettings()
	r.load()
	r.settings = r.settings.normalized()
	return r
}

// defaultPatrolSettings 缺省：自动巡检开启、6 小时一轮、余额 + 对话、并发 3。
//
// 为什么默认开启：需求就是「自动巡检」。默认 both 才能同时判出 11140 封号与零积分；
// 不在意对话消耗的部署可以改成 balance（只读余额，零对话消耗）。
func defaultPatrolSettings() patrolSettings {
	return patrolSettings{
		Enabled:       true,
		IntervalValue: patrolDefaultIntervalHour,
		IntervalUnit:  patrolUnitHour,
		CheckBan:      true,
		CheckCredit:   true,
		Concurrency:   patrolDefaultConcurrency,
	}
}

func (s patrolSettings) normalized() patrolSettings {
	if strings.ToLower(strings.TrimSpace(s.IntervalUnit)) == patrolUnitHour {
		s.IntervalUnit = patrolUnitHour
	} else {
		s.IntervalUnit = patrolUnitMinute
	}
	if s.IntervalValue <= 0 {
		s.IntervalValue = patrolDefaultIntervalHour
		if s.IntervalUnit == patrolUnitMinute {
			s.IntervalValue = 60
		}
	}
	if s.IntervalUnit == patrolUnitHour {
		if s.IntervalValue > patrolMaxIntervalMinutes/60 {
			s.IntervalValue = patrolMaxIntervalMinutes / 60
		}
	} else {
		if s.IntervalValue < patrolMinIntervalMinutes {
			s.IntervalValue = patrolMinIntervalMinutes
		}
		if s.IntervalValue > patrolMaxIntervalMinutes {
			s.IntervalValue = patrolMaxIntervalMinutes
		}
	}
	// 并发：>0 用指定值（不设上限），<=0 = 不限（执行时取账号数）。负数归一成 0，
	// 避免面板误输入 -1 时被当成"1 路"。
	if s.Concurrency < 0 {
		s.Concurrency = 0
	}
	// 两项检测都关掉等于"空转巡检"：回落积分检测（纯后台查询、零对话消耗），
	// 免得运维关掉封号检测后整轮巡检什么都不做还以为在跑。
	if !s.CheckBan && !s.CheckCredit {
		s.CheckCredit = true
	}
	return s
}

// interval 巡检周期（已归一化后的值）。
func (s patrolSettings) interval() time.Duration {
	if s.IntervalUnit == patrolUnitHour {
		return time.Duration(s.IntervalValue) * time.Hour
	}
	return time.Duration(s.IntervalValue) * time.Minute
}

func (r *patrolRunner) load() {
	if r.path == "" {
		return
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("panel: 读取%s失败: %v", filepath.Base(r.path), err)
		}
		return
	}
	var f patrolFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("panel: 解析%s失败（按缺省设置继续）: %v", filepath.Base(r.path), err)
		return
	}
	r.settings = f.Settings
	r.lastRun = f.LastRun
	if len(f.Results) > 0 {
		r.results = f.Results
	}
}

// saveLocked 落盘设置与最近结果（持 r.mu 调用）。失败只记日志：巡检本身已生效，
// 落盘只是"重启后还能看到"，不该因此让面板操作失败。
func (r *patrolRunner) saveLocked() {
	if r.path == "" {
		return
	}
	results := r.results
	if len(results) > patrolSavedResults {
		results = results[len(results)-patrolSavedResults:]
	}
	f := patrolFile{Version: 1, Settings: r.settings, LastRun: r.lastRun, Results: results}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		log.Printf("panel: 巡检设置序列化失败: %v", err)
		return
	}
	if dir := filepath.Dir(r.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("panel: 巡检设置目录创建失败: %v", err)
			return
		}
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("panel: 巡检设置落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("panel: 巡检设置落盘失败: %v", err)
	}
}

func (r *patrolRunner) snapshotSettings() patrolSettings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings
}

func (r *patrolRunner) updateSettings(s patrolSettings) {
	s = s.normalized()
	r.mu.Lock()
	r.settings = s
	r.saveLocked()
	r.mu.Unlock()
	r.poke()
}

// poke 唤醒定时循环按新设置重排（非阻塞）。
func (r *patrolRunner) poke() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// StartPatrol 启动自动巡检循环（main 在信号 ctx 上调用一次）。
func (p *Panel) StartPatrol(ctx context.Context) {
	r := p.patrol
	if r == nil {
		return
	}
	if ctx != nil {
		r.mu.Lock()
		r.rootCtx = ctx
		r.mu.Unlock()
	}
	go r.loop(ctx)
}

// loop 按设置间隔自动巡检。设置变更（poke）立即按新间隔重排，关闭时挂起等待。
func (r *patrolRunner) loop(ctx context.Context) {
	if ctx == nil {
		return
	}
	for {
		s := r.snapshotSettings()
		if !s.Enabled {
			select {
			case <-ctx.Done():
				return
			case <-r.kick:
			}
			continue
		}
		timer := time.NewTimer(s.interval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.kick:
			timer.Stop()
		case <-timer.C:
			if err := r.start(nil, nil); err != nil {
				log.Printf("panel: 自动巡检跳过：%v", err)
				continue
			}
			// 等本轮跑完再排下一轮，避免慢上游把轮次叠加成并发风暴。
			r.wait()
		}
	}
}

// wait 阻塞到当前轮结束（未在跑时立即返回）。
func (r *patrolRunner) wait() {
	for {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

var errPatrolRunning = errors.New("巡检正在执行中")

// start 异步启动一轮巡检。uids 为空 = 全池；检测项取自设置（封号 / 积分两个开关）；
// dryRun 非 nil 时覆盖设置里的试运行开关（面板手动巡检按弹窗勾选，nil = 用设置值）。
func (r *patrolRunner) start(uids []string, dryRun *bool) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errPatrolRunning
	}
	s := r.settings
	s = s.normalized()
	if dryRun != nil {
		s.DryRun = *dryRun
	}
	r.running = true
	r.done = 0
	r.total = 0
	r.skippedBanned = 0
	ctx, cancel := context.WithCancel(r.rootCtx)
	r.cancel = cancel
	r.mu.Unlock()

	go r.execute(ctx, uids, s)
	return nil
}

// stop 停止巡检：取消进行中的一轮（如有），并关闭自动巡检开关（disableAuto）。
// 返回是否取消到了正在跑的轮次、是否真的关掉了自动巡检。
//
// 停止是"两件事"：定时循环由 settings.Enabled 控制（关掉并落盘 + poke 重排），
// 进行中的轮次由 ctx 取消控制（余额查询没有 ctx 入口，靠 patrolBalanceProbeCtx
// 放弃等待；对话请求由 ChatStreamContext 的 ctx 直接中断）。
func (r *patrolRunner) stop(cancelRun, disableAuto bool) (cancelled, autoDisabled bool) {
	r.mu.Lock()
	if cancelRun && r.cancel != nil {
		r.cancel()
		cancelled = true
	}
	if disableAuto && r.settings.Enabled {
		r.settings.Enabled = false
		r.saveLocked()
		autoDisabled = true
	}
	r.mu.Unlock()
	if autoDisabled {
		r.poke()
	}
	return cancelled, autoDisabled
}

// candidates 选出本轮要巡检的账号（按 uid 稳定排序，便于日志与结果比对）。
// candidates 选出本轮要巡检的账号（按 uid 稳定排序，便于日志与结果比对），并返回
// 被跳过的封号池账号数。
//
// 跳过两类：
//   - skipDisabled 打开时的禁用账号（配置项）；
//   - **已经在「封号池」里的账号**——封号是终态，再探一次既改不了结论，又会白烧一次
//     对话（还可能在上游再记一笔风控）。账号解封后人工移回公共池，下一次巡检自然
//     恢复覆盖它；实时封号归组（NotifyAccountBanned）不受此跳过影响。
func (r *patrolRunner) candidates(uids []string, skipDisabled bool) (accounts []*auth.Auth, skippedBanned int) {
	want := map[string]bool{}
	for _, uid := range uids {
		if uid = strings.TrimSpace(uid); uid != "" {
			want[uid] = true
		}
	}
	bannedID := r.p.bannedGroupID()
	out := make([]*auth.Auth, 0, len(uids))
	for _, st := range r.p.cfg.Pool.List() {
		if skipDisabled && st.Disabled {
			continue
		}
		if bannedID != "" && st.Group == bannedID {
			skippedBanned++
			continue
		}
		if len(want) > 0 && !want[st.UID] {
			continue
		}
		if a := r.p.cfg.Pool.AuthByUID(st.UID); a != nil {
			out = append(out, a)
		}
	}
	return out, skippedBanned
}

// bannedGroupID 返回「封号池」分组 id（不存在返回空串）。只读，不创建分组。
func (p *Panel) bannedGroupID() string {
	if p.cfg.Groups == nil {
		return ""
	}
	for _, g := range p.cfg.Groups.Groups() {
		if g.Name == patrolGroupBannedName {
			return g.ID
		}
	}
	return ""
}

// execute 并发探测 + 按结论归组。同一时刻只有一个 execute（由 start 的单飞保证）。
func (r *patrolRunner) execute(ctx context.Context, uids []string, s patrolSettings) {
	started := time.Now()
	accounts, skippedBanned := r.candidates(uids, s.SkipDisabled)
	results := make([]patrolResult, len(accounts))

	r.mu.Lock()
	r.total = len(accounts)
	r.done = 0
	r.skippedBanned = skippedBanned
	r.mu.Unlock()

	dry := ""
	if s.DryRun {
		dry = "，试运行不改分组"
	}
	skipText := ""
	if skippedBanned > 0 {
		skipText = fmt.Sprintf("，跳过封号池 %d 个", skippedBanned)
	}
	log.Printf("panel: 巡检开始（账号 %d 个%s，检测 %s，并发 %d%s）", len(accounts), skipText, patrolChecksText(s), s.Concurrency, dry)

	conc := s.Concurrency
	if conc <= 0 {
		conc = len(accounts) // 0 = 不限：全池并发
	}
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 已被「停止」取消：不再发新请求，直接标记本轮该号未探测。
			if ctx.Err() != nil {
				results[i] = patrolResult{
					UID: a.UID, Nickname: a.Nickname, Realm: a.Realm(), At: time.Now(),
					Verdict: patrolVerdictFailed, Detail: "已取消（巡检被手动停止）",
				}
				r.mu.Lock()
				r.done++
				r.mu.Unlock()
				return
			}
			res := r.p.patrolProbe(ctx, a, s.CheckBan, s.CheckCredit)
			res.FromGroup = a.Group()
			res.Group = res.FromGroup
			if !s.DryRun {
				if gid, target := r.p.patrolTargetGroup(res.Verdict); gid != "" {
					if cur := r.p.cfg.Groups.GroupOf(a.UID); cur != gid {
						if err := r.p.patrolMove(a.UID, gid); err != nil {
							res.Error = "移动分组失败：" + err.Error()
							log.Printf("panel: 巡检 %s 归组失败: %v", logfmt.Label(a.UID, a.Nickname), err)
						} else {
							res.Group = gid
							res.Moved = true
							log.Printf("panel: 巡检 %s 判定%s → 已移入「%s」",
								logfmt.Label(a.UID, a.Nickname), patrolVerdictText(res.Verdict), target)
						}
					} else {
						res.Group = gid
					}
				}
			}
			results[i] = res

			r.mu.Lock()
			r.done++
			r.mu.Unlock()
		}(i, a)
	}
	wg.Wait()

	// 汇总：只记一行，明细在面板巡检弹窗里。
	var banned, noCredit, ok, failed, moved int
	for _, res := range results {
		switch res.Verdict {
		case patrolVerdictBanned:
			banned++
		case patrolVerdictNoCredit:
			noCredit++
		case patrolVerdictOK:
			ok++
		default:
			failed++
		}
		if res.Moved {
			moved++
		}
	}

	r.mu.Lock()
	r.running = false
	r.cancel = nil
	r.lastRun = time.Now()
	r.results = append(r.results, results...)
	if len(r.results) > patrolKeepResults {
		r.results = r.results[len(r.results)-patrolKeepResults:]
	}
	r.saveLocked()
	r.mu.Unlock()

	log.Printf("panel: 巡检完成（%s）：封号 %d / 无积分 %d / 正常 %d / 探测失败 %d，移动 %d 个账号",
		time.Since(started).Round(time.Second), banned, noCredit, ok, failed, moved)
}

// patrolChecksText 日志/面板用的检测项描述。
func patrolChecksText(s patrolSettings) string {
	switch {
	case s.CheckBan && s.CheckCredit:
		return "封号+积分"
	case s.CheckBan:
		return "仅封号"
	case s.CheckCredit:
		return "仅积分"
	}
	return "无"
}

func patrolVerdictText(v string) string {
	switch v {
	case patrolVerdictBanned:
		return "封号"
	case patrolVerdictNoCredit:
		return "无积分"
	case patrolVerdictOK:
		return "正常"
	case patrolVerdictRateLimited:
		return "模型级限流"
	}
	return "探测失败"
}

// patrolTargetGroup 结论 → 目标分组。未命中的结论不移动，返回空。
// 分组不存在时创建（隔离策略：不进公共池、不共用公共池），已存在直接复用。
func (p *Panel) patrolTargetGroup(verdict string) (id, name string) {
	switch verdict {
	case patrolVerdictBanned:
		return p.ensurePatrolGroup(patrolGroupBannedName, patrolGroupBannedColor), patrolGroupBannedName
	case patrolVerdictNoCredit:
		return p.ensurePatrolGroup(patrolGroupNoCreditName, patrolGroupNoCreditColor), patrolGroupNoCreditName
	}
	return "", ""
}

// patrolGroupMu 串行化「按名找/建分组」：并发 worker 同时命中同一结论时不会建出两个同名组。
var patrolGroupMu sync.Mutex

// ensurePatrolGroup 按名称取分组 id，不存在则创建；创建失败返回空串（调用方按未移动处理）。
func (p *Panel) ensurePatrolGroup(name, color string) string {
	if p.cfg.Groups == nil {
		return ""
	}
	patrolGroupMu.Lock()
	defer patrolGroupMu.Unlock()
	for _, g := range p.cfg.Groups.Groups() {
		if g.Name == name {
			return g.ID
		}
	}
	g, _, err := p.cfg.Groups.Create(name, color, false, false)
	if err != nil {
		log.Printf("panel: 巡检创建分组「%s」失败: %v", name, err)
		return ""
	}
	p.applyGroupMeta()
	log.Printf("panel: 巡检自动创建分组「%s」（隔离：不进公共池）", name)
	return g.ID
}

// patrolMove 把账号搬到目标分组（AddMember 会自动移出原分组）。
func (p *Panel) patrolMove(uid, gid string) error {
	if p.cfg.Groups == nil {
		return errors.New("groups not available")
	}
	if err := p.cfg.Groups.AddMember(gid, uid); err != nil {
		return err
	}
	p.cfg.Pool.SetGroup(uid, gid)
	return nil
}

// markBannedAccounts 给账号列表打「封号」标记（11140），供面板把封号号的积分
// 排除在池统计之外。判据两条，任一命中即算：
//   - 账号在「封号池」分组里（巡检 / 实时归组的结果，也是运维可人工调整的权威位置）；
//   - 禁用原因含 11140（实时封号时 server 写入的文案，见 applyErrorPolicy）。
//
// 只在响应副本上打标，不改 pool 内部状态：把账号从封号池移回公共池后标记自然消失，
// 不需要额外维护一套"封号状态机"。
func (p *Panel) markBannedAccounts(list []pool.Status) {
	bannedID := p.bannedGroupID()
	for i := range list {
		if bannedID != "" && list[i].Group == bannedID {
			list[i].Banned = true
			continue
		}
		if strings.Contains(list[i].DisabledReason, "11140") {
			list[i].Banned = true
		}
	}
}

// countBanned 统计封号账号数（overview 的 banned_total，前端据此提示"已排除 N 个"）。
func countBanned(list []pool.Status) int {
	n := 0
	for i := range list {
		if list[i].Banned {
			n++
		}
	}
	return n
}

// NotifyAccountBanned 主链路实时封号归组（server 在 11140 request illegal 时回调）。
//
// 封号检测有两个来源：用户被动请求命中 11140（本函数，实时），或巡检时账号状态
// 探测命中 11140（patrolProbe，周期）。任一命中都自动移入「封号池」。
//
// 异步执行：归组要写 groups.json，不能给正在返回 403 的聊天请求叠文件 IO 延迟。
// 封号检测开关关闭时只保留 server 侧的禁用（不搬分组）——开关要真的说了算。
func (p *Panel) NotifyAccountBanned(uid string) {
	if p == nil || p.cfg.Groups == nil || p.cfg.Pool == nil || uid == "" {
		return
	}
	if p.patrol != nil && !p.patrol.snapshotSettings().CheckBan {
		return
	}
	nickname := ""
	if a := p.cfg.Pool.AuthByUID(uid); a != nil {
		nickname = a.Nickname
	}
	go func() {
		gid := p.ensurePatrolGroup(patrolGroupBannedName, patrolGroupBannedColor)
		if gid == "" {
			return
		}
		if p.cfg.Groups.GroupOf(uid) == gid {
			return
		}
		if err := p.patrolMove(uid, gid); err != nil {
			log.Printf("panel: 实时封号归组失败 %s: %v", logfmt.Label(uid, nickname), err)
			return
		}
		log.Printf("panel: 账号 %s 被上游判封（11140），已实时移入「封号池」", logfmt.Label(uid, nickname))
	}()
}

// patrolProbe 单账号探测：积分检测跑余额查询、封号检测跑对话探针，合并成一条结论。
// 两项都开时先余额后对话——余额更便宜，且已判封号时省掉一次对话请求。
func (p *Panel) patrolProbe(ctx context.Context, a *auth.Auth, checkBan, checkCredit bool) patrolResult {
	res := patrolResult{
		UID:      a.UID,
		Nickname: a.Nickname,
		Realm:    a.Realm(),
		Verdict:  patrolVerdictOK,
		At:       time.Now(),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, patrolProbeTimeout)
	defer cancel()

	var balance, chat *patrolResult
	if checkCredit || checkBan {
		b := p.patrolBalanceProbeCtx(ctx, a)
		balance = &b
	}
	// 已判封号就没必要再打一次对话（省一次上游调用与积分）。
	if checkBan && (balance == nil || balance.Verdict != patrolVerdictBanned) {
		c := p.patrolChatProbe(ctx, a)
		chat = &c
	}
	out := combinePatrol(res, balance, chat)
	// 封号检测关闭时，余额/对话路径顺带暴露的 11140 也不搬号——开关要真的说了算。
	if !checkBan && out.Verdict == patrolVerdictBanned {
		out.Verdict = patrolVerdictFailed
		out.Detail = "疑似封号（11140），封号检测已在设置里关闭，未移动"
	}
	return out
}

// patrolBalanceProbeCtx 余额探测的取消包装：UserResourceDetailedWithExpiry 没有
// ctx 入口（内部固定重试 + 120s HTTP 超时），面板「停止」不能干等它。取消时直接
// 放弃等待并标记未探测——goroutine 会在 HTTP 超时后自行结束，结果丢弃（不写池、
// 不影响本轮结论），代价是一次被放弃的在途查询。
func (p *Panel) patrolBalanceProbeCtx(ctx context.Context, a *auth.Auth) patrolResult {
	done := make(chan patrolResult, 1)
	go func() { done <- p.patrolBalanceProbe(a) }()
	select {
	case res := <-done:
		return res
	case <-ctx.Done():
		return patrolResult{
			UID: a.UID, Nickname: a.Nickname, Realm: a.Realm(), At: time.Now(),
			Verdict: patrolVerdictFailed, Detail: "已取消（巡检被手动停止）", Error: ctx.Err().Error(),
		}
	}
}

// patrolBalanceProbe 余额探测：拿到剩余积分即可判「无积分」；错误信封里的业务码
// 用于识别 11140（封号）。
func (p *Panel) patrolBalanceProbe(a *auth.Auth) patrolResult {
	res := patrolResult{UID: a.UID, Nickname: a.Nickname, Realm: a.Realm(), Verdict: patrolVerdictOK, At: time.Now()}
	remain, total, _, _, _, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		return patrolApplyError(res, err, "余额查询")
	}
	res.Credits = &remain
	res.CreditsTotal = &total
	// 企业版不限量以 total=-1 表达（见 upstream.enterpriseUnlimitedTotal）：
	// 剩余量是哨兵大数，不能按"余额"解读，更不该判成无积分。
	if total != -1 && remain <= 0 {
		res.Verdict = patrolVerdictNoCredit
		res.Detail = fmt.Sprintf("积分剩余 %d（总额 %d）", remain, total)
	}
	return res
}

// patrolChatProbe 对话探测：发一句 hi，走真实 chat 链路。11140 request illegal
// 在这里以 401/403 业务信封暴露（与主链路同一分类链）。
func (p *Panel) patrolChatProbe(ctx context.Context, a *auth.Auth) patrolResult {
	res := patrolResult{UID: a.UID, Nickname: a.Nickname, Realm: a.Realm(), Verdict: patrolVerdictOK, At: time.Now()}
	_, infos, err := p.accountModelCatalog(a)
	if err != nil {
		res.Verdict = patrolVerdictFailed
		res.Detail = "模型目录：" + err.Error()
		res.Error = err.Error()
		return res
	}
	model := aliveProbeModel(infos)
	if model == "" {
		res.Verdict = patrolVerdictFailed
		res.Detail = "模型目录为空"
		res.Error = "模型目录为空"
		return res
	}
	res.Model = model
	body, merr := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": alivePrompt},
		},
		"stream": true,
	})
	if merr != nil {
		res.Verdict = patrolVerdictFailed
		res.Detail = "构造请求失败：" + merr.Error()
		res.Error = merr.Error()
		return res
	}
	rc, status, rawBody, chatErr := p.cfg.Upstream.ChatStreamContext(ctx, a, body, "", upstream.ChatMeta{})
	if rc != nil {
		defer rc.Close()
	}
	if chatErr == nil && status >= 400 {
		chatErr = fmt.Errorf("上游返回 HTTP %d：%s", status, truncateRunes(string(rawBody), 200))
	}
	if chatErr != nil {
		return patrolApplyError(res, chatErr, "对话探测")
	}
	if rc == nil {
		res.Verdict = patrolVerdictFailed
		res.Detail = "上游未返回响应体"
		res.Error = res.Detail
		return res
	}
	if _, aggErr := upstream.Aggregate(io.LimitReader(rc, aliveStreamLimit)); aggErr != nil {
		return patrolApplyError(res, aggErr, "对话探测")
	}
	return res
}

// patrolApplyError 把上游错误翻成巡检结论。
//
// 11140 的两种语义必须分开：request illegal（Kind=ErrAccountFault）= 封号；
// The model provider is rate-limiting（Kind=ErrSoftRate）= 模型级限流，账号本身
// 没问题，绝不能移进封号池。Code 与 Kind 一起看才能断言，不靠文案猜测。
func patrolApplyError(res patrolResult, err error, stage string) patrolResult {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		res.HTTPStatus = ue.Status
		res.Code = ue.Code
		switch {
		case ue.Code == 11140 && ue.Kind == upstream.ErrAccountFault:
			res.Verdict = patrolVerdictBanned
			res.Detail = fmt.Sprintf("%s：上游 code 11140（request illegal，账号封禁）", stage)
			return res
		case ue.Code == 11140 && ue.Kind == upstream.ErrSoftRate:
			res.Verdict = patrolVerdictRateLimited
			res.Detail = fmt.Sprintf("%s：上游 code 11140（模型级限流，非封号）", stage)
			res.Error = ue.Error()
			return res
		}
		res.Verdict = patrolVerdictFailed
		res.Detail = fmt.Sprintf("%s：%s（http %d code=%d）", stage, ue.Kind.String(), ue.Status, ue.Code)
		res.Error = ue.Error()
		return res
	}
	res.Verdict = patrolVerdictFailed
	res.Detail = stage + "：" + err.Error()
	res.Error = err.Error()
	return res
}

// combinePatrol 合并余额与对话两路结论。
//
// 优先级：封号 > 无积分 > 对话失败 > 限流 > 正常。
//   - 对话判出封号即封号（比 billing 侧更接近真实链路）；
//   - 余额已确定 0 就维持无积分（对话可能走免费模型成功，不代表有积分）；
//   - 余额查询失败但对话正常 → 视为正常（billing 抖动不该把健康号搬走）；
//   - 对话失败而余额正常 → 记录失败原因（号当下不可用），但不移动分组。
func combinePatrol(base patrolResult, balance, chat *patrolResult) patrolResult {
	out := base
	if balance != nil {
		out.Credits = balance.Credits
		out.CreditsTotal = balance.CreditsTotal
	}
	if chat != nil && chat.Model != "" {
		out.Model = chat.Model
	}
	switch {
	case chat != nil && chat.Verdict == patrolVerdictBanned:
		out.Verdict = patrolVerdictBanned
		out.Detail = chat.Detail
		out.Code = chat.Code
		out.HTTPStatus = chat.HTTPStatus
		return out
	case balance != nil && balance.Verdict == patrolVerdictBanned:
		out.Verdict = patrolVerdictBanned
		out.Detail = balance.Detail
		out.Code = balance.Code
		out.HTTPStatus = balance.HTTPStatus
		return out
	case balance != nil && balance.Verdict == patrolVerdictNoCredit:
		out.Verdict = patrolVerdictNoCredit
		out.Detail = balance.Detail
		return out
	case balance != nil && balance.Verdict == patrolVerdictFailed &&
		chat != nil && chat.Verdict == patrolVerdictOK:
		// billing 侧抖动但对话正常：号是能用的，不该因为一次余额查询失败被判故障。
		out.Verdict = patrolVerdictOK
		out.Detail = ""
		out.Error = ""
		return out
	}
	switch {
	case chat != nil && chat.Verdict == patrolVerdictFailed:
		out.Verdict = patrolVerdictFailed
		out.Detail = chat.Detail
		out.Code = chat.Code
		out.HTTPStatus = chat.HTTPStatus
		out.Error = chat.Error
	case balance != nil && balance.Verdict == patrolVerdictFailed:
		out.Verdict = patrolVerdictFailed
		out.Detail = balance.Detail
		out.Code = balance.Code
		out.HTTPStatus = balance.HTTPStatus
		out.Error = balance.Error
	case chat != nil && chat.Verdict == patrolVerdictRateLimited:
		out.Verdict = patrolVerdictRateLimited
		out.Detail = chat.Detail
		out.Code = chat.Code
		out.HTTPStatus = chat.HTTPStatus
	case balance != nil && balance.Verdict == patrolVerdictRateLimited:
		out.Verdict = patrolVerdictRateLimited
		out.Detail = balance.Detail
		out.Code = balance.Code
		out.HTTPStatus = balance.HTTPStatus
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

// patrolView 面板 /panel/api/patrol 响应：设置 + 运行态 + 最近结果。
type patrolView struct {
	Settings patrolSettings `json:"settings"`
	Running  bool           `json:"running"`
	Done     int            `json:"done"`
	Total    int            `json:"total"`
	// SkippedBanned 本轮因"在封号池分组"被跳过的账号数（封号号不再重复探测）。
	SkippedBanned int `json:"skipped_banned"`
	// LastRun 指针：time.Time 零值会被 omitempty 忽略不了，面板会以为"巡检过"。
	LastRun   *time.Time        `json:"last_run,omitempty"`
	Results   []patrolResult    `json:"results"`
	Groups    map[string]string `json:"groups"`
	Available bool              `json:"available"`
}

// patrolState GET /panel/api/patrol。
func (p *Panel) patrolState(w http.ResponseWriter, r *http.Request) {
	if err := p.patrolReady(); err != nil {
		writeErr(w, http.StatusNotImplemented, err.Error())
		return
	}
	rp := p.patrol
	rp.mu.Lock()
	view := patrolView{
		Settings:      rp.settings,
		Running:       rp.running,
		Done:          rp.done,
		Total:         rp.total,
		SkippedBanned: rp.skippedBanned,
		Available:     true,
	}
	if !rp.lastRun.IsZero() {
		lastRun := rp.lastRun
		view.LastRun = &lastRun
	}
	results := make([]patrolResult, len(rp.results))
	copy(results, rp.results)
	rp.mu.Unlock()
	// 最近一轮排前面：面板默认按时间倒序看结论。
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	view.Results = results
	view.Groups = map[string]string{}
	if p.cfg.Groups != nil {
		for _, g := range p.cfg.Groups.Groups() {
			switch g.Name {
			case patrolGroupBannedName:
				view.Groups["banned"] = g.ID
			case patrolGroupNoCreditName:
				view.Groups["no_credit"] = g.ID
			}
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// patrolConfigure POST /panel/api/patrol：保存设置（在线生效，立即重排定时循环）。
func (p *Panel) patrolConfigure(w http.ResponseWriter, r *http.Request) {
	if err := p.patrolReady(); err != nil {
		writeErr(w, http.StatusNotImplemented, err.Error())
		return
	}
	var req struct {
		Enabled       *bool   `json:"enabled"`
		IntervalValue *int    `json:"interval_value"`
		IntervalUnit  *string `json:"interval_unit"`
		CheckBan      *bool   `json:"check_ban"`
		CheckCredit   *bool   `json:"check_credit"`
		Concurrency   *int    `json:"concurrency"`
		SkipDisabled  *bool   `json:"skip_disabled"`
		DryRun        *bool   `json:"dry_run"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s := p.patrol.snapshotSettings()
	if req.Enabled != nil {
		s.Enabled = *req.Enabled
	}
	if req.IntervalValue != nil {
		s.IntervalValue = *req.IntervalValue
	}
	if req.IntervalUnit != nil {
		s.IntervalUnit = *req.IntervalUnit
	}
	if req.CheckBan != nil {
		s.CheckBan = *req.CheckBan
	}
	if req.CheckCredit != nil {
		s.CheckCredit = *req.CheckCredit
	}
	if req.Concurrency != nil {
		s.Concurrency = *req.Concurrency
	}
	if req.SkipDisabled != nil {
		s.SkipDisabled = *req.SkipDisabled
	}
	if req.DryRun != nil {
		s.DryRun = *req.DryRun
	}
	p.patrol.updateSettings(s)
	log.Printf("panel: 巡检设置已更新（自动 %v，周期 %s，检测 %s，并发 %d，试运行 %v）",
		s.Enabled, s.interval(), patrolChecksText(s), s.Concurrency, s.DryRun)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": p.patrol.snapshotSettings()})
}

// patrolStart POST /panel/api/patrol/run：立即巡检（异步）。body 可选
// {uids:[...], mode:"both", dry_run:true}。
func (p *Panel) patrolStart(w http.ResponseWriter, r *http.Request) {
	if err := p.patrolReady(); err != nil {
		writeErr(w, http.StatusNotImplemented, err.Error())
		return
	}
	var req struct {
		UIDs   []string `json:"uids"`
		DryRun *bool    `json:"dry_run"`
	}
	if r.ContentLength > 0 {
		if err := decodeJSONBody(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := p.patrol.start(req.UIDs, req.DryRun); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// patrolStop POST /panel/api/patrol/stop：停止巡检。
// body 可选 {cancel_run:true, disable_auto:true}（缺省都停）：
//   - cancel_run 取消进行中的这一轮（探测立即让位，不再发新请求）；
//   - disable_auto 关闭自动巡检开关（定时循环挂起，设置落盘）。
func (p *Panel) patrolStop(w http.ResponseWriter, r *http.Request) {
	if err := p.patrolReady(); err != nil {
		writeErr(w, http.StatusNotImplemented, err.Error())
		return
	}
	var req struct {
		CancelRun   *bool `json:"cancel_run"`
		DisableAuto *bool `json:"disable_auto"`
	}
	if r.ContentLength > 0 {
		if err := decodeJSONBody(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	cancelRun, disableAuto := true, true
	if req.CancelRun != nil {
		cancelRun = *req.CancelRun
	}
	if req.DisableAuto != nil {
		disableAuto = *req.DisableAuto
	}
	cancelled, autoDisabled := p.patrol.stop(cancelRun, disableAuto)
	log.Printf("panel: 巡检已停止（取消当前轮=%v，关闭自动巡检=%v）", cancelled, autoDisabled)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"cancelled":     cancelled,
		"auto_disabled": autoDisabled,
		"settings":      p.patrol.snapshotSettings(),
	})
}

// patrolReady 接口可用性前置检查：面板未装配池 / 上游 / 分组时返回 501。
func (p *Panel) patrolReady() error {
	if p.patrol == nil {
		return errors.New("patrol not available")
	}
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return errors.New("account pool or upstream not available")
	}
	if p.cfg.Groups == nil {
		return errors.New("groups not available")
	}
	return nil
}
