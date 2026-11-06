package okx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGetCandlesAscending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathCandles {
			t.Errorf("path = %s, want %s", r.URL.Path, pathCandles)
		}
		if r.URL.Query().Get("bar") != "1D" {
			t.Errorf("bar = %q, want 1D", r.URL.Query().Get("bar"))
		}
		// OKX 新→旧；GetCandles 应反转为旧→新。
		fmt.Fprint(w, `{"code":"0","data":[
			["1700086400000","1","2","0.5","1.5","0","0","0","1"],
			["1700000000000","1","2","0.5","1.2","0","0","0","1"]
		]}`)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	cs, err := c.GetCandles("BTC-USDT", "1D", 2)
	if err != nil {
		t.Fatalf("GetCandles: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("len = %d, want 2", len(cs))
	}
	if cs[0].Close != 1.2 || cs[1].Close != 1.5 {
		t.Fatalf("not ascending: %+v", cs)
	}
	if !cs[0].Confirm || !cs[1].Confirm {
		t.Fatalf("confirm not parsed: %+v", cs)
	}
}

// 历史 K 线分页：`after` 向前翻页、结果升序、按 startMs 截断。
func TestGetCandlesHistoryPagination(t *testing.T) {
	const dayMs = int64(86400000)
	base := int64(1_760_000_000_000) // 固定基准，避免时钟影响
	var reqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		limit := 300
		if s := r.URL.Query().Get("limit"); s != "" {
			limit, _ = strconv.Atoi(s)
		}
		end := base
		if after := r.URL.Query().Get("after"); after != "" {
			v, _ := strconv.ParseInt(after, 10, 64)
			end = v - dayMs // 严格早于 after
		}
		var rows []string
		for i := 0; i < limit && end-int64(i)*dayMs > base-3000*dayMs; i++ {
			ts := end - int64(i)*dayMs
			rows = append(rows, fmt.Sprintf(`["%d","100","101","99","%.1f","0","0","0","1"]`, ts, 100.0+float64(i%7)))
		}
		fmt.Fprintf(w, `{"code":"0","data":[%s]}`, strings.Join(rows, ","))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	cs, err := c.GetCandlesHistory("BTC-USDT", "1D", 0, 650)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 650 {
		t.Fatalf("根数 = %d，期望 650", len(cs))
	}
	for i := 1; i < len(cs); i++ {
		if cs[i].Ts <= cs[i-1].Ts {
			t.Fatalf("结果未按时间升序: %d 后出现 %d", cs[i-1].Ts, cs[i].Ts)
		}
	}
	if reqs < 2 {
		t.Fatalf("应发生翻页，实际请求 %d 次", reqs)
	}
	// startMs 截断：只要最近 100 根。
	cs, err = c.GetCandlesHistory("BTC-USDT", "1D", base-99*dayMs, 650)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 100 {
		t.Fatalf("按 startMs 截断后 = %d，期望 100", len(cs))
	}
	if cs[0].Ts != base-99*dayMs {
		t.Fatalf("最老一根 = %d，期望 %d", cs[0].Ts, base-99*dayMs)
	}
}
