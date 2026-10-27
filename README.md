# coinoption — OKX 双币赢 vs 期权 APR 对比工具

Go + TUI 开发中。文档：`DEVELOPMENT_PLAN.md`（计划）、`ARCHITECTURE.md`(架构)。

## 当前进度

- **M0 完成**：OKX REST 客户端（签名/限速/重试/代理）✅ 公开行情实测 ✅ 双币赢 `sfp/dcd` 全端点 ✅
- **M1 完成**：strategy 对齐引擎（matcher/APR/决策）+ 黄金用例 + 实线校准（`|strike gap|` 匹配、UM=USD 计价）✅
- **M2 v1 完成**：Bubble Tea TUI Dashboard（默认运行模式，20s 自动刷新，`r` 手动刷新，`q` 退出）✅
- **M3 v1 完成**：executor 执行层 ✅ —— 风控守卫（总敞口 5000 / 单笔 2000 / 并行 5 / 日 10 强校验 + 重启恢复）、DCY 申购两步流（quote→trade）、期权卖单、三种模式路由（live / sim-okx / sim-local）、本地 DCY 纸面模拟结算（[SIM] 单落盘、到期按现价结算）、统一持仓视图、JSONL 审计留痕（`~/.coinoption/`）
- **M3.2 完成**：TUI 下单与持仓 ✅ —— 决策行上 `o` 卖期权 / `d` 申购双币赢，数量输入 → 二次确认 → 提交（`y`/`n`），`p` 持仓页（异步刷新缓存 + `j/k` 滚动），头部常驻模式徽标与敞口/在持进度；live 模式启动时从平台 DCY 在持单 + 期权空头恢复风控敞口

### TUI 快捷键

| 键 | 作用 |
| --- | --- |
| `1` / `3` / `p` | 决策表 / 持仓页切换 |
| `4` / `l` | 审计日志页（submit/reject/settle/restore，最新 200 条） |
| `o` | 对选中的「押注期权」行卖期权（输入张数 → 二次确认） |
| `d` | 对选中的「投注双币赢」行申购 DCY（输入投资币种金额 → 二次确认） |
| `y` / `n` / `Esc` | 确认 / 取消 |
| `j` / `k` | 持仓页滚动 |
| `r` / `q` | 刷新 / 退出 |

刷新失败时保留上一次成功快照并在头部标红提示（不会清空决策表）；持仓页与日志页在可见时随 20s 心跳自动刷新。

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

数据落盘（默认 `~/.coinoption/`，`--store` 覆盖）：`sim_orders.jsonl` / `orders.jsonl` / `audit_log.jsonl`，可直接 `cat` 对账。

### 实测结论（M3 新增）
- **期权合约名义**：BTC 币本位（BTC-USD）与 UM 链单张均为 **1 BTC ≈ 8 万 USDT**，`ctMult=0.01` 只是权利金报价的数量裁量，不影响名义/权利金总口径 → 默认单笔 2000 USDT 无法卖 BTC 期权，期权腿要么放宽 `--limits`、要么依赖 DCY 的小额申购
- **instruments 端点不认 `BTC-USD_UM`**（51014）：UM 合约挂在父链 `BTC-USD` 响应内，代码已自动降级查询
- 行权价字段实测为 `stk`（非 `optStrike`）；UM 期权到期 16:00 UTC（币本位 08:00），±1 天窗口已兼容

### 实测结论（M0–M2 保留）
- **模拟盘（Demo）**：现货/期权数据可用（期权 1026 合约），但**双币赢接口在 Demo 环境返回 500**——金融产品不在模拟盘范围，与文档预期一致
- **双币赢 API（实盘）需要 API Key（只需读取权限）**；期权/现货公开数据无需 Key
- 无 Key 时 TUI 显示「期权机会榜」（纯期权卖方 APR 降序），接入 Key 后呈现完整 DCY↔期权 对照决策

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
| `--proxy` | `COINOPTION_PROXY` | HTTP/SOCKS5 代理（如 `http://127.0.0.1:7890`），不填直连 |
| `--key` | `COINOPTION_API_KEY` | API Key（仅私有接口，双币赢接口实测需要） |
| `--secret` | `COINOPTION_SECRET` | API Secret |
| `--passphrase` | `COINOPTION_PASSPHRASE` | API Passphrase |
| `--demo` | — | 走 OKX 模拟盘（期权验证可；DCY 需实盘 Key） |
| `--uly` | — | 标的，默认 `BTC-USDT`（支持 ETH-USDT/SOL-USDT） |

## API Key 权限需求

- 双币赢（sfp/dcd）接口：需要「读取」权限（产品/订单查询）
- 后续 M3 下单阶段：需要「交易」权限
