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
func (e *Executor) Positions() (PositionsView, error) {
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
			tail = fmt.Sprintf("→ 交付 %.6f %s，损益 %.6f %s（结算价 %.0f）",
				o.SettleSz, o.SettleCcy, o.YieldSz, o.YieldCcy, o.SettlePx)
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

// SyncLiveGuard 从平台现有仓位恢复风控敞口（live 模式启动时调用）：
//   - DCY 在持订单：notional（投资币种）→ USDT
//   - 期权空头：|pos| × ctVal × 现货
//
// 已在纸面单恢复过的模式（sim-*）不调用。幂等性：应在进程启动时调用一次。
func (e *Executor) SyncLiveGuard() (string, error) {
	if e.mode != ModeLive || e.client == nil {
		return "非 live 模式，跳过平台恢复", nil
	}
	var notes []string

	// 1) DCY 在持（order-history state 过滤在本地做：平台 state 枚举随时间演进）
	var dcdUSDT float64
	dcdCount := 0
	if dcds, err := e.client.GetDCDOrderHistory("", "", "", "", 100); err != nil {
		notes = append(notes, "DCY 订单不可得: "+err.Error())
	} else {
		for _, d := range dcds {
			if d.State != "" && d.State != "open" {
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
			u, err := e.toUSDT(-float64(p.Pos)*parseFloatz(ins.CtVal), base)
			if err != nil {
				notes = append(notes, fmt.Sprintf("%s 折算失败: %v", p.InstID, err))
				continue
			}
			optUSDT += u
			optCount++
		}
	}

	// 直接用聚合值恢复（明细已聚合，只关心总额与笔数）。
	e.guard.RestoreAggregate(dcdUSDT+optUSDT, dcdCount+optCount)

	msg := fmt.Sprintf("平台恢复：DCY %d 笔=%.2f USDT，期权空头 %d 笔=%.2f USDT",
		dcdCount, dcdUSDT, optCount, optUSDT)
	if len(notes) > 0 {
		msg += "（" + strings.Join(notes, "；") + "）"
	}
	_ = e.st.Audit(store.AuditEvent{Event: "restore", Mode: string(e.mode), Detail: msg})
	return msg, nil
}
