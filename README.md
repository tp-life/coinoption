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
- **M4 扩展**：Web 看板双标签（对比决策 / 完整期权链）✅；Web 手动下单（`--web-trade`，默认只读）✅ —— 决策行内「卖出期权 / 申购 DCY」弹窗二次确认，复用 executor 风控/审计/模式路由，live 模式启动打印实盘警告

### TUI 快捷键

| 键 | 作用 |
| --- | --- |
| `1` / `3` / `p` | 决策表 / 持仓页切换 |
| `2` | 完整期权链（参考视图，不直接下单；`1` 返回决策页） |
| `4` / `l` | 审计日志页（submit/reject/settle/restore，最新 200 条） |
| `5` / `s` | Settings 页（自动执行开关、阈值调整、风控只读展示） |
| `a` | Settings 页内切换自动执行（live 需 `y` 二次确认） |
| `[` / `]` | Settings 页内期权边际阈值 ±5pp（下次刷新生效） |
| `o` | 对选中的「卖CALL / 卖PUT」行卖出该期权（输入张数 → 二次确认） |
| `d` | 对选中的「申购高卖 / 申购低买」行申购 DCY（输入投资币种金额 → 二次确认） |
| `y` / `n` / `Esc` | 确认 / 取消 |
| `j` / `k` | 持仓页滚动 |
| `r` / `q` | 刷新 / 退出 |
| `u` | 弹窗选择标的（BTC / ETH / SOL）：`↑/↓` 或 `j/k` 移动，`Enter` 确认，`Esc` 取消 |
| `e` | 弹窗选择到期日：`全部（不过滤）` / 快照中每个可用到期日（含行数） / `手动输入日期…` |
| `t` | 弹窗选择方向：`全部方向` / `CALL 看涨（高卖）` / `PUT 看跌（低买）`，含当前组合行数 |
| `b` | 弹窗选择结算本位：`全部本位` / `币本位（BTC-USD 链）` / `U本位（BTC-USD_UM 链）`，含当前组合行数 |
| `Esc` | 关闭弹窗；无弹窗时清除全部筛选（方向 + 本位 + 到期），恢复全量 |

> 当前组合：`标的 + 方向 + 本位 + 到期` 四个维度可自由组合，表头上方的「组合」条实时显示（如 `BTC-USDT · PUT · 币本位 · 2026-11-06`），并给出各维度快捷项与行数；`t` 选方向、`b` 选本位、`e` 选到期、`u` 切标的，`Esc` 一键清空。四个筛选同时作用于决策行和期权榜。
>
> **方向与本位选择会记忆**：`t` / `b` 的选择即时写入 `settings.json`（`sideFilter` / `settleFilter`），退出后下次启动自动恢复（`Esc` 清除也会同步落盘）。到期筛选不记忆。

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
- 开启后：每轮刷新最多 1 单、同一产品/合约只自动做一次（启动时用现有持仓/在持单播种去重）、最小间隔 60s。
- 期权张数 = `min(单笔上限, 剩余敞口) ÷ 现价` 向下取整；BTC 默认 2000 USDT 不足 1 张，因此默认额度下自动执行实际只会申购 DCY。
- **live 模式**：开启需当次 `y` 确认，进程重启后强制重置为关闭（防止静默连续下单）；`sim-okx` / `sim-local` 沿用落盘开关。
- 所有自动单与手动单走同一条 executor 路径，风控硬上限、产品规模区间、审计留痕完全一致。

> 下单键在未接入 executor（只读推荐）或选错行类型时会给出错误提示，不会发出请求；风控拒单原因（敞口/单笔/并行/日限）会以红字回显。
>
> 注意：期权单张名义 ≈ 1 BTC（约 8 万 USDT），默认单笔上限 2000 USDT 会直接拒单——TUI 里卖 BTC/ETH 期权需以 `--limits 500000,160000,5,10` 放宽，或改用 DCY 小额申购。live 启动恢复覆盖「平台 DCY 在持单 + 期权空头敞口」，**当日笔数**仍取自本地 audit（平台历史成交未计入，防重复提交靠本地记录）。



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
# 期权单张名义 = 1 BTC ≈ 8 万 USDT > 默认单笔 2000 —— 验证期权腿需放宽：
./bin/coinoption --order opt --inst BTC-USD_UM-261106-83000-C --sz 2 \
  --limits 500000,160000,5,10
```

数据落盘（默认 `~/.coinoption/`，`--store` 覆盖）：`sim_orders.jsonl` / `orders.jsonl` / `audit_log.jsonl` / `settings.json`，可直接 `cat` 对账。

### 实测结论（M3 新增）
- **期权合约名义**：BTC 币本位（BTC-USD）与 UM 链单张均为 **1 BTC ≈ 8 万 USDT**，`ctMult=0.01` 只是权利金报价的数量裁量，不影响名义/权利金总口径 → 默认单笔 2000 USDT 无法卖 BTC 期权，期权腿要么放宽 `--limits`、要么依赖 DCY 的小额申购
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

### WebSocket 实时行情（M4）

- 公共行情走 OKX WS `wss://ws.okx.com/ws/v5/public`（Demo 为 `wss://wspap.okx.com/ws/v5/public?brokerId=9999`），默认开启；`--ws=false` 回退 REST。
- 订阅内容：现货 `BTC-USDT` + 当前标的全部期权 `tickers`（每张合约一个 `instId`，按 300 个一批发送，避开 64KB 单帧上限）。OKX 的 `tickers` 不支持按 `instFamily` 批量订阅期权；`opt-summary` 只有 IV/Greeks 没有 `bidPx`，无法复用现有卖方 APR 公式，因此没有采用。
- 连接每 20s 发送 `ping`，60s 无数据视为断链；断链后指数退避（1s → 2s → … → 30s）重连并自动重放全部订阅。
- 缓存与重算：WS 只更新 `bid/ask/last` 等动态字段；`market.Live` 每 500ms 最多重组一次策略结果。DCY 产品与 `instruments` 仍走 REST（DCY 30s、产品信息 5min），不占用 WS。
- 降级：WS 断开超过缓存周期后，后台每 30s 用 REST 拉一次现货/期权行情兜底；恢复后自动切回 WS。TUI 头部显示 `WS 实时 / WS 重连中 / WS 降级`，Web 页头额外显示 `REST`（未启用或不可恢复的 WS 配置错误）。
- Web 端 `GET /ws` 每秒推送当前筛选下的完整快照（与 `/api/decisions` 同构）；页面优先用 WebSocket，断线自动重连，重连期间用 REST 兜底。



```bash
./bin/coinoption --web --addr 0.0.0.0:8080
# 启用手动下单（默认只读）：sim-local 下期权为纸面单
./bin/coinoption --web --web-trade --mode sim-local --addr 127.0.0.1:8080
```

- `GET /` — 决策看板页（内嵌 HTML，优先通过 `/ws` 接收每秒推送；WS 不可用时自动回退 `/api/decisions`）
- `GET /ws` — WebSocket 推送（查询参数与 `/api/decisions` 相同：`uly / exp / side / settle`），每秒一帧完整快照；断线由浏览器自动重连
- `GET /api/decisions` — 当前决策 JSON（camelCase：`at / uly / optionUly / spotPrice / optionCount / dcdCount / decisions / optionRows / products / dcyAvailable / warn / wsConnected / wsStatus / mode / tradeEnabled / guard`）；`decisions[].row` 含 `productId / expTime / dcyApr / optApr / diff / strike / days / strikeErrPct / investCcy / optInstId`，`decisions[]` 含 `action / reasonCode / suggestedAmt`（reasonCode 增含 `strike_wrong_side`：卖 PUT 行权价高于现价 / 卖 CALL 低于现价，摆放方向异常不推荐）。`mode` 为 `read-only`（未开 `--web-trade`）或 `live/sim-okx/sim-local`；`guard` 给出敞口/单笔/并行/日限状态。

**Web 看板交互**：

- 币种切换：页内「标的」按钮（BTC / ETH / SOL）→ 带 `uly` 重新拉取决策（当前选中高亮）
- 方向筛选：`全部 / CALL / PUT` 按钮 → 带 `side=C|P` 过滤决策与期权榜
- 本位筛选：`全部 / 币本位 / U本位` 按钮 → 带 `settle=B|U` 过滤决策与期权榜（表头同步显示「本位」列）
- 到期筛选：快捷到期按钮（如 `11-06(12)`，按快照自动生成）+ 日期选择器 + 「筛选」/「全部」按钮
- 当前组合：`标的 + 方向 + 到期` 自由组合，页头与空状态都会显示当前组合
- 页面分为两个标签页（与 TUI `1` / `2` 对应）：**对比决策**（默认，每条 DCY 产品一行，直接对照 DCY APR ↔ 同档期权 APR）与**完整期权链**（参考视图，覆盖全部卖方有效档位，并在「对应 DCY」列标出可对照产品）。两个标签页共用标的 / 方向 / 本位 / 到期筛选，避免 DCY 行与期权行在同一张表里混排。
- 对比决策表列：操作 / DCY 产品 / 对应期权 / 到期(UTC) / 方向 / 本位 / 行权价 / 距现价% / 买价 / DCY APR / 期权 APR / 差 / 偏差% / 天 / 原因 / 建议金额 / 下单；「对应期权」直接给出对齐到的合约（如 `BTC-USDT-261113-66000-P` ↔ `BTC-USD-261113-66000-P`）
- 对比决策排序：先按动作分组（卖期权 → 申购 DCY → 不可对齐 → 无动作），组内按「到期日 → 行权价 → 本位 → 方向」升序；不再按差%排序，避免行权价在列表中来回跳
- 完整期权链排序：到期日 → 行权价 → 本位（币本位先）→ 方向（CALL 先）；APR / 买价不参与排序
- 下单按钮（需 `--web-trade`）：决策行动作为 `卖期权` 时给出「卖出期权」，为 `申购双币赢` 时给出「申购 DCY」；点击后弹窗填写张数 / 投入量并二次确认。未启用时该列显示 `—`，页头提示 Web 只读
- 完整期权链表列：操作 / 期权合约 / 到期(UTC) / 方向 / 本位 / 行权价 / 距现价% / 买价 / 卖价 / 期权 APR / 天 / 对应 DCY / DCY APR / 差
- 顶部图例说明方向和买卖语义：`CALL 看涨 / PUT 看跌`，`卖CALL / 卖PUT = SELL（卖出期权，收权利金）`，`申购DCY（高卖→到期卖币 / 低买→到期买币）`，并标注 `本工具仅卖期权，不买入期权`
- 快照服务端缓存 1s（WS 实时模式；REST 模式下 `market.Fetcher` 内部另有 15s 缓存，避免每个页面轮询都打 OKX）

> 机会榜展示**卖方有效链**：全部到期日、全部有效行权价、不按 DCY 对齐 / 7–30 天窗口 / TopN / `bid` 有无裁剪条数；**实值档直接过滤**（卖 PUT 行权价 > 现价、卖 CALL 行权价 < 现价，超 ±0.5% 容差）。无买盘档位仍保留展示价格（操作列标 `仅报价`）。排序与 OKX 期权链一致：**到期日升序 → 行权价升序 → 本位 → 方向**，方便按价格从低到高顺序对照。**决策页只列 DCY 产品**（每条一行，含对齐结果与建议），**期权链页单独列完整链**，两类数据不再混排。决策侧同类摆放异常给 `unaligned / strike_wrong_side`；7–30 天期限窗口只过滤决策行，不影响期权榜。决策行先按动作分组（卖期权 → 申购 DCY → 不可对齐），组内按「到期日 → 行权价 → 本位 → 方向」升序；期权链按「到期日 → 行权价 → 本位 → 方向」升序。期权链同时覆盖币本位（`BTC-USD`）与 U 本位（`BTC-USD_UM`）：优先取父链响应（实测已含 `_UM`），缺失时自动补拉并按 `instId` 去重。DCY 与期权的底层按**标的币**对齐（`BTC-USDT` ↔ `BTC-USD` ↔ `BTC-USD_UM` 视为同一标的），不会因 quote 币写法不同而误判未对齐；到期匹配用 ±1 天绝对值（兼容 DCY 16:00 与币本位期权 08:00 的结算时刻差）；某档合约存在但没有买盘时，仍建立**展示对齐**（决策行显示对应期权），只是原因给 `no_option_quote`、不产生可执行动作。匹配优先选「有买盘且最近」的档位，避免被更近的空盘档抢走。

**Web API 参数**：

- `GET /api/decisions?uly=BTC-USDT` — 指定标的（默认启动参数）
- `GET /api/decisions?uly=BTC-USDT&exp=2026-11-06` — 按到期日（UTC）过滤决策/期权榜；非法日期返回 400
- `GET /api/decisions?uly=BTC-USDT&side=C` — 按方向过滤（`C`=CALL/高卖，`P`=PUT/低买，大小写不敏感）；非法值返回 400
- `GET /api/decisions?uly=BTC-USDT&settle=B` — 按结算本位过滤（`B`=币本位 BTC-USD，`U`=U本位 BTC-USD_UM，大小写不敏感，也接受 `coin`/`usdt`）；非法值返回 400
- `GET /api/decisions?uly=BTC-USDT&exp=2026-11-06&side=P&settle=B` — 组合筛选（标的 + 方向 + 本位 + 到期）

**Web 手动下单（需 `--web-trade`）**：

```bash
# 卖出期权（省略 px 或 px=0 时按当前买一价挂单）
curl -s -X POST http://127.0.0.1:8080/api/order/option \
  -H 'Content-Type: application/json' \
  -d '{"uly":"BTC-USDT","instId":"BTC-USD-261113-66000-P","sz":1}'

# 申购双币赢（amt 为投资币种数量；超 maxSize 由 executor 截到上限）
curl -s -X POST http://127.0.0.1:8080/api/order/dcd \
  -H 'Content-Type: application/json' \
  -d '{"uly":"BTC-USDT","productId":"BTC-USDT-261113-66000-P","amt":100}'
```

- 默认只读；只有启动时加 `--web-trade` 才装配 executor 并开放上述端点（否则 403）。
- 复用 TUI 同一条执行链路：`live` 真实下单 / `sim-okx` 期权走 OKX Demo、DCY 纸面 / `sim-local` 全纸面；风控（总敞口、单笔、并行、日限）与审计仍由 `internal/executor` 强制校验，拒单原因原样回显。
- 只允许对**当前快照里已明确给出建议**的决策行下单：期权必须命中 `action=buy_option` 且 `instId` 一致，DCY 必须命中 `action=subscribe_dual` 且 `productId` 一致。这样不会绕过实值档过滤、期限窗口或对齐逻辑直接 POST 任意合约。
- 端点只接受 `POST` + `application/json`（`text/plain` 返回 415），且本服务不返回 CORS 头，降低跨站表单直接下单的风险；但这不等于身份认证——**任何能访问该端口的人都能下单**。
- 默认 `--addr :8080` 监听所有网卡；实盘启用 `--web-trade` 时会打印醒目警告。建议改为 `--addr 127.0.0.1:8080`，仅在需要手机/局域网访问时再放开，并自行加防火墙 / 反向代理认证。
- 回执：`{ok, mode, kind, ref, ordId, quoteId, amt, investCcy, amtUsdt, premium, sim, detail}`；参数/风控错误返回 400 + `{error}`。
- 期权卖方的风控敞口按合约名义折算（BTC 单张名义约 1 BTC）；默认单笔上限 2000 USDT 会直接拒掉 1 张 BTC 期权。要实际下单需按自身风险承受能力放宽 `--limits`，例如 `--limits 5000,100000,5,10`（总敞口/单笔/并行/日限，单位 USDT/笔）。页面弹窗会显示当前单笔上限与剩余敞口。
- 无 API Key 时 DCY 产品快照为空、决策行也为空，因此 Web 没有可下单行（完整期权链仍可查看）；要下单需接好 Key 让 DCY 产品与决策生成（`sim-local` 下仍为纸面单）。

> Web 页与 TUI 共用同一决策数据链路（okx client → market.Fetcher → strategy.Build）；期权榜**不**再按「DCY 已支持对齐的合约」裁剪，而是展示卖方有效链（覆盖全部到期日、全部有效行权价、不截断条数、不再套 7–30 天期限窗口——期限窗口只作用于决策行；实值档已过滤），并按到期日 + 行权价升序排列。Web 页用「对比决策 / 完整期权链」两个标签页分离主视图与参考数据：**对比决策页只列 DCY 产品**，每行给出对齐到的期权与 APR 差；**完整期权链页**保留全部卖方有效档位，并通过「对应 DCY」列回链到双币赢产品。页面新增「距现价% / 买价」列；到期快捷按钮不再限制前 12 个，与 OKX 实际列表对照时用「到期 + 方向 + 本位」筛选收敛。

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
| `--limits` | — | 风控覆盖 `"total,perOrder,concurrent,perDay"`（段序：总敞口 / 单笔 / 并行 / 日限；期权大名义验证需放宽），默认 `5000,2000,5,10` |
| `--uly` | — | 标的，默认 `BTC-USDT`（支持 ETH-USDT/SOL-USDT；TUI 可用 `u` 快速切换） |
| `--web` | — | 启动 Web 展示服务（决策看板；默认经 `/ws` 实时推送） |
| `--web-trade` | — | Web 看板启用手动下单（默认只读；`live` 会提交真实订单，建议配合 `--addr 127.0.0.1:8080`） |
| `--addr` | — | Web 服务监听地址（`--web` 开启时生效，默认 `:8080`） |
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
