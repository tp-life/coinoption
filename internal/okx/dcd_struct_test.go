package okx

import (
	"encoding/json"
	"testing"
)

// 真实响应形态（2026-10-27 实测）：全字符串数值 + envelopes。
func TestRealDCDProductShape(t *testing.T) {
	raw := []byte(`{"products":[{"productId":"BTC-USDT-260331-82000-P","absYield":"0.00326059","annualizedYield":"0.242","expTime":"1793520000000","minSize":"10","maxSize":"6000000","strike":"82000","optType":"P","uly":"BTC-USD"}]}`)
	var obj struct {
		Products []DCDProduct `json:"products"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := obj.Products[0]
	if p.ProductID != "BTC-USDT-260331-82000-P" || float64(p.AnnualizedYield) != 0.242 ||
		float64(p.Strike) != 82000 || int64(p.ExpTime) != 1793520000000 {
		t.Fatalf("fields: %+v", p)
	}
	// 裸数字形态也要兼容
	var q DCDProduct
	if err := json.Unmarshal([]byte(`{"annualizedYield":0.242,"expTime":1793520000000}`), &q); err != nil {
		t.Fatalf("numeric form: %v", err)
	}
}
