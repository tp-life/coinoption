package market

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 样本不足 → 分位 -1（不得据此否决）；样本足够后按历史分布返回 0~100。
func TestIVHistoryPercentile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iv_samples.jsonl")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	// 30 天前开始，每天 4 条，IV 在 0.3~0.6 之间均匀分布。
	var b strings.Builder
	n := 0
	for d := 30; d >= 1; d-- {
		for h := 0; h < 4; h++ {
			iv := 0.30 + 0.30*float64(n%40)/40
			n++
			fmt.Fprintf(&b, `{"ts":%q,"uly":"BTC-USDT","iv":%v}`+"\n",
				now.Add(-time.Duration(d)*24*time.Hour+time.Duration(h)*time.Hour).Format(time.RFC3339Nano), iv)
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewIVHistory(path)
	pct := h.Observe(now, "BTC-USDT", 0.45)
	if pct < 40 || pct > 60 {
		t.Fatalf("分位 = %.1f，期望在 0.45 附近（40~60）", pct)
	}
	if h.Count("BTC-USDT") < 100 {
		t.Fatalf("样本数 = %d，期望 ≥100", h.Count("BTC-USDT"))
	}
	// 未知标的：样本不足。
	if got := h.Percentile(now, "ETH-USDT", 0.5); got != -1 {
		t.Fatalf("未知标的分位 = %v，期望 -1", got)
	}
}

// 采样限速：一小时内多次 Observe 只落一条；新实例能读回。
func TestIVHistoryRateLimitAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iv.jsonl")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	h := NewIVHistory(path)
	for i := 0; i < 5; i++ {
		h.Observe(now.Add(time.Duration(i)*time.Minute), "BTC-USDT", 0.4+0.01*float64(i))
	}
	lines := countLines(t, path)
	if lines != 1 {
		t.Fatalf("一小时内应只落 1 条，实际 %d", lines)
	}
	// 超过 55 分钟 → 再落一条。
	h.Observe(now.Add(70*time.Minute), "BTC-USDT", 0.42)
	if got := countLines(t, path); got != 2 {
		t.Fatalf("跨窗口后应有 2 条，实际 %d", got)
	}

	h2 := NewIVHistory(path)
	if got := h2.Count("BTC-USDT"); got != 2 {
		t.Fatalf("重启后应读回 2 条，实际 %d", got)
	}
	if h2.Warn() != "" {
		t.Fatalf("不应有告警: %s", h2.Warn())
	}
}

// 老样本（>180 天）加载时被丢弃。
func TestIVHistoryDropsOldSamples(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iv.jsonl")
	now := time.Now().UTC()
	old := now.Add(-400 * 24 * time.Hour).Format(time.RFC3339Nano)
	cur := now.Add(-time.Hour).Format(time.RFC3339Nano)
	os.WriteFile(path, []byte(fmt.Sprintf(
		`{"ts":%q,"uly":"BTC-USDT","iv":0.5}`+"\n"+`{"ts":%q,"uly":"BTC-USDT","iv":0.6}`+"\n", old, cur)), 0o600)
	h := NewIVHistory(path)
	if got := h.Count("BTC-USDT"); got != 1 {
		t.Fatalf("应只保留 1 条有效样本，实际 %d", got)
	}
}

// 分位计算对边界值单调。
func TestIVHistoryPercentileMonotonic(t *testing.T) {
	h := NewIVHistory("")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		h.Observe(now.Add(-time.Duration(60-i)*3*time.Hour), "BTC-USDT", 0.3+0.005*float64(i))
	}
	lo := h.Percentile(now, "BTC-USDT", 0.32)
	hi := h.Percentile(now, "BTC-USDT", 0.58)
	if lo < 0 || hi < 0 || hi <= lo {
		t.Fatalf("分位应随 IV 单调上升: lo=%.1f hi=%.1f", lo, hi)
	}
	if math.Abs(lo-5) > 20 {
		t.Fatalf("lo = %.1f，期望接近低分位", lo)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}
