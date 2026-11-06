package okx

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

const (
	pathSpotTicker  = "/api/v5/market/ticker"
	pathOptionTick  = "/api/v5/market/tickers"
	pathOptSummary  = "/api/v5/public/opt-summary"
	pathInstruments = "/api/v5/public/instruments"
	pathCandles     = "/api/v5/market/candles"
)

// SpotTicker 现货行情节选。
type SpotTicker struct {
	InstID  string `json:"instId"`
	Last    string `json:"last"`  // 最新成交价
	BidPx   string `json:"bidPx"` // 买一价
	AskPx   string `json:"askPx"` // 卖一价
	Open24h string `json:"open24h"`
	High24h string `json:"high24h"`
	Low24h  string `json:"low24h"`
	Vol24h  string `json:"vol24h"`
	TS      string `json:"ts"`
}

// OptionTicker 期权行情节选（/market/tickers?instType=OPTION）。
type OptionTicker struct {
	InstType   string `json:"instType"` // SPOT / OPTION（WS tickers 推送）
	InstID     string `json:"instId"`   // BTC-USDT-260129-130000-P
	Uly        string `json:"uly"`      // BTC-USDT
	InstFamily string `json:"instFamily"`
	Ccy        string `json:"ccy"`
	OptType    string `json:"optType"`   // C | P
	OptStrike  string `json:"optStrike"` // 行权价
	ExpTime    string `json:"expTime"`   // 到期毫秒时间戳
	Last       string `json:"last"`
	BidPx      string `json:"bidPx"`
	BidSz      string `json:"bidSz"`
	AskPx      string `json:"askPx"`
	AskSz      string `json:"askSz"`
	BidIV      string `json:"bidVol"` // 卖出端隐波（买盘出价隐波）
	AskIV      string `json:"askVol"`
	MarkIV     string `json:"markVol"`
	MarkPx     string `json:"markPx"`
	CtVal      string `json:"ctVal"`
	CtValCcy   string `json:"ctValCcy"` // BTC（币本位）或 USDT
	SettleCcy  string `json:"settleCcy"`
	TS         string `json:"ts"`
}

// OptSummary 期权定价/隐波总览（/public/opt-summary，按 underlying 汇总）。
type OptSummary struct {
	InstID       string `json:"instId"`
	Uly          string `json:"uly"`
	ExpTime      string `json:"expTime"`
	MarkVol      string `json:"markVol"`
	BidVol       string `json:"bidVol"`
	AskVol       string `json:"askVol"`
	BidPx        string `json:"bidPx"`
	AskPx        string `json:"askPx"`
	ForwardPx    string `json:"forwardPx"`
	Delta        string `json:"delta"`
	Gamma        string `json:"gamma"`
	Vega         string `json:"vega"`
	Theta        string `json:"theta"`
	PutCallRatio string `json:"putCallRatio"`
	TS           string `json:"ts"`
}

// Instrument 期权产品基础信息（/public/instruments?instType=OPTION）。
type Instrument struct {
	InstID     string `json:"instId"`
	Uly        string `json:"uly"`
	InstFamily string `json:"instFamily"`
	Ccy        string `json:"ccy"`
	OptType    string `json:"optType"`
	OptStrike  string `json:"optStrike"` // 老 tag 保留（真实响应用 stk）
	Stk        string `json:"stk"`       // 行权价（实测字段名）
	ExpTime    string `json:"expTime"`   // 到期时间（时区相关，UTC）
	TickSz     string `json:"tickSz"`
	CtVal      string `json:"ctVal"`
	CtValCcy   string `json:"ctValCcy"`
	CtMult     string `json:"ctMult"` // 合约乘数（_UM 链实测 0.01；未给=1）
	SettleCcy  string `json:"settleCcy"`
	MinSz      string `json:"minSz"`
}

// DualProduct/GetDualProducts 已迁移到 dcd.go（官方路径 /api/v5/finance/sfp/dcd/*）。

// GetSpotTicker 单个现货 ticker。
func (c *Client) GetSpotTicker(instId string) ([]SpotTicker, error) {
	q := url.Values{"instId": {instId}}
	return wrap[SpotTicker](c.get(context.Background(), pathSpotTicker, q))
}

// GetTicker 单合约 ticker（现货/期权/合约通用，同一 /market/ticker 端点）。
func (c *Client) GetTicker(instId string) ([]SpotTicker, error) {
	return c.GetSpotTicker(instId)
}

// GetOptionTickers 某 uly 的全部期权行情（含 IV/权利金盘口）。
func (c *Client) GetOptionTickers(uly string) ([]OptionTicker, error) {
	q := url.Values{
		"instType": {"OPTION"},
		"uly":      {uly},
	}
	return wrap[OptionTicker](c.get(context.Background(), pathOptionTick, q))
}

// GetOptSummary 某 uly 的期权隐波/敏感度总览。
func (c *Client) GetOptSummary(uly string) ([]OptSummary, error) {
	q := url.Values{"uly": {uly}}
	return wrap[OptSummary](c.get(context.Background(), pathOptSummary, q))
}

// Candle 日线/分钟线收盘数据（只保留策略需要的字段）。
type Candle struct {
	Ts      int64
	Open    float64
	High    float64
	Low     float64
	Close   float64
	Confirm bool // true = 已收盘
}

// GetCandles 拉取 K 线，返回**按时间升序**（旧→新）的列表。
//
// OKX 返回的是 [ts, o, h, l, c, vol, volCcy, volCcyQuote, confirm] 字符串数组，
// 且默认新→旧；这里反转，调用方可以直接用 closes 计算已实现波动率。
// bar 例如 "1D"、"1H"；limit 最大 300。
func (c *Client) GetCandles(instID, bar string, limit int) ([]Candle, error) {
	if limit <= 0 {
		limit = 31
	}
	if limit > 300 {
		limit = 300
	}
	q := url.Values{
		"instId": {instID},
		"bar":    {bar},
		"limit":  {strconv.Itoa(limit)},
	}
	rows, err := wrap[[]string](c.get(context.Background(), pathCandles, q))
	if err != nil {
		return nil, err
	}
	out := parseCandles(rows)
	if len(out) == 0 {
		return nil, fmt.Errorf("K 线为空: %s %s", instID, bar)
	}
	return out, nil
}

// GetCandlesHistory 分页向前拉取历史 K 线（用于 2–3 年回放）。
//
// startMs 为最早时间戳（毫秒，0 = 不限）；want 为期望根数（上限 3000）。
// OKX 的 `after` 语义是「返回早于该 ts 的记录」，因此用当前最老一根的 ts
// 反复向前翻页；返回按时间升序（旧→新）。
func (c *Client) GetCandlesHistory(instID, bar string, startMs int64, want int) ([]Candle, error) {
	if want <= 0 {
		want = 730
	}
	if want > 3000 {
		want = 3000
	}
	var all []Candle
	oldest := int64(0)
	for len(all) < want {
		limit := 300
		if rem := want - len(all); rem < limit {
			limit = rem
		}
		q := url.Values{
			"instId": {instID},
			"bar":    {bar},
			"limit":  {strconv.Itoa(limit)},
		}
		if oldest > 0 {
			q.Set("after", strconv.FormatInt(oldest, 10))
		}
		rows, err := wrap[[]string](c.get(context.Background(), pathCandles, q))
		if err != nil {
			if len(all) > 0 {
				break // 已拿到部分数据：回放可以继续，不因翻页失败整体报错
			}
			return nil, err
		}
		page := parseCandles(rows)
		if len(page) == 0 {
			break
		}
		all = append(page, all...)
		if page[0].Ts == oldest {
			break // 服务端没有更早的数据（防死循环）
		}
		oldest = page[0].Ts
		if startMs > 0 && oldest <= startMs {
			break
		}
		if len(page) < limit {
			break
		}
	}
	if startMs > 0 {
		kept := all[:0]
		for _, k := range all {
			if k.Ts >= startMs {
				kept = append(kept, k)
			}
		}
		all = kept
	}
	if len(all) > want {
		all = all[len(all)-want:]
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("K 线为空: %s %s（start=%d）", instID, bar, startMs)
	}
	return all, nil
}

// parseCandles 把 OKX 的「新→旧」字符串数组解析为「旧→新」。
func parseCandles(rows [][]string) []Candle {
	out := make([]Candle, 0, len(rows))
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		ts, e1 := strconv.ParseInt(r[0], 10, 64)
		o, e2 := strconv.ParseFloat(r[1], 64)
		h, e3 := strconv.ParseFloat(r[2], 64)
		l, e4 := strconv.ParseFloat(r[3], 64)
		cl, e5 := strconv.ParseFloat(r[4], 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
			continue
		}
		confirm := len(r) >= 9 && r[8] == "1"
		out = append(out, Candle{Ts: ts, Open: o, High: h, Low: l, Close: cl, Confirm: confirm})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// GetOptionInstruments 某 uly 的期权产品基础信息。
func (c *Client) GetOptionInstruments(uly string) ([]Instrument, error) {
	q := url.Values{
		"instType": {"OPTION"},
		"uly":      {uly},
	}
	return wrap[Instrument](c.get(context.Background(), pathInstruments, q))
}
