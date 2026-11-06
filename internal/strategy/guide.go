package strategy

import (
	"fmt"
	"math"
)

// GuideSection 策略说明中的一段。Web 与 TUI 共用同一份文本，避免两处说明漂移。
// Key=target 的段落是「目标价怎么选」，UI 会重点展示。
type GuideSection struct {
	Key   string   `json:"key,omitempty"`
	Title string   `json:"title"`
	Lines []string `json:"lines"`
	Note  string   `json:"note,omitempty"`
}

// Guide 策略说明的结构化文本。参数来自当前生效的 strategy.Params，
// 所以 UI 里的阈值和实际决策一致，不会出现「文档写 30%、实际 10%」。
type Guide struct {
	Title    string         `json:"title"`
	Intro    string         `json:"intro"`
	Sections []GuideSection `json:"sections"`
}

// BuildGuide 生成当前参数下的策略说明。
func BuildGuide(p Params) Guide {
	p = normalizeDCYParams(p)

	intro := fmt.Sprintf(
		"把 DCY 低买 PUT 理解成「挂一个低于现价的买单」：到期没跌破目标价，赚权利金；跌破，就按目标价买入。目标价由 OKX 挂出，策略只从这些档位里挑最划算的一档；挑不到就空仓。到期范围 %.0f–%.0f 天。",
		p.TenorMinDays, p.TenorMaxDays,
	)
	if p.StrategyMode == "legacy" {
		intro = "当前 strategyMode=legacy（旧 20pp/0.5% 阈值逻辑）。下面是 v1 稳定策略的说明；把 strategyMode 改回 dcy_only 即启用 v1。"
	}

	per := p.DcyMaxPrincipalUSDT
	if p.DcyMaxPrincipalPct > 0 {
		if x := p.CapitalUSDT * p.DcyMaxPrincipalPct; x < per {
			per = x
		}
	}
	total := p.CapitalUSDT * p.DcyTotalPrincipalPct
	deltaCap := p.CapitalUSDT * p.DcyDeltaExposurePct
	reqAPR, planDeploy := requiredPrincipalAPR(p)
	yield22 := math.Max(p.MinAbsYield, reqAPR*22/365)
	yield30 := math.Max(p.MinAbsYield, reqAPR*30/365)
	stressLoss := p.CapitalUSDT * p.StressMaxLossPct

	return Guide{
		Title: "策略说明 · v2（DCY-only · 资金口径）",
		Intro: intro,
		Sections: []GuideSection{
			{
				Key:   "target",
				Title: "目标价怎么选（先排除，再比性价比）",
				Lines: []string{
					"目标价 = OKX 双币赢产品挂出的买入价。策略不自己定价，只在这些档位里选。",
					fmt.Sprintf("排除不能做的：不是低买 PUT、目标价不低于现价、到期不在 %.0f–%.0f 天、剩余 ≤%.0f 天、没有同价同到期的期权对照。", p.TenorMinDays, p.TenorMaxDays, p.MinDaysToExpiry),
					fmt.Sprintf("排除风险太高的：|Delta| ≤ %.2f。目标价越接近现价，Delta 越大，越容易被行权。", p.MaxAbsDelta),
					fmt.Sprintf("排除不划算的：IV − RV ≥ %.0fpp，且权利金 ≥ %.2f × 保守公允价。波动率不够贵就不做。", p.MinIvRvSpread*100, p.MinPremiumOverFair),
					fmt.Sprintf("排除收益太低的（资金口径）：本金年化 ≥ %.0f%%（= 现金 %.0f%% + 超额 %.0f%% ÷ 计划部署 %.0f%%），且绝对收益 ≥ %.1f%%。22 天约需 %.2f%%，30 天约需 %.2f%%。", reqAPR*100, p.CashAPR*100, p.MinCapitalExcessAPR*100, planDeploy*100, p.MinAbsYield*100, yield22*100, yield30*100),
					"排除机会成本不划算的：DCY 收益必须 ≥ 同价同到期期权的 bid（扣费后）。期权市场为同一份风险付得更多时，不买 DCY。",
					"最后比性价比：score =（DCY 收益 − 公允价）/ max(|Delta|, 0.05)，选 score 最高的一档。",
				},
				Note: "一句话：不是选 APR 最高，也不是选离现价最近，而是选「每承担 1 份 Delta 风险，绝对 EV 最大」的那档；都不合格就空仓。",
			},
			{
				Title: "score 怎么理解",
				Lines: []string{
					"分子「DCY 收益 − 公允价」：这笔比保守估计多赚多少。",
					"分母「max(|Delta|, 0.05)」：被行权的风险；Delta 越大，分母越大。",
					"所以 score = 每承担 1 份 Delta 风险，能多赚多少；越高越划算。",
					"注意：score 高不等于绝对收益高。它可能选到「风险很小但只赚一点」的远档；如果收益低于资金的机会成本，空仓更好。",
					"Delta 太小但收益也低时，score 仍可能不低（分母小），所以顶部同时显示收益/年化，先看这个再看 score。",
				},
			},
			{
				Title: "为什么不是最高 APR / 最近目标价",
				Lines: []string{
					"最近的目标价：APR 最高，但 Delta 大；一次下跌可能把几个月权利金亏掉。",
					"最远的目标价：安全垫厚，但权利金低，常常低于公允价，长期是负 EV。",
					"策略不预测涨跌，只比较「同样的风险下，哪个目标价更划算」。",
				},
			},
			{
				Title: "判断顺序",
				Lines: []string{
					"方向 / 期限 / 目标价方向 → 同价同到期期权 → Delta/买盘 → IV/RV + 权利金 → 相对期权 edge → score → 资金/敞口/并发。",
					"任何一步失败都不会下单；reason code 会说明卡在哪一步（如 delta_too_high、iv_rv_too_low、no_benchmark）。",
					"期权腿只用来定价，不执行；DCY 是唯一可执行腿。",
				},
			},
			{
				Title: "资金与风险",
				Lines: []string{
					fmt.Sprintf("单笔本金 ≤ %.0f USDT（%.0f%% × 资金，且 ≤ %.0f 上限）；总本金 ≤ %.0f USDT（%.0f%%）。", per, p.DcyMaxPrincipalPct*100, p.DcyMaxPrincipalUSDT, total, p.DcyTotalPrincipalPct*100),
					fmt.Sprintf("组合累计 Delta 敞口 ≤ %.0f USDT（%.0f%% 资金，按全部在持 DCY 的 |delta|×本金 累计）；Delta 0.30 时通常只够开 1 笔。", deltaCap, p.DcyDeltaExposurePct*100),
					fmt.Sprintf("压力情景：现货 −%.0f%% 时，全部在持 DCY 浮亏 ≤ %.0f USDT（%.0f%% 资金）。", p.StressShockPct*100, stressLoss, p.StressMaxLossPct*100),
					fmt.Sprintf("并发 ≤ %d 笔；当日已实现亏损 ≥ %.0f%% 资金时暂停自动执行（纸面单结算 + live DCY 历史回填）。", p.MaxConcurrent, p.DailyLossPct*100),
					"DCY 全额资金、没有强平：最大损失是买入后继续下跌的浮亏，不是爆仓。",
					"1% 最大损失是期权腿的规则；v2 不执行期权，所以用本金 / 累计 Delta / 压力情景上限代替。",
				},
			},
			{
				Title: "什么时候一定不交易",
				Lines: []string{
					"没有同价同到期基准、RV/IV 缺失、|Delta| 超过上限、IV−RV 不够、权利金低于公允价、收益低于机会成本、额度用完、当日亏损达线。",
					"空仓是策略的一部分；不要为了用满额度下单。",
				},
			},
			{
				Title: "术语 / 怎么读页面",
				Lines: []string{
					"页面顶部的「本轮最优目标价」= 当前筛选下 score 最高的 dcy_ok 档；显示「空仓」就是没有合格档。",
					"目标价：DCY 产品的行权价，也是到期触发买入的价；「距现价%」是它相对现价的折价。",
					"Delta：被行权风险的近似，越接近 0 越不容易被行权。",
					"IV：DCY 报价反推的隐含波动率；RV：入场波动率 = max(EWMA(λ=0.94), 30d 实际波动率)，两者取高更保守；IV−RV 是卖方赚的波动率溢价。",
					"IV 分位：当前期权链 PUT 买盘 IV 在自身历史样本中的位置（每小时采样一次）；样本不足时显示「—」，不参与决策。",
					"自动执行只碰 dcy_ok；手动卖出 / 申购可以跳过策略建议（有买盘 / 产品在快照即可），但仍受执行器风控上限约束；先看 reason code 和 Delta。",
				},
			},
		},
	}
}
