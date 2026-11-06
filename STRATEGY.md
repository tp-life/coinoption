# 稳定策略 v2（DCY-only · 资金口径）

> 状态：**v2 规则已实现**（代码 + 回归测试；纸面/历史验证待跑）。
> 适用范围：`coinoption` 自动执行（Web + TUI），BTC/ETH，低买（PUT），DCY-only。
> 资金假设：1,000 USDT（代码默认 5,000，按 settings 调整）；期权腿暂不执行，只作为**机会成本基准**。
>
> v2 相对 v1 的变化：① 相对期权从「提示」改为**硬门槛**（edge ≥ 0）；② 收益门槛从「本金年化」改为**资金口径**（扣除现金机会成本与部署比例）；③ Delta 敞口从「单笔折算」改为**组合累计**，并新增 **−30% 压力情景**上限；④ 验收标准与上述口径对齐。

---

## 0. 一句话

**只有当 DCY 同时满足「模型说便宜、期权市场没更便宜、资金口径跑得赢现金、组合扛得住 −30%」时才卖 put；任何一条不满足就空仓。**

---

## 1. 目标与非目标

### 目标

- 在 1,000 USDT 资金下，用 DCY 低买（PUT）获取**风险可控、可解释**的卖方收益。
- 追求的是**资金口径的超额收益**（相对现金），不是单笔最高 APR、也不是最高资金利用率。
- 每一笔都能回答四个问题：
  1. 这笔的绝对 EV 为什么为正？（模型）
  2. 为什么不是期权？（机会成本）
  3. 相对现金值不值？（资金 hurdle）
  4. 最坏/压力情况亏多少？（组合风控）

### 非目标

- 不执行期权（1 张 = 0.01 BTC 名义，超出单笔预算；仅作定价基准）。
- 不做裸卖 CALL、不做跨结算本位、不做杠杆。
- 不做价差/组合保证金/动态对冲。
- 不追求"每天有单"或"用满额度"。

---

## 2. 核心原则

1. **绝对 EV 优先**：先问"卖出的波动率是否比模型公允价贵"。
2. **机会成本是硬约束**：期权 bid 是同一份风险的市场价；DCY 低于它就不做。
3. **资金口径**：本金收益必须换算成"相对现金的超额收益"再比较；本金 12% 在 30% 部署下只是资金 3.6%。
4. **风险先行**：累计 Delta、压力情景、单笔/总本金、流动性、数据新鲜度，任一不满足即否决。
5. **不交易是合法动作**；每次拒绝/成交都有 reason code 与输入快照。

---

## 3. 数据与口径

### 3.1 输入

| 数据 | 来源 | 用途 |
|---|---|---|
| DCY 产品 | OKX `/finance/sfp/dcd/products` | strike、expTime、absYield、min/max/step |
| 现货 | `market.Snapshot.SpotPrice` | BS、premium fraction、Delta、sizing |
| 期权基准 | OKX 期权 tickers（bid/bidSz/bidVol） | 期权权利金、买盘、IV/Delta |
| 30d 已实现波动率 | OKX 日线 `/market/candles?bar=1D` | RV_30 |
| 资金/额度 | `executor.GuardState` + `settings.CapitalUSDT` | sizing、集中度、压力情景 |
| 现金年化 | `settings.CashAPRPct`（默认 4%） | 机会成本 |

### 3.2 统一口径（v2 核心）

记：

- `C` = 资金总额（`CapitalUSDT`）
- `D` = **计划部署比例**（见下）
- `P` = 单笔本金（USDT）
- `y` = `absYield` = 持有期绝对收益 / 行权价名义
- `d` = 剩余天数

定义：

```text
apr_principal       = y × 365 / d                 # 本金年化
capital_excess_apr  = D × (apr_principal − CashAPR)  # 资金口径超额年化
```

**计划部署比例 D** 取「总本金上限」和「Delta 上限能容纳的本金」的较小值（用最坏 Delta 估）：

```text
D = min(DcyTotalPrincipalPct, DcyDeltaExposurePct / MaxAbsDelta)
```

默认：`min(0.30, 0.05/0.30) = 16.7%`。含义：**如果每笔都打满 Delta 上限，最多只能部署 1/6 资金**；30% 只是名义上限。所以真正的并发约束是 Delta，不是 `MaxConcurrent`。

### 3.3 期权基准口径（v2 改为硬约束）

- `p_dcy = y`（每 1 USDT 行权价名义的收益）。
- `p_opt = bid / K`（U 本位期权；必须是 USDT 结算）。
- `fee_fraction = min(0.0003, 0.07 × p_opt)`（OKX 普通档 3bp，封顶 7% 权利金）。
- `p_opt_net = max(0, p_opt − fee_fraction)`。
- `edge = p_dcy − p_opt_net`：**正数 = DCY 比期权更划算；负数 = 期权市场为同一风险付得更多**。

`edge < 0` 时：v2 **默认拒绝**（`dcy_dominated` / `option_benchmark_better`）。理由：期权市场价是同一份 short put 的可观测价格；DCY 低于它说明平台抽走了价差，没有理由接受。若确实无法交易期权，替代是现金或别的档位，而不是"明知更差还买"。`AllowNegativeEdge=true` 可显式关闭该门槛（不推荐）。

### 3.4 数学定义

Black-Scholes（r=0，加密期权常用简化）：

```text
d1 = (ln(S/K) + 0.5σ²T) / (σ√T)
d2 = d1 − σ√T
put = K·N(−d2) − S·N(−d1)
put_delta = N(d1) − 1      # 负数
```

- `RV_30`：最近 30 个**已收盘**日收益率的标准差 × √365；少于 20 个收益则 `rv_missing`。
- `RV_EWMA`：RiskMetrics 指数加权（λ=0.94，半衰期约 11 天），对 regime 切换更敏感。
- `RV_entry = max(RV_EWMA, RV_30)`：入场用两者较高值（更保守；任一口径都不能单独放宽门槛）。`RVWindowDays` 可改主窗口。
- `RV_stress = RV_entry × 1.10`。
- `fair_fraction = put(S, K, T, RV_stress) / K`。
- `iv_dcy`：解 `put(S, K, T, σ) / K = p_dcy`。
- `abs_delta`：优先 `opt-summary.delta`（已接入 REST + WS，取绝对值），否则 `bidVol + BS`，再否则 `iv_dcy + BS`；都失败 → `delta_missing`。
- `IV_pct`：期权链 PUT 买盘 IV 中位数在自身历史样本中的分位（每小时采样）。

### 3.4.1 IV 分位（已接入，默认关闭）

`market.IVHistory` 每小时记录一条「PUT 买盘 IV 中位数」到 `~/.coinoption/iv_samples.jsonl`；样本 ≥40 条且跨度 ≥7 天时给出分位。
`MinIVPercentile > 0` 时，分位低于该值 → `iv_percentile_low`。**默认 0（关闭）**：需要先跑 1–2 周积累样本，再决定是否作为门槛。
样本不足时 `IV_pct = -1`，不参与决策。
- `score = (p_dcy − fair_fraction) / max(abs_delta, 0.05)`。

### 3.5 行权派息（已核实）

OKX 官方 FAQ 的低买结算表（2026-11 核对）：

| 到期价 | 收到 | 币种 |
|---|---|---|
| `> 目标价` | 数量 × (1 + y) | 计价币（USDT） |
| `≤ 目标价` | 数量 / 目标价 × (1 + y) | 基础币（BTC） |

结论：**被行权时，收益与本金一起按行权价折成 BTC**，到期 USDT 价值 = `P × (1+y) × S_T/K`。
因此 DCY 低买 = 卖出同价 put，且权利金在行权时也以现货结算，不是额外发 USDT。

相对「权利金以 USDT 另付」的等价 short put，缺口为：

```text
gap = y × (1 − S_T/K) ≈ y × |1 − S/K|
```

22 天、`y=2.7%`、10% OTM（`S/K≈0.9`）时约 **27bp**；`y=1.7%` 时约 17bp。
旧假设「行权不发收益」估算的 54bp 偏大一倍，但仍与 5% 门槛同阶，所以 `fair` 仍需 buffer。

**已按该口径实现**：`executor.SettleDual` 的 P 方向 `YieldSz = (本金/K) × y`（BTC），
`PnlUSDT = 交付价值 − 本金`；纸面回放/结算/验收数字都以它为准。

补充：结算价取到期日北京时间 15:00–16:00 的指数均价，16:00（UTC+8，即 08:00 UTC）到账，
与 DCY 的 `expTime` 一致。

---

## 4. 决策流程

### 4.1 Universe

| 条件 | 不满足的 reason |
|---|---|
| 标的 BTC/ETH | `unsupported_underlying` |
| 方向 P（低买，投 USDT） | `unsupported_side` / `strike_wrong_side` |
| `10 ≤ d ≤ 30` 天，且 `d > 3` | `out_of_tenor` |
| `strike < spot` | `strike_wrong_side` |
| `absYield > 0` | `dcy_yield_zero` |
| 产品未到期、未过申购截止 | `expired` / `trade_closed`（运行时） |

### 4.2 基准对齐

- 同标的、同方向 P、**行权价完全相等**、**同 UTC 日期且到期差 ≤ 12h**、同结算本位（U 本位）、有 bid。
- 无候选 → `no_benchmark`；跨日/差 >12h → `expiry_mismatch`；本位不符 → `settle_mismatch`；>2h 仅 `expiry_gap_warn`。
- 期权基准报价只用于定价与机会成本；**v1/v2 不要求它的报价新鲜度**（真正下单前会重新取 DCY 报价，期权报价仅影响 edge 的比较方向）。若将来执行期权，再启用 5s 时效。

### 4.3 硬风控（一票否决）

| 条件 | 默认 | reason |
|---|---|---|
| `abs_delta ≤ MaxAbsDelta` | 0.30 | `delta_too_high` |
| **组合累计 Delta** `Σ P×delta ≤ 5%×C` | 50 USDT | `delta_exposure_cap`（运行时） |
| **−30% 压力浮亏** `Σ P×max(0,1−0.7S/K) ≤ 10%×C` | 100 USDT | `stress_loss_cap`（运行时） |
| 期权买盘 `bidSz ≥ 1` | — | `bid_size_insufficient` |
| DCY 报价年龄 ≤ 60s；现货年龄 ≤ 60s | 60s | `stale_quote`（自动执行层） |
| 单笔本金 ≤ `min(10%×C, 100)` | 100 | `size_above_cap` |
| 总本金 ≤ 30%×C | 300 | `total_exposure_cap` |
| 并发 ≤ 3；当日提交 ≤ 10 | — | `max_concurrent` / `per_day` |
| 当日已实现亏损 ≥ 3%×C | 3% | `daily_loss_limit` |
| 可用 USDT ≥ principal | — | `no_capital`（live 下单前由平台拒付兜底） |
| `minSize ≤ P ≤ maxSize`，按 `stepSz` 取整 | — | `size_below_min` |

> **关于 Delta 敞口**：v2 由执行器在下单前按**全部在持 DCY**（纸面 + live 平台订单）累计计算，公式 `Σ principal × |delta|`；单笔 Delta 从 DCY 自身报价反解（缺失时按 0.5 保守估计）。默认 5% 资金 = 50 USDT，delta 0.30 时**只够开 1 笔**（100×0.3=30；第二笔 60>50），delta 0.15 时可开 3 笔。所以"并发 3 笔"只有在低 Delta 档位才可能达到。

> **关于压力情景**：用「现货 −30%」重估所有在持 DCY 的浮亏（不考虑权利金），加上新单后不得超过 10% 资金。−50% 情景在验收里单独看，不作为下单门槛（否则会过度保守）。

> **关于 1% 最大损失**：1% 是**期权腿**的规则；v2 是 DCY-only，DCY 全额资金、无强平，最大损失是"本金被锁定/浮亏"。因此用「单笔本金 10% + 总本金 30% + 累计 Delta 5% + 压力 10%」代替，并在 §7 把验收标准改成与之一致。

### 4.4 绝对 EV（模型门槛）

必须同时满足：

1. `iv_dcy − RV_30 ≥ 5pp`。
2. `p_dcy ≥ 1.05 × fair_fraction`。
3. **资金 hurdle**：`apr_principal ≥ max(MinAPR, CashAPR + MinCapitalExcessAPR / D)`。
4. `y ≥ MinAbsYield`。

默认值代入（D=16.7%）：

```text
required_apr = max(12%, 4% + 4%/0.1667) = max(12%, 28%) = 28%
```

即 22 天需要 `y ≥ 1.69%`、10 天需要 `y ≥ 0.77%`、30 天需要 `y ≥ 2.30%`。

> 这条取代了 v1 的 `MinAPR=12%` 单一口径。`MinAPR` 现在只是**本金年化的地板**；真正生效的是资金 hurdle。

**实际强度**（假设 `市场 IV = RV+5pp`、22 天、`|delta|=0.3`）：

| RV30 | 模型门槛（IV/RV + fair） | 资金门槛（28% 本金 APR） | 实际生效 |
|---|---|---|---|
| 20% | 1.03% | 1.69% | **资金** |
| 30% | 1.69% | 1.69% | 持平 |
| 45% | 2.73% | 1.69% | **模型** |
| 60% | 3.83% | 1.69% | **模型** |

含义：低波动时不再"随便过线"（资金门槛补上），高波动时仍要求真实波动率溢价（模型门槛）。

### 4.5 相对基准（机会成本，硬门槛）

```text
min_edge = max(0.10 × p_opt_net, fee_fraction + MinEdgeAbs)
```

规则（v2）：

1. `edge < 0` → **拒绝**：期权市场更贵。`option_executable=true` 用 `dcy_dominated`，否则用 `option_benchmark_better`；`AllowNegativeEdge=true` 时降级为 warning 并放行。
2. `0 ≤ edge < min_edge` → **允许**（`dcy_ok`），因为期权不可执行时 `min_edge` 只是"别为了几乎一样硬做"的排序约束；记录 `opportunity_cost = min_edge − edge` 供审计。
3. `edge ≥ min_edge` → `dcy_ok`。

> 注意：`option_executable=false`（当前固定）并不意味着可以忽略期权价格——它只意味着"卖期权"不是可执行替代，但期权的 bid 仍是**同一风险的机会成本**。这是 v1 最大的逻辑漏洞，v2 已改为硬门槛。

### 4.6 排序

只对通过 4.1–4.5 的候选排序：

1. `score` 降序（`(p_dcy − fair) / max(|delta|, 0.05)`）；
2. `edge` 降序；
3. 到期日升序；
4. `productID` 字典序。

### 4.7 Sizing

```text
per_trade_cap = min(C × 10%, 100)
total_cap     = C × 30%
principal     = min(per_trade_cap, total_cap − open_principal, product.maxSize)
principal     = floor(principal / stepSz) × stepSz
if principal < max(product.minSize, 10): skip
```

单笔仍保留 Delta 折算封顶（防御性，通常不 binding）；**组合级** Delta 与压力上限改为下单前校验（见 4.3），避免"单笔 cap 永远不生效"的伪风控。

### 4.8 执行

- 下单前用最新快照复算 4.3–4.5；DCY 先取实时 quote，再比快照 `absYield`：下降 >0.5% 放弃（百分数形态已归一化）。
- 写请求只有 50011 重试；超时/5xx/断连返回「结果未知」：**不释放敞口、不自动重试**，保持去重占位，人工按 `clOrdId`/`quoteId` 对账。
- 成交后记录：`productID`、principal、`p_dcy`、`fair`、`iv_dcy`、`RV_30`、`abs_delta`、`edge`、`score`、各 reason code。
- 失败/拒绝同样落库。

### 4.9 监控与暂停

- 结算后回填已实现 `PnlUSDT`（含本金汇兑损益）。
- 自动暂停条件（v2 已实现）：当日已实现亏损 ≥ 3%×C；DCY 报价 >60s；现货 >60s；读取盈亏失败（fail-closed）。
- 后续可加：连续 3 笔亏损、数据源连续失败 >5 分钟。

### 4.10 决策伪代码

```text
for dcy in products:
    if not universe(dcy): reject
    bench = find_benchmark(dcy)
    if bench == nil: reject no_benchmark
    rv = rv30(base); if missing: reject
    iv = implied_vol(p_dcy, S, K, T); if missing: reject
    fair = bs_put(S, K, T, rv*1.10) / K

    delta = bench.delta or bs_delta(bench.iv or iv)
    if abs(delta) > MaxAbsDelta: reject delta_too_high

    if iv - rv < 5pp: reject iv_rv_too_low
    if p_dcy < 1.05*fair: reject premium_below_fair
    if apr_principal < required_apr: reject yield_too_low

    p_opt_net = max(0, bench.bid/K - fee)
    edge = p_dcy - p_opt_net
    if edge < 0 and not AllowNegativeEdge: reject dcy_dominated

    score = (p_dcy - fair) / max(delta, 0.05)
    candidates.append(...)

sort by score desc, edge desc, expiry asc, productID asc

for c in candidates:
    principal = size(c)
    if not runtime_risk(c, principal, spot):
        continue     # 累计 Delta / 压力 / 总敞口 / 并发 / 日限
    return subscribe(c, principal)
return no_trade
```

---

## 5. 参数表

| 参数 | 默认 | 范围 | 含义 |
|---|---|---|---|
| `StrategyMode` | `dcy_only` | `dcy_only` / `legacy` | v2 只做 DCY |
| `TenorMinDays` / `TenorMaxDays` | 10 / 30 | 7–60 | 期限窗口 |
| `MinDaysToExpiry` | 3 | 1–7 | 到期周保护 |
| `MaxExpiryGapHours` | 12 | 2–24 | DCY/期权到期差硬上限 |
| `MaxAbsDelta` | 0.30 | 0.10–0.40 | 单笔 Delta 上限（回放评估后保留 0.30，见 7.4） |
| `MinIvRvSpread` | 0.05 | 0.03–0.10 | IV 高于 RV 的波动率点 |
| `FairVolBuffer` | 1.10 | 1.0–1.3 | 公允价的 RV 缓冲 |
| `MinPremiumOverFair` | 1.05 | 1.0–1.2 | 权利金 / 公允价下限 |
| `MinAbsYield` | 0.005 | 0.002–0.02 | 单笔绝对收益地板 |
| `MinAPR` | 0.12 | 0.01–0.50 | **本金**年化地板（资金 hurdle 的兜底） |
| `CashAPR` | 0.04 | 0.01–0.20 | 现金/无风险年化（机会成本） |
| `MinCapitalExcessAPR` | 0.04 | 0.01–0.30 | **资金口径**超额年化下限 |
| `DcyTotalPrincipalPct` | 0.30 | 0.05–0.60 | 总本金 / 资金（名义上限） |
| `DcyDeltaExposurePct` | 0.05 | 0.01–0.20 | 组合累计 Delta / 资金（真正的并发约束） |
| `StressShockPct` | 0.30 | 0.10–0.60 | 压力情景跌幅 |
| `StressMaxLossPct` | 0.10 | 0.02–0.30 | 压力浮亏 / 资金上限 |
| `AllowNegativeEdge` | false | — | 是否允许 DCY 劣于期权（不推荐开启） |
| `MinEdgeRatio` / `MinEdgeAbs` | 0.10 / 0.0005 | — | 期权可执行时的最小优势 |
| `DcyMaxPrincipalPct` / `DcyMaxPrincipalUSDT` | 0.10 / 100 | — | 单笔本金上限 |
| `MaxConcurrent` | 3 | 1–5 | 并发笔数上限 |
| `DailyLossPct` | 0.03 | 0.01–0.10 | 日已实现亏损暂停线 |
| `RVWindowDays` | 30 | 14–60 | RV 主窗口（EWMA 固定 λ=0.94） |
| `MinIVPercentile` | 0 | 0–90 | IV 历史分位下限（0=关闭；需先积累样本） |

> `--limits` 仍可覆盖总敞口/单笔/并发/日笔数；Delta 与压力上限由设置页参数派生，不受 `--limits` 影响。

---

## 6. Reason codes

| code | 含义 | 动作 |
|---|---|---|
| `dcy_ok` | 全部通过 | 申购 |
| `out_of_tenor` / `unsupported_underlying` / `unsupported_side` | 期限/标的/方向不符 | 跳过 |
| `strike_wrong_side` | PUT 行权价 ≥ 现价 | 跳过 |
| `dcy_yield_zero` / `expired` | 无收益 / 已到期 | 跳过 |
| `no_benchmark` / `expiry_mismatch` / `settle_mismatch` | 无同价同到期同本位基准 | 跳过 |
| `delta_too_high` / `delta_missing` | 单笔 Delta 超限/缺失 | 跳过 |
| `rv_missing` / `iv_missing` | 数据缺失 | 跳过 |
| `iv_rv_too_low` | 波动率溢价不足 | 跳过 |
| `premium_below_fair` | 低于保守公允价 | 跳过 |
| `yield_too_low` | 低于资金 hurdle（本金 APR / 绝对收益） | 跳过 |
| **`dcy_dominated`** | **期权更优（edge<0）且可执行** | **跳过** |
| **`option_benchmark_better`** | **期权更优（edge<0）但不可执行** | **跳过（机会成本）** |
| `edge_too_small` | 期权可执行且优势不足 | 跳过 |
| `bid_size_insufficient` / `stale_quote` | 买盘不足 / 报价过期 | 跳过 |
| `size_below_min` / `size_above_cap` | 规模不合法 | 降额或跳过 |
| `total_exposure_cap` / `delta_exposure_cap` / `stress_loss_cap` | 总本金 / 累计 Delta / 压力损失超限 | 跳过（运行时） |
| `max_concurrent` / `per_day` / `daily_loss_limit` | 并发 / 日笔数 / 日亏损 | 暂停 |
| `no_capital` | 可用 USDT 不足 | 跳过 |
| `expiry_gap_warn` / `spread_wide_warn` | 到期差 >2h / 价差偏宽 | 仅提示 |

> `delta_exposure_cap` / `stress_loss_cap` / `daily_loss_limit` 由执行器/自动引擎在提交前判定，不改变 `Decision.Action`；失败会写入 `autoFailures` 与审计。
>
> **自动 vs 手动**：以上执行器上限只对自动执行**硬拒绝**；手动下单（TUI/Web/CLI）改为「提示 + 审计」——`Outcome.Warnings` 列出越限项，审计记「手动放行（仅提示）」，额度仍按真实提交累加。交易所级硬规则（最小申购、步长、截止时间、无买盘）保持不变。

---

## 7. 验证与验收（v2 重写）

### 7.1 方法（取代「4–6 周 / ≥5 笔」）

卖波动率策略天然约 70–75% 胜率，短样本几乎必然盈利，**不能用"收益 > 0"证明有效**。改为：

1. **历史滚动回放**：2–3 年日线，按当时可见的 RV/期限逐日重放；没有真实期权历史 IV 时用 RV+skew 合成，并在报告里明确标注。
2. **情景压力**：现货单日 −20% / −30% / −50%；以及「波动率翻倍、价格不动」（vega 侧）。
3. **三基准对比**：现金（CashAPR）、卖出同档期权（理论 P&L）、本策略。
4. **样本门槛**：≥30 笔成交，或覆盖 ≥6 个月且至少一个完整到期周期。

### 7.2 验收标准（与 4.3 的风控口径一致）

1. **合规**：100% 成交在成交时点通过 4.3–4.5 全部门槛；审计可逐笔复算。
2. **资金超额收益**：样本期内 `capital_excess_apr ≥ MinCapitalExcessAPR`（默认 4%），扣费后。
3. **回撤**：历史回放最大回撤 ≤ **12%×C**。
4. **压力**：组合在 −30% 情景浮亏 ≤ **10%×C**；−50% 情景 ≤ **15%×C**。
5. **单笔损失**：任一已结算单笔亏损 ≤ **10%×C**（= 单笔本金全损；DCY 不能止损）。
6. **机会成本**：100% 成交满足 `edge ≥ 0`。
7. **波动率校准**：持有期实际 RV 的中位数 ≤ 入场 `iv_dcy` 的 0.9 倍。
8. **不是全拒**：决策周期中至少 10% 存在 `dcy_ok` 候选；否则先看拒绝原因分布，再决定是否调参（不允许直接放宽）。
9. **胜率、单笔正收益、总收益 > 0 不作为验收指标。**

### 7.3 不达标时

先按 reason code 与 RV 分桶看拒绝/成交分布；只允许改**口径错误**（如派息假设、Delta 来源），不允许为了"有单"降门槛。任何参数变更都要重跑同一份回放。

### 7.4 回放工具与基线

```bash
# 2 年日线、22 天、10% OTM；扫描 beta × Δmax × 两种 edge 口径，输出 JSON
go run ./cmd/coinoption --replay --days 730 --capital 1000 --tenor 22 \
  --strikes 0.90 --betas 1.0,0.99,0.98,0.95 --deltas 0.30,0.25,0.20 --out replay.json
```

实现：`internal/backtest`（纯函数，无网络）+ `cmd/coinoption/replay.go`（公开日线，无需 Key）。
每一天只用**当时可见**的收盘价估 RV/EWMA，合成 DCY 报价 `y = beta × BS put(IV=RV×1.15)`，
到期按 §3.5 口径结算；组合闸门复刻执行器的累计 Delta / 压力上限。（`beta` 模拟平台抽走的价差。）

**合成路径基线**（3%/日波动、含一段 −2%/日急跌，900 天；不是真实 BTC 历史，只用于验证门槛数学）：

| beta | Δmax | 硬门槛成交 | 被行权 | 收益 | 最大回撤 | −30% 压力 | vega 压力 |
|---|---|---|---|---|---|---|---|
| 1.00 | 0.30 | 66 | 12 | +1.44% | 9.57% | 9.12% | 1.38% |
| 1.00 | 0.25 | 66 | 15 | +0.48% | 8.60% | 10.84% | 1.50% |
| 1.00 | 0.20 | 15 | 6 | −5.16% | 6.11% | 9.34% | 1.06% |
| 0.98 | 0.30 | **0** | — | — | — | — | — |
| 0.95 | 0.30 | **0** | — | — | — | — | — |

读法：

- **Δmax 保留 0.30**：0.25 只允许 2 笔（累计 50=上限），压力反而升到 10.84%；0.20 让成交掉 77%、收益转负。
  0.30 时组合上限只容得下 1 笔，是最保守的一档。`MaxAbsDelta` 不是主约束，**累计 Delta 才是**。
- **edge 硬门槛非常紧**：`beta=0.98`（平台抽 2%）就 0 成交。手续费 3bp 只能吸收约 1% 的折价。
  所以「6 个月 ≥30 笔」在当前门槛下几乎不可能达成——这是 §7.2.8 要观察的第一件事。
  若回放/实盘确认 DCY 长期比期权低 2–5%，应讨论「允许 edge ≥ −X 的小幅让价」而不是直接关掉门槛。
- 压力 9.12% 已贴近 10% 上限：`DcyDeltaExposurePct` 或单笔本金再放大就会超标。

---

## 8. 实现状态

- [x] 期权机会成本硬门槛（`edge ≥ 0`，可用 `AllowNegativeEdge` 回退）
- [x] 资金口径 hurdle（`CashAPR` + `MinCapitalExcessAPR` + 计划部署 `D`）
- [x] 组合累计 Delta 敞口（执行器下单前按全部在持 DCY 累计）
- [x] −30% 压力情景上限（`stress_loss_cap`）
- [x] 日已实现亏损暂停（纸面结算 + live DCY 历史）
- [x] DCY/现货报价过期暂停；自动结算释放敞口
- [x] 纸面 P&L 修正（含本金汇兑损益、期权权利金）
- [x] 历史滚动回放 + 情景压力脚本（`--replay`，见 7.4）
- [x] 提前赎回（`--redeem-quote` / `--redeem`，live only；BTC/ETH 币对平台不支持）
- [x] IV 分位 / EWMA 替代单窗口 RV（`RV_entry = max(EWMA, RV_30)`；IV 分位采样 + 可选门槛）
- [x] `opt-summary.delta` 接入（REST 5 分钟刷新 + WS 同批缓存）
- [x] 核实行权派息规则（§3.5：收益随本金按行权价折成 BTC）
- [x] 组合级 `MaxAbsDelta` 回放评估：**保留 0.30**（0.20 成交 −77% 且收益转负，见 7.4）

### 8.1 工程加固（与策略同批）

- [x] `opt-summary.delta` 接入（REST 5 分钟 + WS 同批缓存，失败静默回退 IV 反解）
- [x] RV 改 `max(EWMA, RV_30)`，另存 RV7/RV90/分位供展示
- [x] IV 历史采样（`iv_samples.jsonl`，每小时/标的）+ 可选分位门槛
- [x] 提前赎回 CLI（`--redeem-quote` / `--redeem`；仅 live，BTC/ETH 币对平台不支持，可能亏损，不自动）
- [x] 数据目录 flock 独占锁（TUI/可交易 Web/下单命令；只读看板与查询命令不占锁；`--no-lock` 可关）
- [x] Web 授权码改 PBKDF2-SHA256（21 万次迭代 + 随机盐），旧 SHA-256 兼容升级；空授权码 fail-closed
- [x] 持仓视图 5s 缓存（live 下每次要打 DCY 历史 + 期权仓位两个限速接口）
- [x] 优雅退出：SIGINT/SIGTERM 后停止接新连接，在途请求最多 10s 收尾
- [x] WS 单个订阅被拒自动重试 3 次（此前只报错，相关合约一直没行情）

---

## 9. 未决/风险

- **行权派息**：已按官方 FAQ 核实（§3.5），旧「不发收益」假设高估缺口约 2 倍；`fair` 仍保留 10% 波动率 buffer。
- **Skew**：BS 单一波动率会低估 OTM put 的公允价；v2 用期权 bid 做硬约束部分抵消，但 `fair` 仍偏乐观。
- **RV 顺周期**：已用 `max(EWMA, RV_30)` + 资金 hurdle + IV 分位（默认关闭待样本）补强；分位仍需跑满 1–2 周才可信。
- **集中度**：3 笔都是同一标的的 short put，相关性≈1；累计 Delta + 压力情景只约束线性敞口，不能消除跳空。
- **退出机制有限**：提前赎回已实现（CLI），但 BTC/ETH 币对不支持，且赎回只能拿回申购币种、可能亏损；不自动赎回。
- **平台/对手方风险**：不建模。
- **数据时效**：DCY 30s 轮询、现货 WS；已加 60s 过期暂停，但断线期间仍可能用旧价决策（会自动暂停）。
- **多进程**：TUI 与 Web 同时运行会各持一份 guard；已加数据目录文件锁（见 §8），但仍应避免两个写进程同时下单。

---

## 10. 明确不做

- 不做"按 20pp 年化差"的绝对门槛。
- 不做"Diff 降序、第一个合格就下单"。
- 不做无基准的 DCY。
- 不做跨本位、裸 CALL、杠杆。
- 不做"为了用满额度"的交易。
- 不为了成交而放宽 §7 的验收标准。

---

## 11. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-03 | v1 初稿：DCY-only，保守风控，10–30 天，IV/RV + 相对基准 |
| 2026-11-03 | v1 实现：排序、vol/RV、DecideDCY/BuildDCY、settings/UI；到期差改同日 ≤12h |
| 2026-11-05 | 加机会成本门槛 `yield_too_low`（0.5% 绝对 + 12% 年化） |
| 2026-11-06 | 风控上限与策略资金解耦；live 不再恢复 sim 纸面单 |
| 2026-11-06 | 审计修复：写请求不重试、解码加固、启动参数、纸面 P&L、自动结算、日亏损、过期暂停、去重/竞态、实盘确认 |
| 2026-11-06 | **v2 口径重写**：相对期权改硬门槛；收益门槛改资金口径（CashAPR + 部署比例）；Delta 改组合累计；新增 −30% 压力上限；验收标准与风控口径对齐 |
| 2026-11-06 | 手动下单改为「只提示不拦截」：单笔/总敞口/并发/日笔数、累计 Delta、压力情景越限写入 `Outcome.Warnings` 与审计；自动执行仍硬拒绝 |
| 2026-11-06 | 补齐：opt-summary Delta、EWMA/RV 分位、IV 采样、提前赎回、历史回放（`--replay`）、派息规则核实 |
