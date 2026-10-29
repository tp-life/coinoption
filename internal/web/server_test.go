package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/strategy"
)

type fakeFetcher struct {
	calls int32
	snap  *market.Snapshot
}

func (f *fakeFetcher) FetchSnapshot(string) (*market.Snapshot, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.snap, nil
}

func testSnap() *market.Snapshot {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 8, 0, 0, 0, time.UTC) }
	return &market.Snapshot{
		At:           time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
		Uly:          "BTC-USDT",
		OptionUly:    "BTC-USD",
		SpotPrice:    80000,
		OptionCount:  3,
		DcdCount:     2,
		DCYAvailable: true,
		Decisions: []strategy.Decision{
			{
				Row: strategy.MatchRow{
					ProductID: "BTC-USDT-261106-80000-C", ExpTime: day(6),
					OptInstID: "BTC-USD-261106-80000-C", OptMatched: true,
					Strike: 80000, DcyAPR: 0.12, OptAPR: 0.35, Diff: 0.23,
					Days: 8, StrikeErrPct: 0.1, InvestCcy: "USDT",
				},
				Action:       strategy.ActionBuyOption,
				ReasonCode:   "option_apr_higher",
				SuggestedAmt: 2000,
			},
			{
				Row:          strategy.MatchRow{ProductID: "BTC-USDT-261113-78000-C", ExpTime: day(13), DcyAPR: 0.2, OptAPR: 0.22, Diff: 0.02, Days: 15},
				Action:       strategy.ActionSubscribeDual,
				ReasonCode:   "diff_small",
				SuggestedAmt: 500,
			},
		},
		OptionRows: []strategy.OptionRow{
			{InstID: "BTC-USD-261106-79000-C", OptAPR: 0.4, Strike: 79000, Days: 8},
			{InstID: "BTC-USD-261113-77000-C", OptAPR: 0.3, Strike: 77000, Days: 15},
		},
	}
}

func newTestServer(t *testing.T, f SnapshotFetcher) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(f, "BTC-USDT", "").Handler)
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

func TestIndexPageContainsControls(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d", code)
	}
	html := string(body)
	for _, want := range []string{
		`data-uly="BTC-USDT"`,
		`data-uly="ETH-USDT"`,
		`data-uly="SOL-USDT"`,
		`id="exp"`,
		`id="expClear"`,
		"到期(UTC)",
		"CALL 看涨",
		"PUT 看跌",
		"卖出期权收权利金",
		"<th>方向</th>",
		"<th>操作</th>",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("首页缺少 %q", want)
		}
	}
}

func TestIndexDefaultUlyFromFlag(t *testing.T) {
	ts := httptest.NewServer(New(&fakeFetcher{snap: testSnap()}, "ETH-USDT", "").Handler)
	t.Cleanup(ts.Close)
	_, body := get(t, ts.URL+"/")
	if !strings.Contains(string(body), `let curUly = "ETH-USDT"`) {
		t.Fatalf("首页初始标的应跟随 --uly 参数")
	}
}

func TestAPIDecisionsCamelCaseKeys(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/decisions")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}

	var j struct {
		SpotPrice    float64 `json:"spotPrice"`
		DcdCount     int     `json:"dcdCount"`
		DCYAvailable bool    `json:"dcyAvailable"`
		Decisions    []struct {
			Action       string  `json:"action"`
			ReasonCode   string  `json:"reasonCode"`
			SuggestedAmt float64 `json:"suggestedAmt"`
			Row          struct {
				ProductID string    `json:"productId"`
				DcyAPR    float64   `json:"dcyApr"`
				OptAPR    float64   `json:"optApr"`
				Diff      float64   `json:"diff"`
				ExpTime   time.Time `json:"expTime"`
			} `json:"row"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if j.SpotPrice != 80000 || j.DcdCount != 2 || !j.DCYAvailable {
		t.Fatalf("快照字段异常: %+v", j)
	}
	if len(j.Decisions) != 2 {
		t.Fatalf("decisions = %d", len(j.Decisions))
	}
	d := j.Decisions[0]
	if d.Action != "buy_option" || d.ReasonCode != "option_apr_higher" || d.SuggestedAmt != 2000 {
		t.Fatalf("决策字段异常（camelCase 契约）: %+v", d)
	}
	if d.Row.ProductID == "" || d.Row.DcyAPR != 0.12 || d.Row.OptAPR != 0.35 || d.Row.Diff != 0.23 {
		t.Fatalf("row 字段异常（camelCase 契约）: %+v", d.Row)
	}
	if !d.Row.ExpTime.Equal(time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("expTime = %v", d.Row.ExpTime)
	}
}

func TestAPIExpiryFilter(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/decisions?exp=2026-11-06")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}
	var j struct {
		Decisions  []json.RawMessage `json:"decisions"`
		OptionRows []json.RawMessage `json:"optionRows"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(j.Decisions) != 1 || len(j.OptionRows) != 1 {
		t.Fatalf("按 11-06 过滤后 decisions=%d optionRows=%d", len(j.Decisions), len(j.OptionRows))
	}
}

func TestAPIInvalidExpiry(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, _ := get(t, ts.URL+"/api/decisions?exp=not-a-date")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 exp 应返回 400，实际 %d", code)
	}
}

func TestAPIUsesCache(t *testing.T) {
	ff := &fakeFetcher{snap: testSnap()}
	ts := newTestServer(t, ff)
	for i := 0; i < 3; i++ {
		if code, body := get(t, ts.URL+"/api/decisions"); code != http.StatusOK {
			t.Fatalf("第 %d 次请求失败 %d: %s", i+1, code, body)
		}
	}
	if got := atomic.LoadInt32(&ff.calls); got != 1 {
		t.Fatalf("TTL 缓存内应只拉取 1 次，实际 %d 次", got)
	}
}

func TestAPIUlyOverride(t *testing.T) {
	var gotUly string
	f := fetcherFunc(func(uly string) (*market.Snapshot, error) {
		gotUly = uly
		s := testSnap()
		s.Uly = uly
		return s, nil
	})
	ts := newTestServer(t, f)
	code, body := get(t, ts.URL+"/api/decisions?uly=ETH-USDT")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}
	if gotUly != "ETH-USDT" {
		t.Fatalf("fetcher 收到 uly=%q", gotUly)
	}
}

type fetcherFunc func(string) (*market.Snapshot, error)

func (f fetcherFunc) FetchSnapshot(uly string) (*market.Snapshot, error) { return f(uly) }
