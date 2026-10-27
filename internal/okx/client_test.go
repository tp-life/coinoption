package okx

import (
	"encoding/json"
	"testing"
)

func Test50105Hint(t *testing.T) {
	e := &APIError{Code: "50105", Msg: "Request header OK-ACCESS-PASSPHRASE incorrect."}
	s := e.Error()
	if want := "Passphrase"; !contains(s, want) {
		t.Fatalf("hint missing in %q", s)
	}
}

func Test50106Hint(t *testing.T) {
	e := &APIError{Code: "50106", Msg: "API Key IP restriction"}
	if s := e.Error(); !contains(s, "白名单") {
		t.Fatalf("IP hint missing in %q", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// wrapKeys 三种响应形态回归（源自 2026-10-27 实测响应头片段）。
func TestWrapShapes(t *testing.T) {
	// 形态1：标准数组
	d1 := json.RawMessage(`[{"instId":"BTC-USDT","last":"79803"}]`)
	spots, err := wrap[SpotTicker](d1, nil)
	if err != nil || len(spots) != 1 || spots[0].Last != "79803" {
		t.Fatalf("array shape: %v %+v", err, spots)
	}

	// 形态2：{"products":[...]}（实测的双币赢响应）
	d2 := json.RawMessage(`{"code":0,"data":{"products":[{"productId":"BTC-USDT-260331-82000-P","annualizedYield":"0.242"}]}}`)
	b := resp{Data: d2}
	_ = b
	prods, err := wrapKeys[DCDProduct](json.RawMessage(`{"products":[{"productId":"BTC-USDT-260331-82000-P","annualizedYield":"0.242"}]}`), nil, "products")
	if err != nil || len(prods) != 1 || prods[0].AnnualizedYield != 0.242 {
		t.Fatalf("products shape: %v %+v", err, prods)
	}

	// flexCode：数字 0 与字符串 "0" 都算成功
	var r1 resp
	if err := json.Unmarshal(json.RawMessage(`{"code":0}`), &r1); err != nil || !r1.Code.OK() {
		t.Fatalf("numeric code: %v %q", err, r1.Code)
	}
	var r2 resp
	if err := json.Unmarshal(json.RawMessage(`{"code":"0"}`), &r2); err != nil || !r2.Code.OK() {
		t.Fatalf("string code: %v", err)
	}

	// flexCode：错误码字符串保留
	var r3 resp
	if err := json.Unmarshal(json.RawMessage(`{"code":50105,"msg":"x"}`), &r3); err != nil || r3.Code.OK() || r3.Code != "50105" {
		t.Fatalf("numeric error code: %v %q", err, r3.Code)
	}
}
