// Package store — 订单与审计留痕。
//
// M3 v1 采用 JSONL 文件（每行一条 JSON 记录，追加写、原子 flush、
// 并发安全），不引入 CGO/第三方驱动；好处是可直接 `cat`/`jq` 对账。
// 后续如需 SQL 查询可平移为 SQLite（表 = 文件，行 = 记录）。
package store

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tp/coinoption/internal/strategy"
)

// SimOrder 一笔纸面订单（DCY 申购 / 期权卖单 的本地模拟）。
type SimOrder struct {
	ID        string    `json:"id"`                // sim-...
	Kind      string    `json:"kind"`              // "dcd" | "option"
	Mode      string    `json:"mode"`              // 产生时的运行模式（sim-okx / sim-local）
	ProductID string    `json:"productId"`         // DCY 产品 ID 或 期权 instId
	OptType   rune      `json:"optType"`           // C | P
	Strike    float64   `json:"strike"`            // 行权价
	AbsYield  float64   `json:"absYield"`          // 绝对收益率（DCY）或期权 premium/本金（option）
	Amt       float64   `json:"amt"`               // 本金规模（投资币种）
	InvestCcy string    `json:"investCcy"`         // BTC | USDT
	AmtUSDT   float64   `json:"amtUsdt"`           // 下单时折 USDT（风控口径）
	Premium   float64   `json:"premium,omitempty"` // 期权：成交权利金（investCcy 口径）
	CtVal     float64   `json:"ctVal,omitempty"`   // 期权：每张名义（ctVal）
	PremCcy   string    `json:"premCcy,omitempty"` // 期权：计价/结算币种（instrument.settleCcy：BTC/USD/USDT）
	ExpTime   time.Time `json:"expTime"`
	CreatedAt time.Time `json:"createdAt"`
	State     string    `json:"state"` // open | settled | expired
	// 结算结果（State=settled 时填充）
	SettledAt time.Time `json:"settledAt,omitempty"`
	SettlePx  float64   `json:"settlePx,omitempty"`
	SettleSz  float64   `json:"settleSz,omitempty"`  // 本金/损失交付量
	SettleCcy string    `json:"settleCcy,omitempty"` // 交付币种（与交付量成对）
	YieldSz   float64   `json:"yieldSz,omitempty"`   // 收益量（期权为「权利金 − 行权损耗」净额）
	YieldCcy  string    `json:"yieldCcy,omitempty"`
	// PnlUSDT 已实现盈亏（统一 USDT 口径，含本金汇兑损益）。
	// 旧字段 SettleSz/YieldSz 只描述交付量，不能直接当 P&L（被行权时本金
	// 从 USDT 变成 BTC 的浮亏不在其中）。
	PnlUSDT  float64 `json:"pnlUsdt,omitempty"`
	SimOrdID string  `json:"simOrdId,omitempty"` // OKX Demo 回单 ordId（sim-okx 期权）
	Note     string  `json:"note,omitempty"`
}

// Order 真实订单回执（live / sim-okx Demo）。
type Order struct {
	ID        string    `json:"id"`   // ordId / sordId
	Kind      string    `json:"kind"` // "dcd" | "option"
	Mode      string    `json:"mode"`
	Ref       string    `json:"ref"` // productId / instId
	Amt       float64   `json:"amt"`
	AmtUSDT   float64   `json:"amtUsdt"`
	QuoteID   string    `json:"quoteId,omitempty"` // DCY 两步流的报价单
	CreatedAt time.Time `json:"createdAt"`
	Raw       string    `json:"raw,omitempty"` // API 原始回执片段（审计用）
}

// AuditEvent 决策/提交/拒绝审计（plan：下拒绝必须留痕）。
type AuditEvent struct {
	Ts      time.Time `json:"ts"`
	Event   string    `json:"event"`   // "reject" | "submit" | "error" | "settle" | "release"
	Subject string    `json:"subject"` // product/instId
	Reason  string    `json:"reason,omitempty"`
	AmtUSDT float64   `json:"amtUsdt,omitempty"`
	PnlUSDT float64   `json:"pnlUsdt,omitempty"` // 已实现盈亏（结算事件用）
	Mode    string    `json:"mode"`
	Detail  string    `json:"detail,omitempty"`
}

// Store JSONL 追加文件仓。
type Store struct {
	dir string
	mu  sync.Mutex
}

// New 创建（必要时 mkdir -p）。
// dir 为空 → $HOME/.coinoption。
func New(dir string) (*Store, error) {
	resolved, err := ResolveDir(dir)
	if err != nil {
		return nil, err
	}
	dir = resolved
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir 数据目录（CLI 展示）。
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// append 追加一行 JSON。
func (s *Store) append(name string, v any) error {
	bb, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal %T: %w", v, err)
	}
	f, err := os.OpenFile(s.path(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	if _, err := w.Write(append(bb, '\n')); err != nil {
		return err
	}
	return w.Flush()
}

// readAll 读取全部记录。
func readAll[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return out, fmt.Errorf("parse %s: %w (line: %.80s)", path, err, line)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// SaveSim / LoadSim ── 纸面订单表（sim_orders）。
func (s *Store) SaveSim(o SimOrder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append("sim_orders.jsonl", o)
}

func (s *Store) LoadSim() ([]SimOrder, error) { return readAll[SimOrder](s.path("sim_orders.jsonl")) }

// ClearOpenSim 删除所有 open 状态的纸面单（保留已结算历史），返回删除数。
// 用于 sim 测试重置 / 切换 live 前清理历史纸面仓位；不触碰真实平台仓位。
func (s *Store) ClearOpenSim() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := readAll[SimOrder](s.path("sim_orders.jsonl"))
	if err != nil {
		return 0, err
	}
	kept := make([]SimOrder, 0, len(rows))
	removed := 0
	for _, o := range rows {
		if o.State == "open" {
			removed++
			continue
		}
		kept = append(kept, o)
	}
	if removed == 0 {
		return 0, nil
	}
	if err := rewriteAll(s.path("sim_orders.jsonl"), kept, SimOrder{}); err != nil {
		return 0, err
	}
	return removed, nil
}

// SaveOrder / LoadOrders ── 真实订单回执（orders）。
func (s *Store) SaveOrder(o Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append("orders.jsonl", o)
}

func (s *Store) LoadOrders() ([]Order, error) { return readAll[Order](s.path("orders.jsonl")) }

// Audit ── 审计日志（audit_log）。
func (s *Store) Audit(e AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Ts.IsZero() {
		e.Ts = time.Now().UTC()
	}
	return s.append("audit_log.jsonl", e)
}

func (s *Store) LoadAudit() ([]AuditEvent, error) {
	return readAll[AuditEvent](s.path("audit_log.jsonl"))
}

// AutoFailure 一条「自动执行失败」记录。
//
// 自动策略失败后没有回执可查，只能靠这组记录回答「刚才为什么没自动下单」。
// 环形保留最近 AutoFailureLimit 条（旧 → 新），落盘 auto_failures.jsonl，
// 重启后仍可查看；Web / TUI 共用同一份文件。
type AutoFailure struct {
	Ts     time.Time `json:"ts"`
	Kind   string    `json:"kind"`             // "option" | "dcd" | "market"
	Ref    string    `json:"ref,omitempty"`    // instId / productId / 标的 uly
	Reason string    `json:"reason"`           // 失败原因（人读）
	Detail string    `json:"detail,omitempty"` // 当时准备提交的候选摘要
	Count  int       `json:"count"`            // 连续相同失败被合并的次数（≥1）
}

// AutoFailureLimit 失败记录保留条数（用户要求的「最近 10 条」）。
const AutoFailureLimit = 10

// autoFailureMergeWindow 连续相同失败在该窗口内合并为一条并累加 Count，
// 避免同一原因反复重试把 10 条额度刷满、丢掉更早的不同失败。
const autoFailureMergeWindow = 10 * time.Minute

// PushAutoFailure 把一条失败记录并入按时间升序排列的表，返回新表：
//   - 与最后一条 kind/ref/reason 完全相同且间隔 < 10min → 合并（更新 ts、Count++）；
//   - 否则追加，并只保留最后 AutoFailureLimit 条（最旧的被淘汰）。
func PushAutoFailure(rows []AutoFailure, f AutoFailure, now time.Time) []AutoFailure {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if f.Ts.IsZero() {
		f.Ts = now
	}
	if f.Count <= 0 {
		f.Count = 1
	}
	if n := len(rows); n > 0 {
		last := &rows[n-1]
		if last.Kind == f.Kind && last.Ref == f.Ref && last.Reason == f.Reason &&
			f.Ts.Sub(last.Ts) < autoFailureMergeWindow {
			last.Ts = f.Ts
			if f.Detail != "" {
				last.Detail = f.Detail
			}
			last.Count += f.Count
			return rows
		}
	}
	rows = append(rows, f)
	if extra := len(rows) - AutoFailureLimit; extra > 0 {
		rows = append([]AutoFailure(nil), rows[extra:]...)
	}
	return rows
}

// RecordAutoFailure 追加一条自动失败记录（合并 / 截断后整表重写）。
func (s *Store) RecordAutoFailure(f AutoFailure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path("auto_failures.jsonl")
	// 文件损坏时按空表重建：失败记录是诊断辅助，不能反过来拖垮自动执行。
	rows, _ := readAll[AutoFailure](path)
	rows = PushAutoFailure(rows, f, time.Now().UTC())
	return rewriteAll(path, rows, AutoFailure{})
}

// SaveAutoFailures 覆盖写整张失败表（Web 端在内存里维护环形表，再整体落盘）。
func (s *Store) SaveAutoFailures(rows []AutoFailure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(rows) > AutoFailureLimit {
		rows = rows[len(rows)-AutoFailureLimit:]
	}
	return rewriteAll(s.path("auto_failures.jsonl"), rows, AutoFailure{})
}

// LoadAutoFailures 读取失败表（旧 → 新，最多 AutoFailureLimit 条）。
func (s *Store) LoadAutoFailures() ([]AutoFailure, error) {
	rows, err := readAll[AutoFailure](s.path("auto_failures.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(rows) > AutoFailureLimit {
		rows = rows[len(rows)-AutoFailureLimit:]
	}
	return rows, nil
}

// UpdateSim 用 ID 原地更新一笔纸面订单（读出改写全文件——记录量小足够）。
func (s *Store) UpdateSim(id string, upd func(o *SimOrder)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := readAll[SimOrder](s.path("sim_orders.jsonl"))
	if err != nil {
		return err
	}
	var (
		hit     SimOrder
		found   bool
		rewrote = make([]SimOrder, 0, len(rows))
	)
	for _, r := range rows {
		if r.ID == id {
			upd(&r)
			r.State = normalizeState(r.State)
			hit, found = r, true
		}
		rewrote = append(rewrote, r)
	}
	if !found {
		return fmt.Errorf("sim order %s not found", id)
	}
	return rewriteAll(s.path("sim_orders.jsonl"), rewrote, hit)
}

// rewriteAll 全量重写 + 原子替换（临时文件 + rename）。
func rewriteAll[T any](path string, rows []T, _ T) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, r := range rows {
		bb, err := json.Marshal(r)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(append(bb, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	_ = f.Close()
	return os.Rename(tmp, path)
}

func normalizeState(st string) string { return st }

// Settings 用户可调参数（settings.json，单对象覆盖写）。
//
// AutoTrade 是「自动执行」总开关：默认关闭；live 模式启动时会被强制关闭，
// 必须在当次会话的 Settings 页重新确认开启（防止重启后无声自动下单）。
//
// 资金总额（CapitalUSDT）与自动交易差%门槛（MinDiffPct）由 Web 设置页修改，
// TUI 与 Web 共用同一份 settings.json；Web 授权码只保存哈希（WebAuthCodeHash），
// API Key/Secret/Passphrase 可由 Web 设置页写入（明文，文件权限 0600）。
type Settings struct {
	MinOptionEdgePP float64 `json:"minOptionEdgePp"` // 期权须高出 DCY 的百分点（20 = 20pp）
	MinAbsEdgePct   float64 `json:"minAbsEdgePct"`   // 单笔绝对收益差下限（%，0.5 = 0.5%）
	MaxStrikeGapPct float64 `json:"maxStrikeGapPct"` // 行权价相对现价最大偏差（%）
	AutoTrade       bool    `json:"autoTrade"`
	// CapitalUSDT 资金总额（USDT）：策略仓位与默认风控上限的基准。
	// ≤0 → 回落默认 5000（保持旧行为：总敞口 5000 / 单笔 2000）。
	CapitalUSDT float64 `json:"capitalUsdt"`
	// TotalExposureUSDT / PerOrderUSDT 风控绝对上限覆盖（0 = 按 CapitalUSDT 派生）。
	// 用于「策略只拿 1000 试仓，但账户已有 5000 在持」的场景：
	// 资金总额仍然决定策略单笔/总本金，风险上限单独按账户真实承受力设置。
	TotalExposureUSDT float64 `json:"totalExposureUsdt,omitempty"`
	PerOrderUSDT      float64 `json:"perOrderUsdt,omitempty"`
	// MinDiffPct 自动交易最低差%（年化、绝对值；0 = 不设门槛）。
	// 期权卖出要求「期权APR − DCY APR」达标；DCY 申购要求反方向差值达标，
	// 避免自动交易在两条腿几乎无差异时来回下单。只影响自动执行，不影响推荐动作。
	MinDiffPct float64 `json:"minDiffPct"`

	// ── v1 稳定策略（DCY-only）参数；旧字段保留兼容，但不再参与 v1 决策 ──
	StrategyMode       string  `json:"strategyMode,omitempty"` // "dcy_only"（默认）
	TenorMinDays       float64 `json:"tenorMinDays,omitempty"`
	TenorMaxDays       float64 `json:"tenorMaxDays,omitempty"`
	MinDaysToExpiry    float64 `json:"minDaysToExpiry,omitempty"`
	MaxExpiryGapHours  float64 `json:"maxExpiryGapHours,omitempty"`
	MaxAbsDelta        float64 `json:"maxAbsDelta,omitempty"`
	MinIvRvSpread      float64 `json:"minIvRvSpread,omitempty"`
	FairVolBuffer      float64 `json:"fairVolBuffer,omitempty"`
	MinPremiumOverFair float64 `json:"minPremiumOverFair,omitempty"`
	MinAbsYieldPct     float64 `json:"minAbsYieldPct,omitempty"`
	MinAPRPct          float64 `json:"minAprPct,omitempty"` // 本金年化地板（v2 资金 hurdle 的兜底）
	// v2 资金口径：
	//   CashAPRPct          现金/无风险年化（机会成本）
	//   MinCapitalExcessPct 资金口径相对现金的超额年化下限
	//   StressShockPct      压力情景跌幅（%）
	//   StressMaxLossPct    压力浮亏 / 资金上限（%）
	//   AllowNegativeEdge   允许 DCY 劣于期权（默认 false）
	CashAPRPct           float64 `json:"cashAprPct,omitempty"`
	MinCapitalExcessPct  float64 `json:"minCapitalExcessPct,omitempty"`
	StressShockPct       float64 `json:"stressShockPct,omitempty"`
	StressMaxLossPct     float64 `json:"stressMaxLossPct,omitempty"`
	AllowNegativeEdge    bool    `json:"allowNegativeEdge,omitempty"`
	MinEdgeRatio         float64 `json:"minEdgeRatio,omitempty"`
	MinEdgeAbs           float64 `json:"minEdgeAbs,omitempty"`
	MaxSpreadPct         float64 `json:"maxSpreadPct,omitempty"`
	DcyMaxPrincipalPct   float64 `json:"dcyMaxPrincipalPct,omitempty"`
	DcyMaxPrincipalUSDT  float64 `json:"dcyMaxPrincipalUsdt,omitempty"`
	DcyTotalPrincipalPct float64 `json:"dcyTotalPrincipalPct,omitempty"`
	DcyDeltaExposurePct  float64 `json:"dcyDeltaExposurePct,omitempty"`
	MaxConcurrent        int     `json:"maxConcurrent,omitempty"`
	DailyLossPct         float64 `json:"dailyLossPct,omitempty"`
	RVWindowDays         int     `json:"rvWindowDays,omitempty"`
	// MinIVPercentilePct 入场 IV 历史分位下限（0~100；0 = 关闭）。
	// 需要先积累样本（market.IVHistory 每小时一条），否则不生效。
	MinIVPercentilePct float64 `json:"minIvPercentilePct,omitempty"`
	// WebAuthCodeHash Web 授权码摘要（PBKDF2-SHA256；兼容旧版 SHA-256 十六进制）。
	// 空 = 未启用 Web 授权；遗忘时用 --web-code 重置，或删除 settings.json 重新生成。
	WebAuthCodeHash string `json:"webAuthCodeHash,omitempty"`
	// API 凭证（Web 设置页维护，落盘 settings.json 0600）。
	// 启动参数/环境变量优先；三项必须齐全才会被使用，设置页只回显掩码 Key
	// 与「是否配置」，绝不会把 Secret/Passphrase 回传给浏览器。
	APIKey        string `json:"apiKey,omitempty"`
	APISecret     string `json:"apiSecret,omitempty"`
	APIPassphrase string `json:"apiPassphrase,omitempty"`
	// 界面筛选记忆（TUI 退出后重启恢复）：方向 "" | "C" | "P"；
	// 结算本位 "" | "B"（币本位）| "U"（U本位）。
	// 只影响展示筛选，不影响策略/下单。
	SideFilter   string `json:"sideFilter,omitempty"`
	SettleFilter string `json:"settleFilter,omitempty"`
	// 表格排序记忆："price:desc" / "apr:asc" / "diff:desc" 等；
	// "" = 默认顺序（动作分组 → 到期 → 行权价）。只影响展示，不影响策略。
	SortBy string `json:"sortBy,omitempty"`
}

// DefaultSettings 生产默认值（与 strategy.DefaultParams 对齐）。
func DefaultSettings() Settings {
	return Settings{
		MinOptionEdgePP: 20,
		MinAbsEdgePct:   0.5,
		MaxStrikeGapPct: 0.5,
		AutoTrade:       false,
		CapitalUSDT:     5000,
		MinDiffPct:      0,

		StrategyMode:         "dcy_only",
		TenorMinDays:         10,
		TenorMaxDays:         30,
		MinDaysToExpiry:      3,
		MaxExpiryGapHours:    12,
		MaxAbsDelta:          0.30,
		MinIvRvSpread:        0.05,
		FairVolBuffer:        1.10,
		MinPremiumOverFair:   1.05,
		MinAbsYieldPct:       0.5,
		MinAPRPct:            12,
		CashAPRPct:           4,
		MinCapitalExcessPct:  4,
		StressShockPct:       30,
		StressMaxLossPct:     10,
		MinEdgeRatio:         0.10,
		MinEdgeAbs:           0.0005,
		MaxSpreadPct:         0.15,
		DcyMaxPrincipalPct:   0.10,
		DcyMaxPrincipalUSDT:  100,
		DcyTotalPrincipalPct: 0.30,
		DcyDeltaExposurePct:  0.05,
		MaxConcurrent:        3,
		DailyLossPct:         0.03,
		RVWindowDays:         30,
	}
}

// ApplyTo 把 v1 参数映射到 strategy.Params；零值保留策略默认，避免旧
// settings.json / 手改文件把阈值覆盖成 0（0 在策略层通常表示“不设门槛”）。
func (s Settings) ApplyTo(p *strategy.Params) {
	if p == nil {
		return
	}
	if s.CapitalUSDT > 0 {
		p.CapitalUSDT = s.CapitalUSDT
	}
	if s.StrategyMode != "" {
		p.StrategyMode = s.StrategyMode
	}
	if s.TenorMinDays > 0 {
		p.TenorMinDays = s.TenorMinDays
	}
	if s.TenorMaxDays > 0 {
		p.TenorMaxDays = s.TenorMaxDays
	}
	if s.MinDaysToExpiry > 0 {
		p.MinDaysToExpiry = s.MinDaysToExpiry
	}
	if s.MaxExpiryGapHours > 0 {
		p.MaxExpiryGapHours = s.MaxExpiryGapHours
	}
	if s.MaxAbsDelta > 0 {
		p.MaxAbsDelta = s.MaxAbsDelta
	}
	if s.MinIvRvSpread > 0 {
		p.MinIvRvSpread = s.MinIvRvSpread
	}
	if s.FairVolBuffer > 0 {
		p.FairVolBuffer = s.FairVolBuffer
	}
	if s.MinPremiumOverFair > 0 {
		p.MinPremiumOverFair = s.MinPremiumOverFair
	}
	if s.MinAbsYieldPct > 0 {
		p.MinAbsYield = s.MinAbsYieldPct / 100
	}
	if s.MinAPRPct > 0 {
		p.MinAPR = s.MinAPRPct / 100
	}
	if s.CashAPRPct > 0 {
		p.CashAPR = s.CashAPRPct / 100
	}
	if s.MinCapitalExcessPct > 0 {
		p.MinCapitalExcessAPR = s.MinCapitalExcessPct / 100
	}
	p.AllowNegativeEdge = s.AllowNegativeEdge
	if s.MinEdgeRatio > 0 {
		p.MinEdgeRatio = s.MinEdgeRatio
	}
	if s.MinEdgeAbs > 0 {
		p.MinEdgeAbs = s.MinEdgeAbs
	}
	if s.MaxSpreadPct > 0 {
		p.MaxSpreadPct = s.MaxSpreadPct
	}
	if s.DcyMaxPrincipalPct > 0 {
		p.DcyMaxPrincipalPct = s.DcyMaxPrincipalPct
	}
	if s.DcyMaxPrincipalUSDT > 0 {
		p.DcyMaxPrincipalUSDT = s.DcyMaxPrincipalUSDT
	}
	if s.DcyTotalPrincipalPct > 0 {
		p.DcyTotalPrincipalPct = s.DcyTotalPrincipalPct
	}
	if s.DcyDeltaExposurePct > 0 {
		p.DcyDeltaExposurePct = s.DcyDeltaExposurePct
	}
	if s.StressShockPct > 0 {
		p.StressShockPct = s.StressShockPct / 100
	}
	if s.StressMaxLossPct > 0 {
		p.StressMaxLossPct = s.StressMaxLossPct / 100
	}
	if s.MaxConcurrent > 0 {
		p.MaxConcurrent = s.MaxConcurrent
	}
	if s.DailyLossPct > 0 {
		p.DailyLossPct = s.DailyLossPct
	}
	if s.RVWindowDays > 0 {
		p.RVWindowDays = s.RVWindowDays
	}
	p.MinIVPercentile = s.MinIVPercentilePct
}

// LoadSettings 读取配置；文件不存在或字段缺失时回落到默认值（不报错）。
func (s *Store) LoadSettings() (Settings, error) {
	out := DefaultSettings()
	b, err := os.ReadFile(s.path("settings.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return DefaultSettings(), fmt.Errorf("parse settings.json: %w", err)
	}
	// 防御非法值（手改文件导致下单参数失效）。
	if out.MinOptionEdgePP <= 0 {
		out.MinOptionEdgePP = 20
	}
	if out.MinAbsEdgePct <= 0 {
		out.MinAbsEdgePct = 0.5
	}
	if out.MaxStrikeGapPct <= 0 {
		out.MaxStrikeGapPct = 0.5
	}
	// 资金总额决定默认风控上限；非法值回落 5000，避免手改文件把风控改成 0/负数。
	if out.CapitalUSDT <= 0 {
		out.CapitalUSDT = 5000
	}
	// 风控绝对上限覆盖：0 = 按资金总额派生；负值/超限视为未设置。
	// 单笔不得超过总敞口，否则任何新单都必然被拒。
	if out.TotalExposureUSDT < 0 || out.TotalExposureUSDT > 1e9 {
		out.TotalExposureUSDT = 0
	}
	if out.PerOrderUSDT < 0 || out.PerOrderUSDT > 1e9 {
		out.PerOrderUSDT = 0
	}
	if out.TotalExposureUSDT > 0 && out.PerOrderUSDT > out.TotalExposureUSDT {
		out.PerOrderUSDT = out.TotalExposureUSDT
	}
	// 未设总敞口覆盖时，单笔不能超过资金派生出的总敞口；坏值直接回落派生值。
	if out.TotalExposureUSDT == 0 && out.PerOrderUSDT > out.CapitalUSDT {
		out.PerOrderUSDT = 0
	}
	if out.MinDiffPct < 0 {
		out.MinDiffPct = 0
	}
	if out.MinDiffPct > 1000 {
		out.MinDiffPct = 1000 // 1000pp 已是极端值，防止手滑写出天文数字
	}
	// v1 参数：手改文件写 0/负数时回落到默认，避免把风控改成“全放行”。
	def := DefaultSettings()
	if out.StrategyMode != "legacy" && out.StrategyMode != "dcy_only" {
		out.StrategyMode = def.StrategyMode
	}
	if out.TenorMinDays <= 0 {
		out.TenorMinDays = def.TenorMinDays
	}
	if out.TenorMaxDays <= 0 {
		out.TenorMaxDays = def.TenorMaxDays
	}
	if out.MinDaysToExpiry <= 0 {
		out.MinDaysToExpiry = def.MinDaysToExpiry
	}
	if out.MaxExpiryGapHours <= 0 {
		out.MaxExpiryGapHours = def.MaxExpiryGapHours
	}
	if out.MaxAbsDelta <= 0 {
		out.MaxAbsDelta = def.MaxAbsDelta
	}
	if out.MinIvRvSpread <= 0 {
		out.MinIvRvSpread = def.MinIvRvSpread
	}
	if out.FairVolBuffer <= 0 {
		out.FairVolBuffer = def.FairVolBuffer
	}
	if out.MinPremiumOverFair <= 0 {
		out.MinPremiumOverFair = def.MinPremiumOverFair
	}
	if out.MinAbsYieldPct <= 0 {
		out.MinAbsYieldPct = def.MinAbsYieldPct
	}
	if out.MinAPRPct <= 0 {
		out.MinAPRPct = def.MinAPRPct
	}
	if out.CashAPRPct <= 0 || out.CashAPRPct > 50 {
		out.CashAPRPct = def.CashAPRPct
	}
	if out.MinCapitalExcessPct <= 0 || out.MinCapitalExcessPct > 100 {
		out.MinCapitalExcessPct = def.MinCapitalExcessPct
	}
	if out.StressShockPct <= 0 || out.StressShockPct > 90 {
		out.StressShockPct = def.StressShockPct
	}
	if out.StressMaxLossPct <= 0 || out.StressMaxLossPct > 100 {
		out.StressMaxLossPct = def.StressMaxLossPct
	}
	if out.MinEdgeRatio <= 0 {
		out.MinEdgeRatio = def.MinEdgeRatio
	}
	if out.MinEdgeAbs <= 0 {
		out.MinEdgeAbs = def.MinEdgeAbs
	}
	if out.MaxSpreadPct <= 0 {
		out.MaxSpreadPct = def.MaxSpreadPct
	}
	if out.DcyMaxPrincipalPct <= 0 {
		out.DcyMaxPrincipalPct = def.DcyMaxPrincipalPct
	}
	if out.DcyMaxPrincipalUSDT <= 0 {
		out.DcyMaxPrincipalUSDT = def.DcyMaxPrincipalUSDT
	}
	if out.DcyTotalPrincipalPct <= 0 {
		out.DcyTotalPrincipalPct = def.DcyTotalPrincipalPct
	}
	if out.DcyDeltaExposurePct <= 0 {
		out.DcyDeltaExposurePct = def.DcyDeltaExposurePct
	}
	if out.MaxConcurrent <= 0 {
		out.MaxConcurrent = def.MaxConcurrent
	}
	if out.DailyLossPct <= 0 {
		out.DailyLossPct = def.DailyLossPct
	}
	if out.RVWindowDays <= 0 {
		out.RVWindowDays = def.RVWindowDays
	}
	if out.MinIVPercentilePct < 0 || out.MinIVPercentilePct > 100 {
		out.MinIVPercentilePct = 0
	}
	// 授权码摘要：PBKDF2 串或旧版 64 位十六进制；写坏则视为未启用
	//（启动时会重新生成并打印）。这里只做格式判断，不引入 web 包依赖。
	if out.WebAuthCodeHash != "" && !validAuthHash(out.WebAuthCodeHash) {
		out.WebAuthCodeHash = ""
	}
	// API 凭证三项必须齐全；残缺配置直接清空，避免半套密钥被拿去签名。
	out.APIKey = strings.TrimSpace(out.APIKey)
	if out.APIKey == "" || out.APISecret == "" || out.APIPassphrase == "" {
		out.APIKey, out.APISecret, out.APIPassphrase = "", "", ""
	}
	// 筛选记忆只认合法枚举值，手改文件写坏则回落「全部」。
	switch out.SideFilter {
	case "C", "P":
	default:
		out.SideFilter = ""
	}
	switch out.SettleFilter {
	case "B", "U":
	default:
		out.SettleFilter = ""
	}
	// 排序记忆同理：只认「键:方向」枚举，坏值回落默认顺序。
	switch out.SortBy {
	case "price:asc", "price:desc", "apr:asc", "apr:desc", "diff:asc", "diff:desc":
	default:
		out.SortBy = ""
	}
	return out, nil
}

// validAuthHash 判断授权码摘要格式：PBKDF2 串（pbkdf2-sha256$iter$salt$hash）
// 或旧版 64 位十六进制 SHA-256。
func validAuthHash(spec string) bool {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "pbkdf2-sha256$") {
		parts := strings.Split(spec, "$")
		if len(parts) != 4 {
			return false
		}
		if _, err := strconv.Atoi(parts[1]); err != nil {
			return false
		}
		return parts[2] != "" && parts[3] != ""
	}
	b, err := hex.DecodeString(spec)
	return err == nil && len(b) == 32
}

// SaveSettings 覆盖写配置（临时文件 + rename，避免半截文件）。
func (s *Store) SaveSettings(v Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path("settings.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path("settings.json"))
}
