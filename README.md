# coinoption — OKX 双币赢 vs 期权 APR 对比工具

Go + TUI 开发中。文档：`DEVELOPMENT_PLAN.md`（计划）、`ARCHITECTURE.md`(架构)。

## 当前进度

- **M0 完成**：OKX REST 客户端（签名/限速/重试/代理）✅ 公开行情实测 ✅ 双币赢 `sfp/dcd` 全端点 ✅
- **M1 完成**：strategy 对齐引擎（matcher/APR/决策）+ 黄金用例（文档样例锚定）✅ `--dump` 实时验证通道 ✅
- **M2 v1 完成**：Bubble Tea TUI Dashboard（默认运行模式，20s 自动刷新，`r` 手动刷新，`q` 退出）✅

### 实测结论
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
