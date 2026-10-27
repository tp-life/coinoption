# 架构设计文档 — coinoption

> 配套文档：`DEVELOPMENT_PLAN.md`（本档定义代码分层、模块接口与依赖工具清单）

---

## 1. 依赖工具清单（Go 三方库）

| 依赖 | 版本基线 | 用途 | 选型理由 |
| --- | --- | --- | --- |
| `charmbracelet/bubbletea` | v1.x | TUI 主框架（Elm 架构 Model/Update/View） | TUI 事实标准，事件驱动，WS 推送可无缝映射为 `tea.Msg` |
| `charmbracelet/lipgloss` | v1.x | 样式/布局/配色 | 与 bubbletea 同族，组件风格统一 |
| `charmbracelet/bubbles` | v1.x | table / viewport / textinput / spinner / keys | 现成的对比表、滚动日志、设置输入框 |
| `gorilla/websocket` | v1.5 | OKX WS 行情与私有频道 | 稳定、社区标准 |
| `modernc.org/sqlite` | 最新 | SQLite 驱动 | **纯 Go 无 CGO**，macOS/Linux/ARM 交叉编译零负担 |
| `spf13/viper` + TOML | v1.x | 配置管理 | 支持配置热更新（阈值/标的/模式切换） |
| `zerolog` | v1.x | 结构化日志 | 零分配、低成本，日志写文件 + TUI 环形缓冲双路 |
| `golang.org/x/time/rate` | 最新 | 客户端限速器 | OKX 有严格限速（20 req/2s 级） |
| `stretchr/testify` | v1.x | 测试断言 | 黄金用例（行情快照）做匹配/APR 回归测试 |
| `golang.org/x/net/proxy`（可选） | — | SOCKS5 代理 | 国内访问 OKX Debug 增强 |

标准库即可覆盖：`crypto/hmac` + `crypto/sha256`（OKX 签名）、`time`、`sync`、`context`、`encoding/json`、`sort`、`strconv`。

---

## 2. 代码架构总览

```
coinoption/
├── cmd/coinoption/
│   └── main.go                    # 入口：读配置 → 组装依赖 → 启动 TUI / --dump 直通模式
│
├── internal/
│   ├── okx/                       # ── OKX v5 API 客户端层（纯 SDK，无业务逻辑）
│   │   ├── client.go              #   REST 基座：HMAC-SHA256 签名、限速、超时、重试
│   │   ├── types.go               #   统一响应结构 {code,msg,data}，错误码转 error
│   │   ├── market.go              #   公开行情：spot ticker、option tickers、opt-summary、books
│   │   ├── instruments.go         #   /public/instruments（期权链、花式行权价/到期档）
│   │   ├── dual.go                #   双币赢：products（在售）/ orders（持仓）/ purchase-order
│   │   ├── account.go             #   私有：balance、positions
│   │   ├── trade.go               #   期权下单 /trade/order（卖方 tdMode=cross）
│   │   ├── ws.go                  #   WS 公共频道封装：订阅、心跳、断线重连
│   │   └── wsdemo.go              #   Demo 环境切换（REST 加 x-simulated-trading 头、WS 换 wspap host）
│   │
│   ├── market/                    # ── 行情采集与缓存层（策略层的唯一数据源）
│   │   ├── cache.go               #   内存缓存：线程安全 snapshots，带时间戳/新鲜度标记
│   │   ├── collector.go           #   后台 goroutine 组：慢轮询(REST 30-60s) + 快订阅(WS)
│   │   └── exchange_rate.go       #   USD/BTC 币本位 → USDT 口径换算缓存
│   │
│   ├── strategy/                  # ── 策略层（无 IO，全部纯函数，可黄金单测）
│   │   ├── types.go               #   对齐条目 MatchRow、决策 Decision、阈值参数
│   │   ├── apr.go                 #   APR 计算：DCY 口径 / 期权口（权利金/本金×天数年化）
│   │   ├── matcher.go             #   DCY 产品 ↔ 期权链 对齐（到期日±窗口、最近行权价、偏差标记）
│   │   ├── engine.go              #   构建 MatchRow 全集 → 计算 diff → 决策（期权/双币赢/不可对齐）
│   │   └── engine_test.go         #   【黄金用例】用真实行情快照 JSON 回放，APR/对齐/决策断言
│   │
│   ├── executor/                  # ── 执行层（唯一被允许下单的模块）
│   │   ├── guard.go               #   风控守卫：总敞口5000/单笔≤2000/≤5笔并行/日≤10次 强校验
│   │   ├── executor.go            #   Decision → 下单请求；sim/live 路由
│   │   ├── option_order.go        #   期权卖单封装（live → trade.go / sim-okx → Demo 头同端点）
│   │   ├── dual_order.go          #   DCY 申购封装（live → dual.go / sim → sim.go）
│   │   ├── sim.go                 #   L2 本地纸面模拟器：sim_orders 表 + 到期按指数价结算引擎
│   │   └── positions.go           #   统一持仓视图：合并真实 DCY 持仓、期权空头、模拟单（[SIM] 徽标）
│   │
│   ├── store/                     # ── 存储层（SQLite）
│   │   ├── store.go               #   初始化、schema 迁移
│   │   ├── schema.sql             #   表：market_snapshots、decisions、orders、sim_orders、audit_log
│   │   └── audit.go               #   决策/下单审计写入（全自动模式的关键留痕）
│   │
│   ├── config/                    # ── 配置层
│   │   ├── config.go              #   Config 结构体 + 默认值 + 校验（含危险项显式确认）
│   │   └── watch.go               #   viper 热更新：阈值/标的/模式变更广播到运行中的模块
│   │
│   └── ui/                        # ── TUI 层（Bubble Tea）
│       ├── app.go                 #   根 Model：页面路由、全局键位（1-6/Tab/q）、消息分发
│       ├── msg.go                 #   tea.Msg 定义：行情刷新、决策更新、持仓变更、模式切换
│       ├── theme.go               #   Lipgloss 配色/样式集中管理
│       ├── dashboard.go           #   页1：现价、IV 总览、Top5 推荐
│       ├── compare.go             #   页2：主对比表（DCY APR vs 期权 APR vs diff + 建议色块）
│       ├── positions.go           #   页3：持仓、敞口占用/剩余额度、[SIM] 徽标
│       ├── orderpanel.go          #   页4：单双方下单弹层 + 二次确认（o 期权 / d 双币赢）
│       ├── settings.go            #   页5：模式切换(live/sim-okx/sim-local)、半/全自动开关、阈值
│       └── logs.go                #   页6：滚动日志 + API 错误高亮
│
├── configs/
│   ├── config.example.toml        # 配置样例（underlyings、期限 7-30、阈值、sim 模式、风控参数）
│   └── snapshotBTCETH/            # 黄金测试用例的行情快照 JSON
├── DEVELOPMENT_PLAN.md
├── ARCHITECTURE.md
├── go.mod
└── Makefile                       # build / test / vet / lint / snapshot（抓行情快照生成用例）
```

---

## 3. 分层依赖规则（从上到下单向依赖）

```
ui ──► strategy ◄── market ──► okx
 │          │                   ▲
 ├──► executor ──────────────────┤
 └──► config                     │
      store ◄──── executor / market / ui(日志)
```

铁律：
1. **strategy 是纯函数层**——只读 market 缓存快照，返回决策列表；不做任何网络与存储操作 → 保证黄金单测可跑、可重放。
2. **只有 executor 能下单**——UI 不直接持有 OKX trade 客户端；风控守卫 `guard.go` 在 executor 入口强制校验。
3. **okx 层不知道业务**——只做 API 封装与限速，DCY/期权语义不进入该层。
4. **sim 不是一个模式分支散落各处，而是一个显式路由件**——`executor.go` 依据当前模式把同一决策路由到 `live / Demo / sim` 三条路径。

---

## 4. 关键接口定义（契约）

```go
// strategy.Decision —— 策略层与执行层之间的唯一契约
type Decision struct {
    Row          MatchRow   // 对齐结果（含 dcy/apr/期权/AI字段）
    Action       Action     // BuyOption | SubscribeDual | None | Unaligned
    ReasonCode   string     // "option_apr_higher" | "diff_small" | "strike_gap_too_big" ...
    SuggestedAmt float64    // 依据单笔上限计算的估算金额
    Ts           time.Time
}

// executor 由 Decision 驱动
type OrderOutcome interface{ ... }   // live单 / demo单 / sim单 的统一回执

// market.Cache —— 策略层唯一读入
type Cache interface {
    OptionChain(uly string, from, to time.Time) []OptionQuote
    DualProducts(uly string) []DualProduct
    Spot(uly string) SpotTicker
    Freshness() map[string]time.Time
}
```

---

## 5. 并发与数据流模型

```
[collector: WS goroutine]  ──msg──►  [cache(互斥锁)]  ──快照──►  [engine 每 N 秒或 WS 触发重算]
        │                                                                  │
[collector: REST 慢轮询 goroutine×(spot/options/dual)]                     ▼
        │                                                        Result → bus(ch) ─┐
[executor goroutine] ◄── 下单请求(cfg 上限) ── ui/cmd ─────────────────────────┘
        │  审计写 store
[bubbletea 主循环] ◄──── tea.Msg（行情刷新/决策/持仓/错误）──┘
```

- 所有跨 goroutine 通信走带缓冲 channel → `tea.Msg`，无共享可变状态（除 cache 内部锁）；
- WS 断线重连 + 指数退避由 `okx/ws.go` 内封装；限速器挂在 `okx/client.go` 出口；
- sim-local 模式下不启动私有 collector，DCY 结算由 executor 内的 `sim.go` 依据缓存行情定时结算。

---

## 6. 错误与降级策略

| 故障 | 行为 |
| --- | --- |
| WS 断流 | 指数退避重连；期间 cache 数据标记 stale（UI 顶部黄条提示"数据不新鲜"） |
| REST 限速(429/50011) | 限速冷却 + 单次重试，仍失败则跳过本轮轮询 |
| 下单失败(风控上限) | 拒绝并写 audit_log，TUI 红条提示原因码 |
| 配置错误(如资金上限为负) | 启动即 fail-fast |
| 模拟与实盘切换 | 切换需在 Settings 二次确认；sim 单与真实单物理隔离（不同表） |
