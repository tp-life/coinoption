# coinoption — OKX 双币赢 vs 期权 APR 对比工具

Go + TUI 开发中。文档：`DEVELOPMENT_PLAN.md`（计划）、`ARCHITECTURE.md`(架构)。

## 当前进度

- **M0 完成**：OKX REST 客户端（签名/限速/重试/代理）✅ 公开行情实测 ✅ 双币赢 `sfp/dcd` 全端点 ✅
- **M1 完成**：strategy 对齐引擎（matcher/APR/决策）+ 黄金用例 + 实线校准（`|strike gap|` 匹配、UM=USD 计价）✅
- **M2 v1 完成**：Bubble Tea TUI Dashboard（WS 实时行情，1s 从本地缓存重绘；`r` 手动刷新，`q` 退出）✅
- **M3 v1 完成**：executor 执行层 ✅ —— 风控守卫（总敞口 5000 / 单笔 2000 / 并行 5 / 日 10 强校验 + 重启恢复）、DCY 申购两步流（quote→trade）、期权卖单、三种模式路由（live / sim-okx / sim-local）、本地 DCY 纸面模拟结算（[SIM] 单落盘、到期按现价结算）、统一持仓视图、JSONL 审计留痕（`~/.coinoption/`）
- **M3.2 完成**：TUI 下单与持仓 ✅ —— 决策行上 `o` 卖期权 / `d` 申购双币赢，数量输入 → 二次确认 → 提交（`y`/`n`），`p` 持仓页（异步刷新缓存 + `j/k` 滚动），头部常驻模式徽标与敞口/在持进度；live 模式启动时从平台 DCY 在持单 + 期权空头恢复风控敞口
- **M3.3 完成**：Settings 页 + 自动执行（默认关闭）✅ —— 阈值/开关落盘 `settings.json`，自动执行每次刷新最多 1 单、同产品跨重启只做一次、间隔 ≥60s，仍受 executor 风控硬上限约束；live 模式开启需当次 `y` 二次确认，进程重启强制回到关闭
- **M4 v1 完成**：OKX WebSocket 实时行情 ✅ —— 现货 + 全部期权 `tickers` 订阅（每批 300 条），心跳、指数退避重连、断线自动降级 30s REST；`market.Live` 缓存每 500ms 重组一次，TUI 1s 重绘；Web 新增 `GET /ws` 每秒推送当前筛选快照，浏览器断线自动重连并回退 REST
- **M4 扩展**：Web 看板双标签（对比决策 / 完整期权链）✅；Web 交易默认开启（`--read-only` 可关）+ 页面自动交易开关 ✅ —— 决策行内「卖出期权 / 申购 DCY」弹窗二次确认，复用 executor 风控/审计/模式路由，live 模式启动打印实盘警告；**Web 看板按「Approva」brutalist dashboard 重做三栏 UI**（侧栏 / 列表卡片 / 策略单据 + 动作栏），见下方「Web 看板视觉与交互」
- **M5 完成**：Web 设置页 + 整站授权码 ✅ —— 新增**资金总额**（作为总敞口上限，单笔按 40% 派生）、**差值比例**（自动交易最低差%）、**自动交易开关**（落盘 `settings.json`，重启沿用；live 启动仍强制关闭）与策略阈值热更新；**整站授权码**（`--web-code` / `COINOPTION_WEB_CODE`，未指定则首次随机生成并打印，settings.json 只存 SHA-256 摘要），未登录只能看到登录页，下单 / 自动开关 / 改设置全部要求登录，登录失败按 IP 限速 5 次/分钟，改码即注销所有会话

### TUI 快捷键

| 键 | 作用 |
| --- | --- |
| `1` / `3` / `p` | 决策表 / 持仓页切换 |
| `2` | 完整期权链（参考视图，不直接下单；`1` 返回决策页） |
| `4` / `l` | 审计日志页（submit/reject/settle/restore，最新 200 条） |
| `5` / `s` | Settings 页（自动执行开关、阈值调整、资金总额 / 自动交易差% / 风控只读展示、最近 10 条自动失败记录；资金与差%在 Web 设置页修改） |
| `a` | Settings 页内切换自动执行（live 需 `y` 二次确认） |
| `[` / `]` | Settings 页内期权边际阈值 ±5pp（下次刷新生效） |
| `o` | 对选中的「卖CALL / 卖PUT」行卖出该期权（输入张数 → 二次确认） |
| `d` | 对选中的 DCY 行申购（输入投资币种金额 → 二次确认）；「不可对齐」行也可手动申购（标注 `未对齐·手动`） |
| `y` / `n` / `Esc` | 确认 / 取消 |
| `j` / `k` | 持仓页滚动 |
| `r` / `q` | 刷新 / 退出 |
| `u` | 弹窗选择标的（BTC / ETH / SOL）：`↑/↓` 或 `j/k` 移动，`Enter` 确认，`Esc` 取消 |
| `e` | 弹窗选择到期日：`全部（不过滤）` / 快照中每个可用到期日（含行数） / `手动输入日期…` |
| `t` | 弹窗选择方向：`全部方向` / `CALL 看涨（高卖）` / `PUT 看跌（低买）`，含当前组合行数 |
| `b` | 弹窗选择结算本位：`全部本位` / `币本位（BTC-USD 链）` / `U本位（BTC-USD_UM 链）`，含当前组合行数 |
| `f` | 弹窗选择排序：`默认顺序` / `价格 高→低/低→高` / `期权APR 高→低/低→高` / `差% 高→低/低→高`（决策表与期权链表共用，详见下方「排序」） |
| `Esc` | 关闭弹窗；无弹窗时清除全部筛选（方向 + 本位 + 到期），恢复全量 |

> 当前组合：`标的 + 方向 + 本位 + 到期` 四个维度可自由组合，表头上方的「组合」条实时显示（如 `BTC-USDT · PUT · 币本位 · 2026-11-06`），并给出各维度快捷项与行数；`t` 选方向、`b` 选本位、`e` 选到期、`u` 切标的、`f` 改排序，`Esc` 一键清空筛选。四个筛选同时作用于决策行和期权榜。
>
> **方向 / 本位 / 排序会记忆**：`t` / `b` 的选择与 `f` 的排序键即时写入 `settings.json`（`sideFilter` / `settleFilter` / `sortBy`，如 `"sortBy": "price:desc"`），退出后下次启动自动恢复（`Esc` 清除筛选也会同步落盘，但不清排序）。到期筛选不记忆。
>
> **排序**（`f`）：`价格` = 期权买价折算 USDT（币本位报价 × 现价，否则 0.0038 与 290 这类同经济价值的两个链会被拆到表格两端）；无买盘的档位用最新价兜底；`期权APR` / `差%` 以期权侧为准；没有可比值的行（无买盘算不出 APR/差%、期权链里没有对应 DCY）**无论升序降序都沉底**，并列行保持默认顺序。按价格/APR 实时排序时行会随报价上下移动，光标会跟随**同一产品/合约**（按 `productId` / `instId` 定位），不会因为行号偏移而选中另一条产品。

> 到期时间筛选：按 `e` 弹出到期日选择框（`全部` / 各可用到期日 + 行数 / `手动输入日期…`，手动模式支持 RFC3339 或 `2026-11-06`）。选定后按「到期日（UTC）」过滤决策/期权榜；表格内展示日期，详情行展示完整 `YYYY-MM-DD HH:mm`（UTC）。

> 主表列宽随终端宽度自适应（120 列下不裁切）：操作 / 产品·合约 / 到期(UTC) / 方向 / 本位 / 行权价 / **距现价%** / **买价** / DCY APR / 期权APR / 差% / 天 / 建议。**决策页（`1`）每条 DCY 产品一行**，期权 APR / 差 来自对齐到的同档合约；**期权链页（`2`）单独展示完整期权链**（操作 / 期权合约 / 到期 / 方向 / 本位 / 行权价 / 距现价% / 买价 / 期权APR / DCY APR / 差% / 天），有对应 DCY 的合约在合约名后标 `←DCY`，详情行给出对应产品与差值。`本位` 列区分期权腿结算：**币本位**（`BTC-USD`/`ETH-USD` 链，结算币为标的币）与 **U本位**（`BTC-USD_UM` 链，USDT 结算）。`距现价%` =（行权价 − 现价）/ 现价，正=高于现价、负=低于现价；`买价` 是该档权利金买一价（币本位计 BTC、U本位计 USDT）。到期列只显示 UTC 日期（OKX 期权统一 08:00 结算）；选中行下方详情行展示完整产品与期权合约、交易方向、本位、到期时间、行权价、现价与距现价绝对值/百分比、买价/卖价、偏差、原因与建议金额。

### 方向与买卖语义（CALL / PUT）

| 列 | 取值 | 含义 |
| --- | --- | --- |
| 方向 | `CALL` | 看涨（原 `C`）；期权侧为卖 Call，DCY 侧为「高卖」 |
| 方向 | `PUT` | 看跌（原 `P`）；期权侧为卖 Put，DCY 侧为「低买」 |
| 操作 | `卖CALL` / `卖PUT` | **SELL**：卖出该期权（开空、收权利金），按 `o` 下单 |
| 操作 | `申购高卖` / `申购低买` | **申购**双币赢，按 `d` 下单；高卖＝到期按行权价**卖币**（投入 BTC/ETH），低买＝到期按行权价**买币**（投入 USDT） |
| 操作 | `卖出期权` / `仅报价` | 期权榜行：`卖出期权`=有买盘且卖方方向有效；`仅报价`=方向有效但暂无买盘（算不出卖方 APR）。实值档已从期权榜过滤 |

> 本工具没有「买入期权」（BUY option）路径：`buy_option` 是策略名，实际执行的是卖出期权；表中统一显示为「卖…」/`卖出期权` 避免误解。选定行的详情行会用一句话写明，例如 `卖出CALL（开空收权利金）` 或 `申购双币赢·低买`。
>
> `高卖 / 低买` 里的「卖 / 买」指到期时**换币方向**（卖币 / 买币），不是期权买卖；期权的买卖看「操作」列：`卖CALL / 卖PUT / 卖出期权` 都是 SELL。

刷新失败时保留上一次成功快照并在头部标红提示（不会清空决策表）；持仓页与日志页在可见时随 1s 心跳自动刷新（WS 缓存；`--ws=false` 时 REST 15s 缓存）。

**自动执行边界**（`settings.json` 的 `autoTrade`）：

- 默认关闭；关闭时 TUI 只给推荐，不自动下单。
- 开启后：每轮刷新最多 1 单、同一产品/合约只自动做一次（启动时用现有持仓/在持单播种去重；**已结算**的同一产品重启后可能再次自动执行，会话内则不会）、最小间隔 60s。
- 自动执行使用**未过滤快照**，在可执行候选里按差% 降序挑最优；到期/本位/搜索等展示筛选不影响自动交易范围。**方向（PUT/CALL）例外**：沿用你持久化的方向偏好，避免自动执行跑到不想要的方向。
- 期权张数 = `floor(min(单笔上限, 剩余敞口) / (现价 × ctVal × ctMult))`，再封顶 20 张。OKX BTC 期权 **1 张 = 0.01 BTC**、ETH = 0.1 ETH；默认 2000 USDT 单笔可卖 2 张 BTC 期权。
- 期权结算本位与投资币种不一致（如 USDT 现金 vs 币本位期权）的候选**自动跳过**：APR 可比但抵押品/强平风险不同，需人工确认。
- **live 模式**：开启需当次 `y` 确认，进程重启后强制重置为关闭（防止静默连续下单）；`sim-okx` / `sim-local` 沿用落盘开关。
- 只有成功回执才写入去重表并计入次数；提交失败不占坑，冷却 60s 后允许重试。
- **自动失败记录**：自动提交失败（风控拒绝 / 行情变动导致 APR 不达标 / API 报错）与行情拉取失败都会记下时间、标的、原因和当时的候选摘要，**只保留最近 10 条**。记录落盘 `auto_failures.jsonl`，Web 侧栏 `FAIL LOG › 失败 N` 或 TUI Settings 页随时查看，进程重启后仍在；相同原因（10 分钟内）合并为一条并标 `×N`，行情持续失败 5 分钟内只记一次，避免刷屏。
- Web 端同一套规则：auto 引擎每秒评估一次，`sim` 沿用落盘开关（默认关闭）、`live` 启动强制暂停，页面 `AUTO TRADE` 开关实时切换（`POST /api/auto`）；开关会写入 `settings.json`，重启后沿用（live 除外）。
- 所有自动单与手动单走同一条 executor 路径，风控硬上限、产品规模区间、审计留痕完全一致。

> 下单键在未接入 executor（只读推荐）或选错行类型时会给出错误提示，不会发出请求；风控拒单原因（敞口/单笔/并行/日限）会以红字回显。
>
> 注意：期权单张名义 = 0.01 BTC（约 760 USDT），默认单笔上限 2000 USDT 可卖 2 张；需要更大仓位时再按风险承受能力放宽 `--limits`。live 启动恢复覆盖「平台 DCY 在持单 + 期权空头敞口」，**当日笔数**仍取自本地 audit（平台历史成交未计入，防重复提交靠本地记录）。



### M3 CLI（执行通道）

```bash
# 纸面申购（sim-local 默认；无 Key 时产品参数由 --strike/--abs-yield/--exp-time 合成）
./bin/coinoption --order dcd --product BTC-USDT-261106-80000-C --amt 0.02 \
  --strike 80000 --abs-yield 0.008 --exp-time 2026-11-06T08:00:00Z
# 有 Key 则直接线上取产品（但注意 IP 白名单：50106/50110 = 出口 IP 未入白名单）
# 纸面期权卖单（限价=0 时按当前 bid 追卖）
./bin/coinoption --order opt --inst BTC-USD_UM-261106-83000-C --sz 2 --px 0
# 模式与风控
./bin/coinoption --order dcd ... --mode sim-okx      # 期权走 OKX Demo（需 Demo Key），DCY 本地纸面
./bin/coinoption --positions                         # 持仓：风控快照 + [SIM] 纸面单 + 平台单
./bin/coinoption --settle                            # 到期纸面单按现价结算并释放敞口
./bin/coinoption --guard                             # 只看风控守卫
# 1 张 BTC 期权 = 0.01 BTC ≈ 760 USDT，默认 2000 单笔即可；大仓位再放宽：
./bin/coinoption --order opt --inst BTC-USD_UM-261106-83000-C --sz 2 \
  --limits 500000,160000,5,10
```

### 配置文件、凭证与数据目录

- **界面 / 策略配置**：`~/.coinoption/settings.json`（默认目录，`--store <dir>` 可改）。当前字段：
  - `minOptionEdgePp`：期权须高出 DCY 的百分点（默认 20）
  - `minAbsEdgePct`：单笔绝对收益差下限（%，默认 0.5）；7 天 20pp 只有 0.38% 实际收益，短期限靠年化阈值会放行噪声
  - `maxStrikeGapPct`：行权价相对现价的最大偏差 %（默认 0.5）
  - `capitalUsdt`：资金总额（USDT，默认 5000）——风控总敞口上限，单笔上限按 40% 派生（默认 5000 → 单笔 2000）；启动时 `--limits` 优先，Web 设置页保存后在运行期以页面保存值为准
  - `minDiffPct`：自动交易最低差 %（默认 0 = 不设门槛）——`|期权APR − DCY APR|` 低于该值时自动执行跳过；只影响自动执行，不影响推荐动作
  - `autoTrade`：自动执行开关（默认 `false`；Web 设置页 / TUI Settings 页切换后落盘；live 启动强制关闭）
  - `webAuthCodeHash`：Web 授权码的 SHA-256 摘要（64 位 hex，不落明文）；遗忘时用 `--web-code` 重置，或删除该字段后重启自动生成
  - `sideFilter` / `settleFilter`：TUI 的方向 / 本位筛选记忆
  - `sortBy`：TUI 排序记忆（如 `"price:desc"`）
- **API 凭证不在配置文件里**：用 `--key / --secret / --passphrase`，或环境变量 `COINOPTION_API_KEY` / `COINOPTION_SECRET` / `COINOPTION_PASSPHRASE`；代理用 `--proxy` 或 `COINOPTION_PROXY`。
- **数据文件**（同目录）：`sim_orders.jsonl`（纸面单）/ `orders.jsonl`（真实或 Demo 回执）/ `audit_log.jsonl`（审计）/ `auto_failures.jsonl`（自动失败记录，最近 10 条）/ `settings.json`，可直接 `cat` 对账。
- **Web 端界面偏好**：标的 / 方向 / 排序存在浏览器 `localStorage`（键 `coinoption.prefs.v1`），不在上述文件里。
- **交易开关**：默认启动即交易——`--web` + 默认 `sim-local` 为纸面单；自动交易默认关闭，可在 Web 设置页 / TUI Settings 页开启并落盘，live 模式启动强制暂停（页面当次确认才能开）。`--read-only` 关闭全部下单与自动交易，只看板。

### 实测结论（M3 新增）
- **期权合约名义**：1 张 = `ctVal × ctMult`，BTC 币本位（BTC-USD）与 UM 链均为 **0.01 BTC（约 760 USDT）**、ETH 为 0.1 ETH。之前误把 `ctVal=1` 当名义，导致敞口/权利金/纸面结算放大 100 倍、默认 2000 USDT 拒掉所有期权单；已修正为由 `ctVal × ctMult` 计算，默认 2000 可卖 2 张 BTC 期权
- **instruments 端点不认 `BTC-USD_UM`**（51014）：UM 合约挂在父链 `BTC-USD` 响应内，代码已自动降级查询
- 行权价字段实测为 `stk`（非 `optStrike`）；UM 期权到期 16:00 UTC（币本位 08:00），±1 天窗口已兼容

### 实测结论（M0–M2 保留）
- **模拟盘（Demo）**：现货/期权数据可用（期权 1026 合约），但**双币赢接口在 Demo 环境返回 500**——金融产品不在模拟盘范围，与文档预期一致
- **双币赢 API（实盘）需要 API Key（只需读取权限）**；期权/现货公开数据无需 Key
- 无 Key 时 TUI 显示「期权机会榜」（按到期日 + 行权价升序，整条链完整展示），接入 Key 后呈现完整 DCY↔期权 对照决策

### 网络重试与限速（M4）

- 网络错误 / HTTP 429 / 5xx：指数退避（400ms 起，±25% 抖动；429 优先用 `Retry-After`），最多 2 次重试。
- 业务码 `50011`（限速，请求未被处理）：任何方法都重试。
- 只读 GET 的瞬时故障（`50001/50026/50013/50004/50005`）可重试；**POST 一律不重试**，避免「已成交又重下一单」。
- 签名/参数/白名单类错误（`50105/50106/50110` 等）立即失败并给出中文提示，不浪费重试。
- **双币赢（`/api/v5/finance/sfp/dcd/*`）单独节流**：官方限速 1 req/s（User ID 级），由 `okx.Client` 按路径统一保证（默认 1.1s，重试也算在内），调用方不再各自 `sleep`——此前 CLI `--dump/--smoke` 漏写间隔会稳定撞 `50011`。
- 代理：REST 与 WS 都遵循 `--proxy`；未显式指定时统一跟随 `HTTPS_PROXY / HTTP_PROXY / ALL_PROXY` 环境变量（此前 WS 的 Dialer 不读环境变量，只配环境变量代理时会出现「行情正常、WS 一直重连中」）。

### WebSocket 实时行情（M4）

- 公共行情走 OKX WS `wss://ws.okx.com/ws/v5/public`（Demo 为 `wss://wspap.okx.com/ws/v5/public?brokerId=9999`），默认开启；`--ws=false` 回退 REST。
- 订阅内容：现货 `BTC-USDT` + 当前标的全部期权 `tickers`（每张合约一个 `instId`，按 300 个一批发送，避开 64KB 单帧上限）。OKX 的 `tickers` 不支持按 `instFamily` 批量订阅期权；`opt-summary` 只有 IV/Greeks 没有 `bidPx`，无法复用现有卖方 APR 公式，因此没有采用。
- 连接每 20s 发送 `ping`，60s 无数据视为断链；断链后指数退避（1s → 2s → … → 30s）重连并自动重放全部订阅。
- 缓存与重算：WS 只更新 `bid/ask/last` 等动态字段；`market.Live` 每 500ms 最多重组一次策略结果。DCY 产品与 `instruments` 仍走 REST（DCY 30s、产品信息 5min），不占用 WS。
- DCY 刷新按方向（C/P）独立合并：某个方向失败时沿用该方向上次数据并在 `warn` 说明，不会因为另一半成功就把失败方向的产品整批清空；首轮失败后 5s/10s/20s 快速重试，再回到 30s 节奏。
- 降级：WS 断开超过缓存周期后，后台每 30s 用 REST 拉一次现货/期权行情兜底；恢复后自动切回 WS。TUI 头部显示 `WS 实时 / WS 重连中 / WS 降级`，Web 页头额外显示 `REST`（未启用或不可恢复的 WS 配置错误）；WS 断开时 `warn` 会带上具体的拨号/握手错误，便于判断是代理没生效还是服务端拒绝。
- 期权榜的「幽灵合约」判据为 `bid / ask / mark / last` **全空**才丢弃：`/market/tickers` 对期权从不返回 `markPx`，若只判 `bid/ask/mark`，会把「无买卖盘但当天成交过」的档位（典型如 `BTC-USD_UM-*` 冷门行权价）整行丢掉，而 OKX APP 里仍能看到这些价格。仅 last 的档位以「仅报价」展示，不产生卖方 APR。
- Web 端 `GET /ws` 每秒推送当前筛选下的完整快照（与 `/api/decisions` 同构）；页面优先用 WebSocket，断线自动重连，重连期间用 REST 兜底。



```bash
./bin/coinoption --web --addr 0.0.0.0:8080
# 默认即开交易（sim-local 纸面单）；首次启动会打印随机 Web 授权码
./bin/coinoption --web --mode sim-local --addr 127.0.0.1:8080

# 显式指定 Web 授权码（也支持环境变量 COINOPTION_WEB_CODE）
./bin/coinoption --web --mode sim-local --addr 127.0.0.1:8080 --web-code 'my-code'

# 只读看板（不装配执行器、不自动交易，仍需授权码）
./bin/coinoption --web --read-only --addr 127.0.0.1:8080
```

> **整站授权**：除 `GET /login` 与 `POST /api/login` 外，所有页面 / API / `/ws` 都要求登录 Cookie。命令行调试先登录再带上 Cookie：
>
> ```bash
> curl -s -c /tmp/ck -H 'Content-Type: application/json' \
>   -d '{"code":"my-code"}' http://127.0.0.1:8080/api/login
> curl -s -b /tmp/ck http://127.0.0.1:8080/api/decisions
> ```
>
> 登录 Cookie 默认 12h；服务重启会清空内存会话，需要重新输入授权码；改授权码（设置页）会注销所有设备。授权码与 Cookie 在 HTTP 下是明文传输，**仅在 127.0.0.1 使用**；若确实要跨机访问，请放在 HTTPS 反向代理（Caddy / Nginx）之后，并保持 `--addr 127.0.0.1:8080` 只监听本机。

- `GET /login` — 授权码登录页（`POST /api/login {"code":"..."}` 换取会话 Cookie；`POST /api/logout` 退出）
- `GET /` — 决策看板页（内嵌 HTML，优先通过 `/ws` 接收每秒推送；WS 不可用时自动回退 `/api/decisions`）
- `GET /ws` — WebSocket 推送（查询参数与 `/api/decisions` 相同：`uly / exp / side / settle`），每秒一帧完整快照；断线由浏览器自动重连
- `GET /api/decisions` — 当前决策 JSON（camelCase：`at / uly / optionUly / spotPrice / optionCount / dcdCount / decisions / optionRows / products / dcyAvailable / warn / wsConnected / wsStatus / mode / tradeEnabled / guard`）；`decisions[].row` 含 `productId / expTime / dcyApr / optApr / diff / strike / days / strikeErrPct / investCcy / optInstId / optExact / optCtVal / settleMismatch`，`optionRows[]` 含 `instId / strike / bidPx / askPx / lastPx / optApr / aprValid`（`optApr` 为扣除普通档 0.03% 手续费后的净年化）（`lastPx` 为最新成交价，无买卖盘的档位仍展示；`aprValid=false` 表示算不出卖方 APR），`decisions[]` 含 `action / reasonCode / suggestedAmt`（reasonCode 增含 `strike_wrong_side`：卖 PUT 行权价高于现价 / 卖 CALL 低于现价，摆放方向异常不推荐；`no_exact_contract`：没有同行权价 + 同到期日合约，只展示不对齐执行；`edge_abs_small`：年化差达标但单笔绝对收益差 < `minAbsEdgePct`）。`mode` 为 `read-only`（启动加 `--read-only`）或 `live/sim-okx/sim-local`；`autoAvailable / autoTrade / autoNote / autoCount` 为自动交易状态，`autoFailures` 为最近自动失败记录（最新在前，最多 10 条，字段 `ts / kind / ref / reason / detail / count`）；`guard` 给出敞口/单笔/并行/日限状态。

**Web 看板视觉与交互**：

- 布局：三栏 brutalist —— **左侧栏 260px**（工作区导航 / 本轮可执行统计 / MODE·风控卡 / 语义图例）、**中间列表 44%**（决策卡或期权链列表）、**右侧详情**（策略单据 + DECIDE 动作栏 + 底部黑色 LOG 卡）。详情区铺 28px 网格纸底纹。
- 视觉 token（与参考同源）：画布薄荷绿 `#CFE8D6`、墨色 `#121210`、纸白 `#FFFFFF`、状态绿 `#2E8C42` / 琥珀 `#E8A030` / 红 `#C03A3A`；结构线一律 3px 黑边，阴影为 4px / 6px 无模糊偏移，悬停抬升 `translate(-2px,-2px)` 阴影变 8px，按钮按下位移 4px 阴影归零；字体 Archivo（标题）/ Hanken Grotesk（正文）/ Space Mono（数字与标签），圆角基本为 0，仅 stamp 4px、chain 节点全圆。
- 决策列表：一张卡片一条 DCY 产品 —— 左上角标 `PUT 77,000`、标题「行权价 · 高卖/低买」、右侧大号期权 APR、标签（动作 / 本位 / 需跌或需涨 / 差 pp 或原因）、底部对齐的期权合约 + 买价 + 三段链节点（DCY → 期权 → 差）。点击卡片右侧出单据，选中卡黑色左标 + 加重阴影。
- 期权链：紧凑行列表（合约 / 行权价 / 买价 / 期权APR），进入时自动选中第一档有买盘的合约，右侧查看报价、卖方 APR、DCY 对照与卖出入口。
- 筛选：标的 / 方向 / 本位 chips + 到期快捷 chips + 日期输入 + SORT chips，两个页面共用同一套筛选与排序。
- 搜索：`⌘K`（Ctrl+K）聚焦，输入即按产品 ID / 合约 / 行权价本地过滤，`Esc` 清空。
- 详情单据：大标题（`卖出 PUT` / `申购低买`）+ 建议金额；三列摘要（DCY 产品 / 期权合约 / 到期）；指标表（DCY APR、期权 APR、差%、行权价、到行权（需跌/涨）、买一价、剩余天数、原因码）；对照链（标的 → DCY → 期权 → 差）用参考页的审批链方块样式。
- 动作栏：`卖出期权` / `申购 DCY` 两枚 stamp 按钮（无数据时也常驻置灰）+ `POLICY CHECK`（模式 / 下单范围 / 期权费率 / 单笔上限 / 剩余敞口 / 今日笔数 / 行情状态）。不可执行时按钮置灰并在下方说明原因（无买盘 / 无 DCY 对齐 / Web 只读）。交易启用时期权链页任意有买盘的合约都可手动卖出。快捷键 `⌘/Ctrl+Enter` 卖出、`⌘/Ctrl+D` 申购。
- 下单弹窗沿用二次确认（张数 / 投入量），样式换成同一套硬边卡片；提交成功后单据上盖一枚 `SELL` / `DCY` 印章动画。
- 偏好记忆：`标的 / 方向 / 排序`写入浏览器 `localStorage`（键 `coinoption.prefs.v1`），刷新页面或重启浏览器 / 服务后自动恢复（同一 origin）；恢复时先校验枚举值，非法值回落默认。到期 / 本位 / 搜索属于临时观察条件，不落盘。换端口 = 换 origin，缓存不共享；要清掉记忆可在控制台执行 `localStorage.removeItem("coinoption.prefs.v1")` 或清站点数据。
- 空状态：虚线硬边框 + 说明文案（无 DCY Key / 该组合无决策 / 未选中合约），不再渲染空白纸面。
- 响应式：≤1400px 收窄侧栏与列表；≤1480px 单据与动作栏上下堆叠、动作栏转为两列栅格（参考页是 1350px，这里因为单据列更密提前一档）；≤1180px 列表与详情上下堆叠；≤1100px 侧栏折叠为顶部导航。
- 设计来源：Kombai Gallery「Approva」dashboard（brutalist / mint canvas / hard black borders / offset shadows）。字体与图标走 CDN（Google Fonts、Tabler Icons），离线时回退系统字体、图标降级为空。
- 快照服务端缓存 1s（WS 实时模式；REST 模式下 `market.Fetcher` 内部另有 15s 缓存）。

> 机会榜展示**卖方有效链**：全部到期日、全部有效行权价、不按 DCY 对齐 / 7–30 天窗口 / TopN / `bid` 有无裁剪条数；**实值档直接过滤**（卖 PUT 行权价 > 现价、卖 CALL 行权价 < 现价，超 ±0.5% 容差）。无买盘档位仍保留展示价格（操作列标 `仅报价`）。排序与 OKX 期权链一致：**到期日升序 → 行权价升序 → 本位 → 方向**，方便按价格从低到高顺序对照。**决策页只列 DCY 产品**（每条一行，含对齐结果与建议），**期权链页单独列完整链**，两类数据不再混排。决策侧同类摆放异常给 `unaligned / strike_wrong_side`；期权 APR 统一为**净口径**：扣 OKX 普通档（Lvl 1）0.030% 挂单/吃单费，并按官方「手续费不超过 7% 权利金」规则封顶；因此净年化不会低于 gross 的 93%。动作判定用**双门槛**：`差% ≥ minOptionEdgePp` **且** `差% × 天数/365 ≥ minAbsEdgePct`，否则申购 DCY（短期限 20pp 可能只有 0.38% 实际收益，会被绝对门槛挡回 `edge_abs_small`）；7–30 天期限窗口只过滤决策行，不影响期权榜。决策行先按动作分组（卖期权 → 申购 DCY → 不可对齐），组内按「到期日 → 行权价 → 本位 → 方向」升序；期权链按「到期日 → 行权价 → 本位 → 方向」升序。期权链同时覆盖币本位（`BTC-USD`）与 U 本位（`BTC-USD_UM`）：优先取父链响应（实测已含 `_UM`），缺失时自动补拉并按 `instId` 去重。DCY 与期权的底层按**标的币**对齐（`BTC-USDT` ↔ `BTC-USD` ↔ `BTC-USD_UM` 视为同一标的），不会因 quote 币写法不同而误判未对齐；到期匹配用 ±1 天绝对值做**展示对齐**（兼容 DCY 16:00 与币本位期权 08:00 的结算时刻差）；**可执行动作只给「同行权价 + 同 UTC 到期日 + 有买盘」的精确档**，最近档/相邻日期只展示不下单——否则 0.5% 行权价差 × Delta≈0.5，7 天年化就差约 13pp，足以凭空跨过 20pp 阈值（原因码 `no_exact_contract`）。某档合约存在但没有买盘时，仍建立展示对齐，原因给 `no_option_quote`、不产生动作。**需跌/涨**：PUT 显示现价还要跌多少才到行权价（安全垫），CALL 显示还要涨多少；已越过行权价（实值方向）显示「已穿过」；TUI 的选中行详情同样显示该值。**对齐比例**（`settings.json` 的 `maxStrikeGapPct`，默认 0.5%）现在只用于「行权价方向/偏差过大」的诊断，不会让近似档变成可执行；要按比例放宽可执行对齐，需要新增 `alignTolerancePct` 并对行权价差做年化折价，避免重新引入假 edge（未启用）。**期权结算本位与投资币种不一致**（低买投 USDT 却对上币本位 put，或高卖投 BTC 却对上 U 本位 call）时照常展示/比较 APR，但标记 `settleMismatch`，自动执行跳过，需人工核对抵押品。**同价同到期的币本位与 U 本位期权打平时按确定性顺序兜底**（行权价差 → 到期差 → 与投资币种同本位优先 → `instId` 字典序）：低买（投入 USDT）优先 U 本位、高卖（投入 BTC）优先币本位。此前按 `tickers`（来自 map，重建时顺序随机）遍历顺序取先到者，同一 DCY 行的「对应期权 / 买价 / 本位」会在两条链之间反复横跳（如买价 290 ↔ 0.0038），看起来像「价格一会有一会没有」。

**Web API 参数**：

- `GET /api/decisions?uly=BTC-USDT` — 指定标的（默认启动参数）
- `GET /api/decisions?uly=BTC-USDT&exp=2026-11-06` — 按到期日（UTC）过滤决策/期权榜；非法日期返回 400
- `GET /api/decisions?uly=BTC-USDT&side=C` — 按方向过滤（`C`=CALL/高卖，`P`=PUT/低买，大小写不敏感）；非法值返回 400
- `GET /api/decisions?uly=BTC-USDT&settle=B` — 按结算本位过滤（`B`=币本位 BTC-USD，`U`=U本位 BTC-USD_UM，大小写不敏感，也接受 `coin`/`usdt`）；非法值返回 400
- `GET /api/decisions?uly=BTC-USDT&exp=2026-11-06&side=P&settle=B` — 组合筛选（标的 + 方向 + 本位 + 到期）
- `POST /api/auto` — 自动交易开关 `{"enabled": true|false}`（落盘 `settings.json`）；`GET /api/auto` 返回当前状态与最近一次说明
- `GET /api/auto/failures` — 最近的自动失败记录 `{"limit":10,"count":N,"failures":[{ts,kind,ref,reason,detail,count}]}`（最新在前；`kind` = `option` / `dcd` / `market`；只读看板返回 403）
- `GET /api/settings` — 读取用户设置与生效中的风控上限（含 `capitalUsdt / minDiffPct / minOptionEdgePp / minAbsEdgePct / maxStrikeGapPct / autoTrade / autoAvailable / authEnabled / limits`）
- `POST /api/settings` — 合并保存设置（只覆盖请求里出现的字段），保存后立即热更新风控与决策参数；`newAuthCode` 可改授权码，返回 `relogin:true` 时需重新登录
  ```bash
  curl -s -b /tmp/ck -H 'Content-Type: application/json' \
    -d '{"capitalUsdt":10000,"minDiffPct":2.5,"autoTrade":true}' \
    http://127.0.0.1:8080/api/settings
  ```

**Web 交易与自动执行**：

```bash
# 卖出期权（省略 px 或 px=0 时按当前买一价挂单）
curl -s -X POST http://127.0.0.1:8080/api/order/option \
  -H 'Content-Type: application/json' \
  -d '{"uly":"BTC-USDT","instId":"BTC-USD-261113-66000-P","sz":1}'

# 申购双币赢（amt 为投资币种数量；超 maxSize 由 executor 截到上限）
curl -s -X POST http://127.0.0.1:8080/api/order/dcd \
  -H 'Content-Type: application/json' \
  -d '{"uly":"BTC-USDT","productId":"BTC-USDT-261113-66000-P","amt":100}'

# 自动交易开关
curl -s -X POST http://127.0.0.1:8080/api/auto \
  -H 'Content-Type: application/json' -d '{"enabled":false}'
```

- `--web` 默认装配 executor 并开启交易（sim-local 为纸面单）；`--read-only` 关闭全部下单与自动交易。**自动交易**：默认关闭，在设置页（或侧栏 AUTO TRADE）开启后落盘，重启沿用；live 模式启动强制暂停，需在页面当次确认。
- **设置页**（左侧栏「设置」）：资金总额、差值比例、自动交易开关、三个策略阈值、修改授权码、只读风控上限。保存后资金总额立即派生总敞口 / 单笔上限，阈值立即重算决策，无需重启。
- **自动失败记录**：侧栏 AUTO TRADE 下方的 `FAIL LOG › 失败 N` 打开最近 10 条记录（时间 / 类别 / 合约或产品 / 原因 / 当时的候选摘要，相同原因标 `×N`），数据来自 WS 推送的 `autoFailures`，弹窗开着时会实时刷新；也可 `curl /api/auto/failures` 或直接 `cat ~/.coinoption/auto_failures.jsonl`。TUI 的 Settings 页底部同样列出这 10 条。
- 手动卖出：期权链里**任何有买盘**的合约都可卖（实值档仍已被榜单过滤）；决策页仍只对建议行动作。手动申购：DCY 产品**即使没有「申购」建议（不可对齐）也可手动申购**；自动执行则只挑**精确对齐的建议**（差% 最大、60s 冷却、成交前复核 APR、抵押品不一致跳过、差% 未达「差值比例」跳过、成功才写去重表）。
- 复用 TUI 同一条执行链路：`live` 真实下单 / `sim-okx` 期权走 OKX Demo、DCY 纸面 / `sim-local` 全纸面；风控（总敞口、单笔、并行、日限）与审计仍由 `internal/executor` 强制校验，拒单原因原样回显。
- 期权卖方敞口按 `sz × ctVal × ctMult × 现价` 折算（BTC 1 张 = 0.01 BTC），默认单笔 2000 USDT 可卖 2 张；更大仓位先在设置页调大资金总额，或用 `--limits` 显式覆盖，例如 `--limits 20000,5000,5,10`（总敞口/单笔/并行/日限，USDT/笔；启动时 `--limits` 优先于资金总额，Web 设置页保存后以页面值为准）。
- 端点只接受 `POST` + `application/json`（`text/plain` 返回 415），且本服务不返回 CORS 头；整站还需要授权码，未登录的写请求返回 401。
- 无 API Key 时 DCY 决策为空，自动交易没有候选；期权链来自公开行情，仍可手动卖出任意有买盘的合约（纸面）验证链路。

## 构建

```bash
make build          # 产物 ./bin/coinoption
go run ./cmd/coinoption --smoke       # 冒烟测试
go run ./cmd/coinoption --dump      # CSV 决策输出
go run ./cmd/coinoption             # TUI 主界面
go test ./internal/strategy/        # 黄金回归
```

## 运行参数

| 参数 | 环境变量 | 说明 |
| --- | --- | --- |
| `--proxy` | `COINOPTION_PROXY` | HTTP/SOCKS5 代理（如 `http://127.0.0.1:7890` / `socks5h://127.0.0.1:1080`），不填直连；必须带 scheme，非法配置启动即报错（不再静默直连） |
| `--egress` | — | 出口自检：经同一代理链路查询外部服务看到的真实出口 IP（IPv4/IPv6），用于核对 OKX API Key 白名单 |
| `--key` | `COINOPTION_API_KEY` | API Key（仅私有接口，双币赢接口实测需要） |
| `--secret` | `COINOPTION_SECRET` | API Secret |
| `--passphrase` | `COINOPTION_PASSPHRASE` | API Passphrase |
| `--demo` | — | 公开行情/Demo 兼容标志（TUI 执行路径以 `--mode` 为准） |
| `--optdebug` | — | 期权链诊断：到期日分布 × bid 覆盖 |
| `--mode` | — | `live` / `sim-okx`（期权走 Demo，DCY 本地纸面）/ `sim-local`（默认，全本地） |
| `--store` | — | 数据目录，默认 `~/.coinoption` |
| `--limits` | — | 风控覆盖 `"total,perOrder,concurrent,perDay"`（段序：总敞口 / 单笔 / 并行 / 日限；期权大名义验证需放宽），默认 `5000,2000,5,10`；启动时优先于 `settings.json` 的资金总额（Web 设置页保存后在运行期以页面值为准） |
| `--uly` | — | 标的，默认 `BTC-USDT`（支持 ETH-USDT/SOL-USDT；TUI 可用 `u` 快速切换） |
| `--web` | — | 启动 Web 看板（默认开启交易；自动交易默认关闭但沿用 `settings.json`，数据经 `/ws` 实时推送） |
| `--read-only` | — | Web 看板只读：不装配执行器、不自动交易（仍要求授权码） |
| `--addr` | — | Web 服务监听地址（`--web` 开启时生效，默认 `:8080`） |
| `--web-code` | `COINOPTION_WEB_CODE` | Web 授权码（整站访问控制）；不传则读取 `settings.json` 摘要，首次随机生成并打印；命令行显式指定优先 |
| `--ws` | — | 启用 OKX WebSocket 实时行情（默认 `true`；`--ws=false` 回退 REST 轮询/15s 缓存） |

> 运行模式在启动时固定（executor 装配后不可热切换）；Settings 页显示当前模式，切换需重启并指定 `--mode`。

## API Key 权限需求

- 双币赢（sfp/dcd）接口：需要「读取」权限（产品/订单查询）；实盘申购需要「交易」权限
- 期权卖单：需要「交易」权限（`tdMode=cross`）
- **IP 白名单**：私有接口会校验来源 IP（实测出口 `198.176.52.187` 未在 Key 白名单 → `50110`）；需在 OKX 后台加入当前出口 IP，或改用不绑定 IP 的 Key
- **代理出口是 IPv6 时**：OKX 报文里的 IP 是「它看到的出口」，不是代理配置本身。若白名单只填了 IPv4，而出口为 IPv6（`2408:...`），必然 50106/50110。先跑 `./bin/coinoption --proxy <...> --egress` 看真实出口，再二选一：把该 IPv6 加入白名单，或在代理侧强制 IPv4 出口（Clash `ipv6: false` / 换 IPv4-only 节点）。配置非法的代理（如漏写 `http://`）现在会启动即报错，不会静默直连

## 下一步验证清单（需要人工配合）

1. **白名单**：把代理出口 IP 加入 API Key 白名单（或换不绑 IP 的 Key），使 `--dump` 能看到 `dcdProducts > 0`。
2. **`sim-okx` 期权 Demo**：提供模拟盘专用 Key，执行
   `./bin/coinoption --mode sim-okx --order opt --inst BTC-USD_UM-261106-83000-C --sz 1 --px 0 --limits 500000,160000,5,10`
   预期：Demo 成交回执写入 `orders.jsonl`，DCY 仍走本地纸面。
3. **小实盘**：先用 `--limits 5000,2000,5,10` 下 1 笔最小 DCY 申购（如 10 USDT 等值），确认订单与 `--positions` 平台数据一致。
4. **自动执行**：在 sim-local 打开 Settings → `a` 观察 1–2 轮（同一产品只做一次）；确认无误后再考虑 sim-okx / live（live 需当次 `y` 确认）。
