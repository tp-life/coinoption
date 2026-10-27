// Package store — 订单与审计留痕。
//
// M3 v1 采用 JSONL 文件（每行一条 JSON 记录，追加写、原子 flush、
// 并发安全），不引入 CGO/第三方驱动；好处是可直接 `cat`/`jq` 对账。
// 后续如需 SQL 查询可平移为 SQLite（表 = 文件，行 = 记录）。
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	YieldSz   float64   `json:"yieldSz,omitempty"`   // 收益量
	YieldCcy  string    `json:"yieldCcy,omitempty"`
	SimOrdID  string    `json:"simOrdId,omitempty"` // OKX Demo 回单 ordId（sim-okx 期权）
	Note      string    `json:"note,omitempty"`
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
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("user home: %w", err)
		}
		dir = filepath.Join(home, ".coinoption")
	}
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
type Settings struct {
	MinOptionEdgePP float64 `json:"minOptionEdgePp"` // 期权须高出 DCY 的百分点（20 = 20pp）
	MaxStrikeGapPct float64 `json:"maxStrikeGapPct"` // 行权价相对现价最大偏差（%）
	AutoTrade       bool    `json:"autoTrade"`
}

// DefaultSettings 生产默认值（与 strategy.DefaultParams 对齐）。
func DefaultSettings() Settings {
	return Settings{
		MinOptionEdgePP: 20,
		MaxStrikeGapPct: 0.5,
		AutoTrade:       false,
	}
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
	if out.MaxStrikeGapPct <= 0 {
		out.MaxStrikeGapPct = 0.5
	}
	return out, nil
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
