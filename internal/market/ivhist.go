package market

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// IVHistory 期权隐含波动率的历史采样（IV 分位的数据源）。
//
// 为什么需要它：单看「IV 高于 RV 5pp」只能说明相对已实现波动率不亏，
// 不能说明现在卖 put 的绝对价格处在什么位置。同样的 40% IV，在 30%
// 分位和 90% 分位对应的赔率完全不同。分位需要时间序列，而 OKX 不提供
// 历史 IV，只能自己按小时采样。
//
// 存储为 JSONL（与 store 一致，可直接 jq 查看），最多每小时一条/标的，
// 加载时只保留最近 ivKeepDays 天。采样是 best-effort：写失败不影响行情。
type IVHistory struct {
	path string

	mu     sync.Mutex
	loaded bool
	rows   map[string][]ivSample
	lastAt map[string]time.Time
	pct    map[string]float64 // 上次计算的分位缓存（避免 500ms 重组时反复排序）
	warn   string
}

type ivSample struct {
	Ts  time.Time `json:"ts"`
	Uly string    `json:"uly"`
	IV  float64   `json:"iv"`
}

const (
	ivKeepDays   = 180
	ivSampleRate = 55 * time.Minute // 每小时一条（略小于 1h，避免抖动漏采）
	ivMinSamples = 40
	ivMinSpan    = 7 * 24 * time.Hour
	ivLookback   = 90 * 24 * time.Hour
)

// NewIVHistory 创建采样器；path 为 JSONL 文件路径（空 = 只在内存中，不落盘）。
func NewIVHistory(path string) *IVHistory {
	return &IVHistory{
		path:   path,
		rows:   make(map[string][]ivSample, 2),
		lastAt: make(map[string]time.Time, 2),
		pct:    make(map[string]float64, 2),
	}
}

// Warn 最近一次加载/写入错误（展示给用户，空 = 正常）。
func (h *IVHistory) Warn() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.warn
}

// Observe 记录一个 IV 样本（按标的限速 1 条/小时），返回**加入样本前**的分位。
//
// 返回 -1 表示样本不足（不要据此决策）；否则为 0~100。
func (h *IVHistory) Observe(now time.Time, uly string, iv float64) float64 {
	if h == nil || uly == "" || iv <= 0 {
		return -1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked(now)
	hist := h.rows[uly]
	if last, ok := h.lastAt[uly]; !ok || now.Sub(last) >= ivSampleRate {
		s := ivSample{Ts: now, Uly: uly, IV: iv}
		hist = append(hist, s)
		h.rows[uly] = hist
		h.lastAt[uly] = now
		h.pct[uly] = percentileLocked(hist, now, iv)
		h.appendLocked(s)
	}
	if p, ok := h.pct[uly]; ok {
		return p
	}
	// 首次查询已有历史：算一次并缓存。
	h.pct[uly] = percentileLocked(hist, now, iv)
	return h.pct[uly]
}

// Percentile 不写样本，只查询当前值在历史中的分位（0~100；-1=样本不足）。
func (h *IVHistory) Percentile(now time.Time, uly string, iv float64) float64 {
	if h == nil || uly == "" || iv <= 0 {
		return -1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked(now)
	return percentileLocked(h.rows[uly], now, iv)
}

// Count 当前标的的样本数（诊断/展示）。
func (h *IVHistory) Count(uly string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked(time.Now())
	return len(h.rows[uly])
}

func percentileLocked(rows []ivSample, now time.Time, iv float64) float64 {
	cut := now.Add(-ivLookback)
	vals := make([]float64, 0, len(rows))
	oldest := now
	for _, r := range rows {
		if r.Ts.Before(cut) || r.Ts.After(now.Add(time.Minute)) {
			continue
		}
		vals = append(vals, r.IV)
		if r.Ts.Before(oldest) {
			oldest = r.Ts
		}
	}
	if len(vals) < ivMinSamples || now.Sub(oldest) < ivMinSpan {
		return -1
	}
	sort.Float64s(vals)
	le := sort.SearchFloat64s(vals, iv) // 第一个 >= iv 的位置
	for le < len(vals) && vals[le] <= iv {
		le++
	}
	return float64(le) / float64(len(vals)) * 100
}

// loadLocked 懒加载：只读 ivKeepDays 内的样本，按时间升序。
func (h *IVHistory) loadLocked(now time.Time) {
	if h.loaded {
		return
	}
	h.loaded = true
	if h.path == "" {
		return
	}
	f, err := os.Open(h.path)
	if err != nil {
		if !os.IsNotExist(err) {
			h.warn = "读取 IV 样本失败: " + err.Error()
		}
		return
	}
	defer f.Close()
	cut := now.Add(-ivKeepDays * 24 * time.Hour)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	var bad int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s ivSample
		if err := json.Unmarshal([]byte(line), &s); err != nil || s.IV <= 0 || s.Ts.Before(cut) {
			bad++
			continue
		}
		h.rows[s.Uly] = append(h.rows[s.Uly], s)
		if s.Ts.After(h.lastAt[s.Uly]) {
			h.lastAt[s.Uly] = s.Ts
		}
	}
	if err := sc.Err(); err != nil {
		h.warn = "读取 IV 样本失败: " + err.Error()
	}
	if bad > 0 && h.warn == "" {
		h.warn = "部分 IV 样本无法解析（已跳过）"
	}
}

// appendLocked 追加一行（best-effort）。
func (h *IVHistory) appendLocked(s ivSample) {
	if h.path == "" {
		return
	}
	bb, err := json.Marshal(s)
	if err != nil {
		return
	}
	if dir := filepath.Dir(h.path); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		h.warn = "写入 IV 样本失败: " + err.Error()
		return
	}
	defer f.Close()
	if _, err := f.Write(append(bb, '\n')); err != nil {
		h.warn = "写入 IV 样本失败: " + err.Error()
	}
}
