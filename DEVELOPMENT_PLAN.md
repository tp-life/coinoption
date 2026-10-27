# OKX 双币赢 vs 期权 APR 对比工具 — 开发计划

> 版本：v1.0（2026-10-27）
> 技术栈：Golang + TUI（Bubble Tea）

---

## 1. 项目背景与目标

OKX 的「双币赢」（Dual Investment，下称 DCY）本质上是**交易所打包的卖出期权策略**：
- **高卖（High Sell）**：持有 BTC，设定目标价，到期价 ≥ 目标价则按目标价卖出 → 等价于 *卖出备兑看涨期权（Covered Call / Short Call）*
- **低买（Low Buy）**：持有 USDT，设定目标价，到期价 ≤ 目标价则按目标价买入 → 等价于 *卖出担保看跌期权（Cash-Secured Put / Short Put）*

而 OKX 同时开放期权交易市场。同一标的、同一到期日、同一行权价上，**直接卖出期权的权利金收益（APR）** 与 **DCY 的报价收益率（APR）** 通常存在差价（DCY 收益一般略低，因为交易所要撮合对冲、且流动性较差，但 DCY 有省心、自动结算的优势）。

**本工具目标**：实时拉取两边行情，在同一"到期日 + 行权价"维度上对齐两者的 APR，给出并可执行决策：

| 条件 | 动作 |
| --- | --- |
| `apr_option − apr_dcy ≥ Δ_high` | **押注期权**（卖出对应期权） |
| `|apr_option − apr_dcy| < Δ_switch` | **投注双币赢**（差距不大，选省心的 DCY） |
| `apr_option − apr_dcy ≤ −Δ_low` | 投注双币赢（期权更差） |

最终形态是一个本地运行的 TUI 工具：只读模式监控对比，可选半自动/全自动下单。

---

## 2. 核心策略逻辑详述

### 2.1 APR 口径定义（关键：两边必须统一到"年化收益率"）

**DCY 侧：**
```
apr_dcy = rate × (365 / D)
```
- `rate`：产品页报出的收益率（到期决算时给收益方的那一份收益）
- `D`：申购时刻距结算日的天数
- OKX 双币赢产品 API 会直接返回 ` apr ` 字段；若无则用上述公式换算

**期权侧（卖出同一行权价 K、同一到期日 T 的期权）：**
```
premium_apr = (markIV 对应的权利金卖价 或 bid价 × 合约规模 × 张数) / 名义本金 × (365 / D)
                 − 预估手续费折算
```
- 本金口径：BTC 期权为 USD（BTC 计价）期权，DCY 为 USDT 计价，**需用指数价格换算成一致口径**（建议统一折算为 USDT 年化，并展示口径说明）
- 权利金卖价优先取盘口 `bid` 中间值，兼顾 `markIV`

### 2.2 产品对齐算法（匹配引擎）

1. 用户新/自动选定标的与到期日范围（如近 3 个到期日）；
2. DCY 侧拉取所有在售产品：`(方向 high_sell/low_buy, strike, settleDate, apr, minAmt)`；
3. 期权侧拉取该标的对应结算日前后 ±1 天到期的期权链：
   - 高卖双币赢 → 匹配 §Short Call（strike = 目标价，或最近行权价）
   - 低买双币赢 → 匹配 §Short Put
4. 行权价完全一致则直接配对；不一致取最近档位，并记录 `strike_error`（行权价偏差），超阈值（如 ±0.5%）标记为"不可对齐"；
5. 计算双向 APR 与差值，按差值排序输出推荐列表。

### 2.3 决策与执行

### 2.3 运行模式（已确认）

| 模式 | 行为 | 状态 |
| --- | --- | --- |
| **推荐（默认）** | 仅展示对比与建议图标（✅ 期权 / 🏦 双币赢 / ⚠️ 不可对齐），不触碰任何下单接口 | 默认 |
| **半自动** | TUI 内选中一行，按 `o` 下期权卖单 / `d` 申购 DCY，弹确认框（二次按键） | 可选 |
| **全自动** | 监控线程触发条件直接下单；需在 Settings 显式开启 + 风控三重上限（见 2.4） | 需显式开启 |

### 2.4 标的、期限与资金风控（已确认）

- **标的清单**：首期 `BTC-USDT`、`ETH-USDT`、`SOL-USDT`（DCY 币对与期权同标的对齐）；配置文件支持任意添加（`config.toml` 中 `underlyings = [...]`，后续可加其他币对，前提是该币种 OKX 同时提供期权与双币赢）。期权侧同时尝试 USD期权 与 USDT期权两条链，优先与 DCY 计价一致的 USDT 期权。
- **到期期限范围**：7–30 天，配置化。
- **资金风控（Executor 层强校验，UI/配置异常也拦得住）**：
  - 总敞口上限：**5000 USDT**
  - 单笔上限：可配（默认 ≤ 2000 USDT）
  - 最大同时持有订单数：可配（默认 5 笔，**支持多笔并行订单**）
  - 单日下单次数：可配（默认 10 次）
  - 任一超限直接拒绝并写审计日志，TUI Positions 页实时展示当前敞口/剩余额度。

### 2.5 模拟模式（Paper Trading，两级）

| 层级 | 对象 | 机制 | 说明 |
| --- | --- | --- | --- |
| **L1：OKX Demo API** | 期权卖单 | 相同 REST 端点 + 请求头 `x-simulated-trading: 1`，WS 连 `wspap.okx.com:8443` | 模拟盘覆盖期权交易，走真实撮合/风控校验，最贴近真实 |
| **L2：本地纸面模拟器** | 双币赢申购 | 本地记录 `{产品, 金额, 申购价, 目标价, 结算日}`，到结算日按当前指数价自行结算并计算收益 | OKX Demo **不覆盖金融产品（DCY）**，只能本地模拟 DCY 结算逻辑 |

- 模式值为 `live` / `sim-okx`（期权 Demo + DCY 本地）/ `sim-local`（全本地，甚至不用 API Key 也能跑，行情走公开接口）
- 所有模拟成交与结算写入 `store` 的独立 `sim_orders` 表，TUI Positions 页用 `[SIM]` 徽标区分
- 切换模式无需重启，事后可一键对账（模拟 vs 当日真实行情重放）

---

## 3. 系统架构

### 3.1 分层架构

```
┌─────────────────────────────────────────────────────┐
│                  TUI 展示层 (ui/)                    │
│  Bubble Tea + Lipgloss + Bubbles(table, viewport)   │
│  Dashboard │ 对比表 │ 持仓 │ 下单面板 │ 设置 │ 日志   │
├─────────────────────────────────────────────────────┤
│                  策略层 (strategy/)                  │
│  Matcher(对齐) │ APR 计算器 │ 决策引擎 │ 风控守卫     │
├─────────────────────────────────────────────────────┤
│                  交易层 (executor/)                  │
│  DCY 申购器 │ 期权下单器 │ 持仓/OEO状态同步            │
├─────────────────────────────────────────────────────┤
│                  数据层 (market / store)             │
│  OKX REST 客户端 │ WS 行情流 │ SQLite 快照/审计存储   │
└─────────────────────────────────────────────────────┘
```

### 3.2 目录结构（规划）

```
coinoption/
├── cmd/coinoption/          # 入口 main.go
├── internal/
│   ├── okx/                 # OKX v5 API 客户端
│   │   ├── client.go        # HMAC-SHA256 签名、限速、重试
│   │   ├── market.go        # 行情：spot tickers, option tickers, opt-summary
│   │   ├── option.go        # 期权 instruments / 公开数据
│   │   ├── dual.go          # 双币赢 products / orders / 申购
│   │   ├── account.go       # 余额、持仓
│   │   └── ws.go            # WS 公共行情订阅（ticker/best-quote/opt 增量）
│   ├── market/              # 行情采集与缓存（内存 + SQLite 落地）
│   ├── strategy/
│   │   ├── matcher.go       # DCY ↔ 期权 对齐
│   │   ├── apr.go           # APR 计算与口径换算
│   │   └── engine.go        # 差值比较 + 决策生成
│   ├── executor/
│   │   ├── executor.go     # 下单入口 + 风控守卫(总敞口5000/单笔/笔数/次数)
│   │   ├── option_order.go # 期权卖单（含模拟路由）
│   │   ├── dual_order.go   # DCY 申购（含模拟路由）
│   │   └── sim.go          # L2 本地纸面模拟器 + 结算逻辑
│   ├── store/               # SQLite (modernc.org/sqlite 纯 Go)
│   ├── config/              # TOML 配置 + viper
│   └── ui/                  # Bubble Tea 页面
│       ├── model.go         # 主 Model, 路由
│       ├── dashboard.go
│       ├── compare.go
│       ├── positions.go
│       ├── orderpanel.go
│       └── settings.go
└── configs/config.example.toml
```

### 3.3 数据流

```
WS 订阅(spot ticker / option best-IV) ─┐
REST 轮询(慢，30~60s)：                 ├─► market.Cache ─► Matcher ─► Engine ─► TUI Render
 · /api/v5/public/instruments(OPTION)  │                 (每 tick / 每 30s 重算)
 · /api/v5/market/tickers(OPTION)      └─► store.SQLite (行情快照 + 决策记录)
 · /api/v5/finance/dual-currency/*     ─► Engine 决策 ─► Executor ─► OKX(私有 API)
```

---

## 4. 使用的 OKX API 清单

| 用途 | 端点 | 权限 |
| --- | --- | --- |
| 现货指数/现价 | `GET /api/v5/market/ticker` | 公开 |
| 期权基础信息 | `GET /api/v5/public/instruments?instType=OPTION&uly=BTC-USD` | 公开 |
| 期权行情（含 bid/ask、隐波） | `GET /api/v5/market/tickers?instType=OPTION` | 公开 |
| 期权定价/隐波总览 | `GET /api/v5/public/opt-summary` | 公开 |
| 期权下行报价 | `GET /api/v5/market/books` | 公开 |
| DCY 在售产品 | `GET /api/v5/finance/dual-currency/products` | 公开/私有混合（以文档为准核对字段） |
| DCY 持仓/订单 | `GET /api/v5/finance/dual-currency/orders` | 私有 |
| DCY 申购 | `POST /api/v5/finance/dual-currency/purchase-order` | 私有 |
| 期权下单（卖方） | `POST /api/v5/trade/order-pack` 或 `/trade/order` (tdMode=cross) | 私有 |
| 余额/持仓 | `GET /api/v5/account/balance`、`/account/positions` | 私有 |
| WS 行情 | `public频道: tickers / option 相关系列` | 公开 |
| WS 私有（持仓推送） | `private频道: account / orders / positions` | 私有 |

> ⚠️ 编码前需一次性核对官方文档（https://www.okx.com/docs-v5/zh ）中"金融产品/双币赢"分节的准确路径与字段名；上端点为初稿，M0 阶段以真实的 openAPI 调用为准。

---

## 5. TUI 设计

- **主题**：简洁深色，Lipgloss 单色主题 + 少量强调色（绿/黄/红），不花哨。
- **页面（数字键 / Tab 切换）**：
  1. `Dashboard` — 当前价格、30 天期隐波、DCY 数学枚排前 5 推荐
  2. `Compare` — 主战场表格：每到期日/行权价一行，列含但不少于
     `结算日 / strike / dcy_dir / dcy_apr / opt_bid_iv / opt_apr(diff) / 建议`
     建议列有色块：`买期权`(绿) `双币赢`(蓝) `不可对齐`(灰)
  3. `Positions` — 双币赢持仓 + 期权空仓 + 浮动收益预估
  4. `Order` — 下单弹层（金额、确认二次按键）
  5. `Settings` — 阈值、标的清单、模式切换、金额上限
  6. `Logs` — 决策日志 & API 错误滚动窗口

---

## 6. 里程碑与排期（建议）

| 阶段 | 内容 | 产出 | 预估 |
| --- | --- | --- | --- |
| **M0 骨架** | Go 工程初始化、OKX REST 客户端（签名、限速、重试）、公共行情打通、config | `curl 命令在 Go 里跑通期权/DCY 产品列表` | 2–3 天 |
| **M1 对齐引擎** | Matcher + APR 两口径计算 + 决策枚举 + 纯 Go 单测（真实行情快照做黄金用例） | `go test ./strategy` 全绿，可命令行 `--dump` 出 CSV 对比 | 3–4 天 |
| **M2 TUI v1（只读）** | Bubble Tea 全部只读页面 + WS 行情注入 | 使用者可在 TUI 内看推荐但不下单 | 4–5 天 |
| **M3 交易执行** | 私有 API 登录、余额校验、DCY 申购、期权卖单（tdMode=cross）、持仓同步、互动确认、模拟模式（L1 Demo API + L2 本地 DCY 模拟器）、多笔并发的仓位管理器与风控守卫（**总敞口 5000 USDT 卡死**） | 模拟盘下期权 Demo 单 + 本地模拟 DCY 单可行，本地跑全模拟无需 API Key 也能观察 | 5–6 天 |
| **M4 稳定化** | 审计日志、异常恢复（断线重连、限速退避）、小实盘验证（金额上限）、可选通知 | 稳定降级运行 | 3–4 天 |

约 3–4 周的紧凑计划，可按实际节奏伸缩。

---

## 7. 风险与注意事项

1. **口径错配**：BTC 期权为 USD 计价（BTC settled），DCY 为 USDT 计价。统一换算时必须说明，避免"赚的是币本位"误会。
2. **行权价缺失**：DCY 的目标价不一定是期权链的标准行权价（如 $57,700），需启用最近档位 + 偏差标记。
3. **限速**：OKX 公开接口 20 req/2s 级别；务必用 WS 增量 + 轮询降频。
4. **模拟盘限制**：OKX 模拟盘（Demo Trading）覆盖现货/合约/期权等交易产品，但**不覆盖金融产品（双币赢）**。因此模拟采用两级方案（见 2.5）：期权走 Demo 真实验证，DCY 由本地按真实行情重放结算，逻辑一致性再做单测覆盖。
5. **风控优先**：任何自动化默认关闭，或需在 Settings 中主动开启 + 金额/次数双上限；最大敞口校验放在 Executor 而非 UI。
6. **时间对齐**：两边 APR 的 `D`（距结算天数）必须一致；DCY 结算时间与期权行权交割时间可能有数小时差，统一取期权到期时间。

---

## 8. 参数确认结果（经讨论定稿）

| 项 | 决定 |
| --- | --- |
| 自动化程度 | **默认仅推荐**，Settings 中可开启半自动/全自动 |
| 标的 | BTC-USDT、ETH-USDT、SOL-USDT，配置文件可添加任意其他币对 |
| 到期期限 | **7–30 天**，配置化 |
| 资金上限 | **总敞口 5000 USDT**；默认单笔 ≤ 2000、最大并行 5 笔、单日 ≤ 10 次（均可配置，Executor 层强制校验） |
| 多笔订单 | **支持并行多笔**，Positions 页实时显示敞口占用/剩余额度 |
| 模拟模式 | 三种运行模式：`live` / `sim-okx`（期权走 OKX Demo API + DCY 本地模拟）/ `sim-local`（全本地，无需 API Key） |
