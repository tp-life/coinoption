package okx

import (
	"context"
	"net/url"
)

const (
	pathSpotTicker  = "/api/v5/market/ticker"
	pathOptionTick  = "/api/v5/market/tickers"
	pathOptSummary  = "/api/v5/public/opt-summary"
	pathInstruments = "/api/v5/public/instruments"
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
	InstID     string `json:"instId"` // BTC-USDT-260129-130000-P
	Uly        string `json:"uly"`    // BTC-USDT
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
	OptStrike  string `json:"optStrike"`
	ExpTime    string `json:"expTime"` // 到期时间（时区相关，UTC）
	TickSz     string `json:"tickSz"`
	CtVal      string `json:"ctVal"`
	CtValCcy   string `json:"ctValCcy"`
	SettleCcy  string `json:"settleCcy"`
	MinSz      string `json:"minSz"`
}

// DualProduct/GetDualProducts 已迁移到 dcd.go（官方路径 /api/v5/finance/sfp/dcd/*）。

// GetSpotTicker 单个现货 ticker。
func (c *Client) GetSpotTicker(instId string) ([]SpotTicker, error) {
	q := url.Values{"instId": {instId}}
	return wrap[SpotTicker](c.get(context.Background(), pathSpotTicker, q))
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

// GetOptionInstruments 某 uly 的期权产品基础信息。
func (c *Client) GetOptionInstruments(uly string) ([]Instrument, error) {
	q := url.Values{
		"instType": {"OPTION"},
		"uly":      {uly},
	}
	return wrap[Instrument](c.get(context.Background(), pathInstruments, q))
}
