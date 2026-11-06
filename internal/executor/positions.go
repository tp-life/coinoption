package executor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
)

// PositionsView 统一持仓视图：风控快照 + 纸面单 + 平台真实单。
type PositionsView struct {
	GeneratedAt time.Time
	Mode        Mode
	Guard       GuardState
	Sim         []store.SimOrder  // 纸面单（[SIM] 徽标）
	LiveDCD     []okx.DCDOrder    // 平台 DCY 订单（近 20 条）
	LiveOptions []okx.OptPosition // 平台期权仓位（仅卖方）
	Info        string            // 取数降级说明（私有限速 / 无 Key）
}

// Positions 组装视图（尽力而为：平台取数失败不阻塞纸面数据）。
//
// 5 秒缓存 + 单飞：Web 弹窗会被反复打开/多标签轮询，而 live 下每次要打
// 两个限速接口（DCY 历史 1/s、期权仓位），不缓存很容易撞 50011。
// 下单/结算/赎回后调用 invalidatePositions 立即失效。
func (e *Executor) Positions() (PositionsView, error) {
	e.posMu.Lock()
	if !e.posAt.IsZero() && time.Since(e.posAt) < 5*time.Second {
		v := e.posVal
		e.posMu.Unlock()
		return v, nil
	}
	e.posMu.Unlock()

	// 不持锁做网络 I/O：positionsUncached 会触发到期结算，而结算又要
	// invalidatePositions（同一把锁）——持锁就会自锁。
	v, err := e.positionsUncached()
	if err == nil {
		e.posMu.Lock()
		e.posAt, e.posVal = time.Now(), v
		e.posMu.Unlock()
	}
	return v, err
}

func (e *Executor) positionsUncached() (PositionsView, error) {
	e.SettleDueQuiet() // 先落地已到期纸面单，保证持仓/风控与页面一致
	v := PositionsView{
		GeneratedAt: time.Now(),
		Mode:        e.mode,
		Guard:       e.guard.Snapshot(),
	}
	rows, err := e.st.LoadSim()
	if err != nil {
		return v, fmt.Errorf("读纸面订单: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].State == rows[j].State {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return rows[i].State == "open" // 在持排前
	})
	v.Sim = rows

	if e.client != nil && e.mode != ModeSimLocal {
		if dcds, err := e.client.GetDCDOrderHistory("", "", "", "", 20); err != nil {
			v.Info += fmt.Sprintf("平台DCY订单不可得(%v)｜", err)
		} else {
			v.LiveDCD = dcds
		}
		if pos, err := e.client.GetOptionPositions(); err != nil {
			v.Info += fmt.Sprintf("平台期权仓位不可得(%v)｜", err)
		} else {
			for _, p := range pos {
				if float64(p.Pos) < 0 { // 只关心卖方
					v.LiveOptions = append(v.LiveOptions, p)
				}
			}
		}
	}
	return v, nil
}

// Lines 文本行（CLI Print 与 TUI 持仓页共用）。
func (v PositionsView) Lines() []string {
	var out []string
	g := v.Guard
	out = append(out, fmt.Sprintf("模式 %s｜在持 %d/%d 笔｜敞口 %.2f / %.0f USDT｜今日 %d/%d 笔",
		v.Mode, g.OpenCount, g.Limits.MaxConcurrent, g.ExposureUSDT, g.Limits.TotalExposureUSDT,
		g.DayCount, g.Limits.MaxSessionsPerDay))
	out = append(out, "", "[SIM] 纸面单：")
	if len(v.Sim) == 0 {
		out = append(out, "  （空）")
	}
	for _, o := range v.Sim {
		var amt, tail string
		switch o.Kind {
		case "option":
			amt = fmt.Sprintf("%s 张 行权 %.0f，权利金 %.8f %s",
				trimFloat(o.Amt), o.Strike, o.Premium, o.PremCcy)
		default: // dcd
			amt = fmt.Sprintf("%.4f %s，行权 %.0f，absYield %.8f",
				o.Amt, o.InvestCcy, o.Strike, o.AbsYield)
		}
		switch o.State {
		case "settled":
			// 损益统一用 PnlUSDT（含本金汇兑损益）；SettleSz/YieldSz 只是交付量，
			// 被行权时把 YieldSz 当损益会显示成「赚了收益」。
			tail = fmt.Sprintf("→ 交付 %.6f %s（结算价 %.0f），已实现损益 %+.2f USDT",
				o.SettleSz, o.SettleCcy, o.SettlePx, o.PnlUSDT)
			if o.YieldSz != 0 {
				tail += fmt.Sprintf("，结算收益 %.6f %s", o.YieldSz, o.YieldCcy)
			}
		default:
			tail = fmt.Sprintf("→ 开单 %s，到期 %s",
				o.CreatedAt.Format("01-02 15:04"), o.ExpTime.Format("2006-01-02"))
		}
		out = append(out, fmt.Sprintf("  [SIM] %-20s %-6s %s｜USDT≈%.2f %s",
			o.ID, o.Kind, amt, o.AmtUSDT, tail))
	}

	if v.Mode != ModeSimLocal {
		out = append(out, "", "[平台] DCY 订单（近 20）：")
		if len(v.LiveDCD) == 0 {
			out = append(out, "  （空或不可得）")
		}
		for _, d := range v.LiveDCD {
			out = append(out, fmt.Sprintf("  %s %s %s %s state=%s 收益 %s %s",
				d.OrdID, d.ProductID, d.NotionalSz, d.NotionalCcy, d.State, d.YieldSz, d.YieldCcy))
		}
		out = append(out, "", "[平台] 期权空头：")
		if len(v.LiveOptions) == 0 {
			out = append(out, "  （空或不可得）")
		}
		for _, p := range v.LiveOptions {
			out = append(out, fmt.Sprintf("  %s pos=%s 保证金模式 %s",
				p.InstID, trimFloat(float64(p.Pos)), p.MgnMode))
		}
	}
	if v.Info != "" {
		out = append(out, "", "[info] "+v.Info)
	}
	return out
}

// Print Human 渲染（CLI 通道）。
func (v PositionsView) Print() {
	fmt.Printf("== 持仓 @ %s ==\n", v.GeneratedAt.Format("15:04:05"))
	fmt.Println(strings.Join(v.Lines(), "\n"))
}

// ─── live 侧守卫恢复 ─────────────────────────────────────────────

// SyncLiveGuard 用平台现有仓位**替换**进程内风控敞口（live 启动 + 定时对账）：
//   - DCY 在持订单：notional（投资币种）→ USDT
//   - 期权空头：|pos| × ctVal × 现货
//
// 幂等：每次都用平台数据整体替换，而不是累加；这样到期/平仓后敞口能自动
// 释放（此前只在启动时累加一次，持仓到期后额度永远不回收，最终所有新单
// 都被 total_exposure 拒绝）。任一来源取数失败时不改动现有敞口。
func (e *Executor) SyncLiveGuard() (string, error) {
	if e.mode != ModeLive || e.client == nil {
		return "非 live 模式，跳过平台恢复", nil
	}
	var notes []string
	dcdOK, optOK := true, true

	// 1) DCY 在持（order-history state 过滤在本地做：平台 state 枚举随时间演进）
	var dcdUSDT float64
	dcdCount := 0
	if dcds, err := e.client.GetDCDOrderHistory("", "", "", "", 100); err != nil {
		dcdOK = false
		notes = append(notes, "DCY 订单不可得: "+err.Error())
	} else {
		for _, d := range dcds {
			// 只有明确「已结算/已赎回/已撤销/失败」才排除；未知 state 视为在持
			//（避免漏计导致敞口低估、重复申购）。
			switch strings.ToLower(strings.TrimSpace(d.State)) {
			case "settled", "redeemed", "canceled", "cancelled", "failed":
				continue
			}
			amt := parseFloatz(d.NotionalSz)
			if amt <= 0 {
				continue
			}
			u, err := e.toUSDT(amt, d.NotionalCcy)
			if err != nil {
				notes = append(notes, fmt.Sprintf("%s 折算失败: %v", d.OrdID, err))
				continue
			}
			dcdUSDT += u
			dcdCount++
		}
	}

	// 2) 期权空头
	var optUSDT float64
	optCount := 0
	if pos, err := e.client.GetOptionPositions(); err != nil {
		optOK = false
		notes = append(notes, "期权仓位不可得: "+err.Error())
	} else {
		for _, p := range pos {
			if float64(p.Pos) >= 0 {
				continue
			}
			ins, err := e.instrumentByID(p.InstID)
			if err != nil {
				notes = append(notes, fmt.Sprintf("%s 元数据不可得: %v", p.InstID, err))
				continue
			}
			base := ins.CtValCcy
			if base == "" {
				base = strings.Split(p.InstID, "-")[0]
			}
			// 每张名义 = ctVal × ctMult（BTC 0.01 / ETH 0.1）；只乘 ctVal 会把
			// 平台空头敞口放大 100 倍（BTC）/10 倍（ETH）。
			ctVal := parseFloatz(ins.CtVal)
			if m := parseFloatz(ins.CtMult); m > 0 {
				ctVal *= m
			}
			u, err := e.toUSDT(-float64(p.Pos)*ctVal, base)
			if err != nil {
				notes = append(notes, fmt.Sprintf("%s 折算失败: %v", p.InstID, err))
				continue
			}
			optUSDT += u
			optCount++
		}
	}

	// 两个来源都成功才整体替换；否则保持现有敞口（宁高勿低）。
	if dcdOK && optOK {
		e.guard.ResetOpen(dcdUSDT+optUSDT, dcdCount+optCount)
	}

	msg := fmt.Sprintf("平台恢复：DCY %d 笔=%.2f USDT，期权空头 %d 笔=%.2f USDT",
		dcdCount, dcdUSDT, optCount, optUSDT)
	if len(notes) > 0 {
		msg += "（" + strings.Join(notes, "；") + "）"
	}
	_ = e.st.Audit(store.AuditEvent{Event: "restore", Mode: string(e.mode), Detail: msg})
	return msg, nil
}

// RecordAudit 写入一条审计事件（Web 的自动交易开关/风控操作留痕）。
func (e *Executor) RecordAudit(ev store.AuditEvent) error {
	if ev.Mode == "" {
		ev.Mode = string(e.mode)
	}
	return e.st.Audit(ev)
}

// Audit 最近 n 条审计事件（TUI 日志页；最新在前）。
func (e *Executor) Audit(n int) ([]store.AuditEvent, error) {
	rows, err := e.st.LoadAudit()
	if err != nil {
		return nil, err
	}
	if n > 0 && len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// LoadSettings / SaveSettings TUI Settings 页的配置入口。
func (e *Executor) LoadSettings() (store.Settings, error) { return e.st.LoadSettings() }
func (e *Executor) SaveSettings(v store.Settings) error   { return e.st.SaveSettings(v) }
