package executor

import (
	"fmt"
	"sort"
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

// Print Human 渲染（CLI 通道）。
func (v PositionsView) Print() {
	fmt.Printf("== 持仓 @ %s（模式 %s）==\n", v.GeneratedAt.Format("15:04:05"), v.Mode)
	g := v.Guard
	fmt.Printf("[风控] 在持 %d/%d 笔｜敞口 %.2f / %.0f USDT｜今日 %d/%d 笔\n",
		g.OpenCount, g.Limits.MaxConcurrent, g.ExposureUSDT, g.Limits.TotalExposureUSDT,
		g.DayCount, g.Limits.MaxSessionsPerDay)

	fmt.Println("[SIM] 纸面单：")
	if len(v.Sim) == 0 {
		fmt.Println("  （空）")
	}
	for _, o := range v.Sim {
		var amt, tail string
		switch o.Kind {
		case "option":
			amt = fmt.Sprintf("%s 张 行权 %.0f，权利金 %.8f %s",
				trimFloat(o.Amt), o.Strike, o.Premium, o.SettleCcy)
		default: // dcd
			amt = fmt.Sprintf("%.4f %s，行权 %.0f，absYield %.8f",
				o.Amt, o.InvestCcy, o.Strike, o.AbsYield)
		}
		switch o.State {
		case "settled":
			tail = fmt.Sprintf("→ 交付 %.6f %s，损益 %.6f %s（结算价 %.0f）%s",
				o.SettleSz, o.SettleCcy, o.YieldSz, o.YieldCcy, o.SettlePx, o.Note)
		default:
			tail = fmt.Sprintf("→ 开单 %s，到期 %s",
				o.CreatedAt.Format("01-02 15:04"), o.ExpTime.Format("2006-01-02"))
		}
		fmt.Printf("  [SIM] %-20s %-6s %s｜USDT≈%.2f %s\n",
			o.ID, o.Kind, amt, o.AmtUSDT, tail)
	}

	if v.Mode != ModeSimLocal {
		fmt.Println("[平台] DCY 订单（近 20）：")
		if len(v.LiveDCD) == 0 {
			fmt.Println("  （空或不可得）")
		}
		for _, d := range v.LiveDCD {
			fmt.Printf("  %s %s %.4f %s state=%s 收益 %s %s\n",
				d.OrdID, d.ProductID, parseFloatz(d.NotionalSz), d.NotionalCcy,
				d.State, d.YieldSz, d.YieldCcy)
		}
		fmt.Println("[平台] 期权空头：")
		if len(v.LiveOptions) == 0 {
			fmt.Println("  （空或不可得）")
		}
		for _, p := range v.LiveOptions {
			fmt.Printf("  %s pos=%s 保证金模式 %s\n", p.InstID, trimFloat(float64(p.Pos)), p.MgnMode)
		}
	}
	if v.Info != "" {
		fmt.Println("[info]", v.Info)
	}
}
