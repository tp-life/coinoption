package okx

import (
	"fmt"
	"net/url"
)

// ─── 双币赢（DCD, Dual Currency Deposit）── 官方分节：金融产品/双币赢 ───
// 全部端点限速规则为 User ID，需要签名（M0 实测确认权限要求）。

const (
	pathDCDCurrencyPairs = "/api/v5/finance/sfp/dcd/currency-pair"
	pathDCDProducts      = "/api/v5/finance/sfp/dcd/products"
	pathDCDQuote         = "/api/v5/finance/sfp/dcd/quote"         // 10次/60s
	pathDCDTrade         = "/api/v5/finance/sfp/dcd/trade"         // 2次/60s
	pathDCDRedeemQuote   = "/api/v5/finance/sfp/dcd/redeem-quote"  // 10次/60s
	pathDCDRedeem        = "/api/v5/finance/sfp/dcd/redeem"        // 2次/60s
	pathDCDOrderStatus   = "/api/v5/finance/sfp/dcd/order-status"  // 3次/s
	pathDCDOrderHistory  = "/api/v5/finance/sfp/dcd/order-history" // 1次/s
)

// DCDResult 统一简单回执结构（quote/trade/redeem 均为 data[0] 对象）。
type DCDResult struct {
	OrdID string `json:"ordId"`
	State string `json:"state"`
}

// DCDCurrencyPair 可用币对。
type DCDCurrencyPair struct {
	BaseCcy  string `json:"baseCcy"`
	QuoteCcy string `json:"quoteCcy"`
	OptType  string `json:"optType"` // C | P
	Uly      string `json:"uly"`     // BTC-USD（指数底用，与期权最小单位一致）
}

// DCDProduct 在售产品。
// 字段语义（官方文档核对）：
//   - absYield     绝对收益率（小数，如 0.00232413）
//   - annualizedYield 年化收益率（decimal 小数，如 0.0541 = 5.41%）
//   - notionalCcy  投资币种：optType=C → baseCcy（币本位高卖）；optType=P → quoteCcy（USDT 低买）
//   - uly          与期权的底层指数一致（BTC-USD），可直接对齐期权链
type DCDProduct struct {
	ProductID           string  `json:"productId"` // BTC-USDT-260327-54500-P
	BaseCcy             string  `json:"baseCcy"`
	QuoteCcy            string  `json:"quoteCcy"`
	NotionalCcy         string  `json:"notionalCcy"`
	OptType             string  `json:"optType"` // C=看涨(高卖) | P=看跌(低买)
	Uly                 string  `json:"uly"`     // BTC-USD
	Strike              float64 `json:"strike"`
	AbsYield            float64 `json:"absYield"`
	AnnualizedYield     float64 `json:"annualizedYield"`
	MinSize             float64 `json:"minSize"`
	MaxSize             float64 `json:"maxSize"`
	StepSz              float64 `json:"stepSz"`
	ListTime            int64   `json:"listTime"`            // ms
	ExpTime             int64   `json:"expTime"`             // 到期 ms
	InterestAccrualTime int64   `json:"interestAccrualTime"` // 计息开始 ms
	RedeemStartTime     int64   `json:"redeemStartTime"`     // 提前赎回开始 ms
	RedeemEndTime       int64   `json:"redeemEndTime"`
	TradeEndTime        int64   `json:"tradeEndTime"` // 申购截止 ms
	QuoteTime           int64   `json:"quoteTime"`    // 报价生成 ms
}

// DCDQuoteRequest 报价请求。
type DCDQuoteRequest struct {
	ProductID   string  `json:"productId"`
	NotionalSz  float64 `json:"notionalSz,string"`
	NotionalCcy string  `json:"notionalCcy"`
}

// DCDQuote 报价响应（quoteId 有效期内才能下单）。
type DCDQuote struct {
	QuoteID             string  `json:"quoteId"`
	ProductID           string  `json:"productId"`
	AbsYield            float64 `json:"absYield,string"`
	AnnualizedYield     float64 `json:"annualizedYield,string"`
	NotionalSz          string  `json:"notionalSz"`
	NotionalCcy         string  `json:"notionalCcy"`
	InterestAccrualTime int64   `json:"interestAccrualTime,string"`
	ValidUntil          int64   `json:"validUntil,string"`
	IdxPx               string  `json:"idxPx"`
}

// GetDCDCurrencyPairs 可用币对。
func (c *Client) GetDCDCurrencyPairs() ([]DCDCurrencyPair, error) {
	return wrap[DCDCurrencyPair](c.get(ctx(), pathDCDCurrencyPairs, nil))
}

// GetDCDProducts 在售产品（baseCcy/quoteCcy/optType 均必填）。
// 限速 1次/s，注意调用方自带节流。
func (c *Client) GetDCDProducts(baseCcy, quoteCcy, optType string) ([]DCDProduct, error) {
	q := url.Values{
		"baseCcy":  {baseCcy},
		"quoteCcy": {quoteCcy},
		"optType":  {optType},
	}
	return wrap[DCDProduct](c.get(ctx(), pathDCDProducts, q))
}

// PostDCDQuote 阶段一：请求实时报价（quoteId 有有效期）。
func (c *Client) PostDCDQuote(req DCDQuoteRequest) ([]DCDQuote, error) {
	q, err := c.postDecode[DCDQuote](pathDCDQuote, req)
	if err != nil {
		return nil, err
	}
	return []DCDQuote{*q}, nil
}

// PostDCDTrade 阶段二：用有效 quoteId 下单。
func (c *Client) PostDCDTrade(quoteID string) ([]DCDResult, error) {
	body := map[string]string{"quoteId": quoteID}
	r, err := c.postDecode[DCDResult](pathDCDTrade, body)
	if err != nil {
		return nil, err
	}
	return []DCDResult{*r}, nil
}

// GetDCDOrderStatus 订单实时状态。
func (c *Client) GetDCDOrderStatus(ordID string) ([]DCDResult, error) {
	q := url.Values{"ordId": {ordID}}
	return wrap[DCDResult](c.get(ctx(), pathDCDOrderStatus, q))
}

// GetDCDOrderHistory 历史订单（state 可空）。
func (c *Client) GetDCDOrderHistory(ordID, productID, uly, state string, limit int) ([]DCDOrder, error) {
	q := url.Values{}
	if ordID != "" {
		q.Set("ordId", ordID)
	}
	if productID != "" {
		q.Set("productId", productID)
	}
	if uly != "" {
		q.Set("uly", uly)
	}
	if state != "" {
		q.Set("state", state)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	return wrap[DCDOrder](c.get(ctx(), pathDCDOrderHistory, q))
}

// DCDOrder 历史订单（持仓页/结算重放用）。
type DCDOrder struct {
	OrdID           string `json:"ordId"`
	QuoteID         string `json:"quoteId"`
	State           string `json:"state"`
	ProductID       string `json:"productId"`
	BaseCcy         string `json:"baseCcy"`
	QuoteCcy        string `json:"quoteCcy"`
	Uly             string `json:"uly"`
	Strike          string `json:"strike"`
	NotionalSz      string `json:"notionalSz"`
	NotionalCcy     string `json:"notionalCcy"`
	AbsYield        string `json:"absYield"`
	AnnualizedYield string `json:"annualizedYield"`
	YieldSz         string `json:"yieldSz"`
	YieldCcy        string `json:"yieldCcy"`
	SettleSz        string `json:"settleSz"`
	SettleCcy       string `json:"settleCcy"`
	SettlePx        string `json:"settlePx"`
	ExpTime         int64  `json:"expTime,string"`
	CTime           int64  `json:"cTime,string"`
	UTime           int64  `json:"uTime,string"`
}
