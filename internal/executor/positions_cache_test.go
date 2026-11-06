package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
)

// 持仓视图 5 秒内复用缓存：连续两次 Positions 只打一轮平台接口。
func TestPositionsCached(t *testing.T) {
	var n int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/finance/sfp/dcd/order-history", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		io.WriteString(w, `{"code":"0","data":{"orders":[]}}`)
	})
	mux.HandleFunc("/api/v5/account/positions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		io.WriteString(w, `{"code":"0","data":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ex := New(ModeLive, okx.NewClient(okx.WithBaseURL(srv.URL)), NewGuard(DefaultLimits()), st, nil)

	if _, err := ex.Positions(); err != nil {
		t.Fatalf("Positions: %v", err)
	}
	first := atomic.LoadInt32(&n)
	if first != 2 {
		t.Fatalf("首轮平台请求 = %d，期望 2（DCY 历史 + 期权仓位）", first)
	}
	if _, err := ex.Positions(); err != nil {
		t.Fatalf("Positions(2): %v", err)
	}
	if got := atomic.LoadInt32(&n); got != first {
		t.Fatalf("缓存未命中：请求数 %d → %d", first, got)
	}
	// 下单/结算后必须失效，下一次重新取数。
	ex.invalidatePositions()
	if _, err := ex.Positions(); err != nil {
		t.Fatalf("Positions(3): %v", err)
	}
	if got := atomic.LoadInt32(&n); got == first {
		t.Fatal("invalidatePositions 后应重新请求")
	}
}
