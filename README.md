# coinoption — OKX 双币赢 vs 期权 APR 对比工具

Go + TUI 开发中。文档：`DEVELOPMENT_PLAN.md`（计划）、`ARCHITECTURE.md`(架构)。

## 当前进度（M0 完成）

- OKX REST 客户端：HMAC 签名、限速（10 rps）、429/5xx 重试、代理支持 ✅
- 公开行情打通（实测）：现货 ticker、BTC-USD 期权链（1304 合约）、行情 ✅
- 双币赢 `/api/v5/finance/sfp/dcd/*` 端点封装（报价→下单两步式）✅（需 API Key 实测）
- 关于期权底层：`--uly BTC-USDT` 自动降级使用 `BTC-USD` 底层；另发现 `BTC-USD_UM`（USDT 本位期权）可用

## 构建

```bash
make build          # 产物 ./bin/coinoption
go run ./cmd/coinoption --smoke --uly BTC-USDT   # 冒烟测试
```

## 运行参数

| 参数 | 环境变量 | 说明 |
| --- | --- | --- |
| `--proxy` | `COINOPTION_PROXY` | HTTP/SOCKS5 代理（如 `http://127.0.0.1:7890`），不填直连 |
| `--key` | `COINOPTION_API_KEY` | API Key（仅私有接口，双币赢接口实测需要） |
| `--secret` | `COINOPTION_SECRET` | API Secret |
| `--passphrase` | `COINOPTION_PASSPHRASE` | API Passphrase |
| `--uly` | — | 标的，默认 `BTC-USDT` |

## API Key 权限需求

- 双币赢（sfp/dcd）接口：需要「读取」权限（产品/订单查询）
- 后续 M3 下单阶段：需要「交易」权限
