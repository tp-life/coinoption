// Package web 提供 Web 展示服务：GET / 为决策看板页，GET /api/decisions 返回 JSON。
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

// SnapshotFetcher 拉取指定标的的快照（*market.Fetcher 实现；便于测试注入）。
type SnapshotFetcher interface {
	FetchSnapshot(uly string) (*market.Snapshot, error)
}

// Trader Web 手动下单能力（*executor.Executor 实现；nil = 只读看板）。
// 只暴露执行器已有的入口，风控/审计仍由 executor 强制校验。
type Trader interface {
	Mode() executor.Mode
	State() executor.GuardState
	SubmitOption(instID string, sz, limPx float64) (executor.Outcome, error)
	SubmitOptionChecked(instID string, sz, limPx, minAPR float64) (executor.Outcome, error)
	SubmitDCD(p okx.DCDProduct, amt float64) (executor.Outcome, error)
	Positions() (executor.PositionsView, error)
}

// SettingsStore Web 设置页的读写入口（*store.Store 实现）。
// nil = 不提供设置页（例如只读嵌入场景）。
type SettingsStore interface {
	LoadSettings() (store.Settings, error)
	SaveSettings(store.Settings) error
}

// ServerOption 可选能力（手动下单 / 自动执行 / 设置 / 授权码）。
type ServerOption func(*serverConfig)

type serverConfig struct {
	trader Trader
	autoOn bool // 已装配自动执行引擎（是否初始启用由 autoDefault 决定）
	auto   *autoEngine

	autoDefault bool
	autoParams  strategy.Params

	settings SettingsStore
	// settingsMu 串行化多标签页同时保存设置（读-改-写不丢字段）。
	settingsMu *sync.Mutex
	auth       *authenticator // nil = 不启用授权码（仅测试/内嵌场景）
	// failures 自动失败记录持久化（nil = 只保留进程内最近 10 条）。
	failures FailureStore
}

// WithTrader 启用 Web 手动下单（不传 = 只读，与旧行为一致）。
func WithTrader(t Trader) ServerOption {
	return func(c *serverConfig) { c.trader = t }
}

// WithAutoTrade 装配自动执行引擎；initial 决定启动时是否立即交易
// （持久化的 autoTrade 设置决定；live 模式启动时强制关闭）。
func WithAutoTrade(initial bool) ServerOption {
	return func(c *serverConfig) {
		c.autoOn = true
		c.autoDefault = initial
	}
}

// WithAutoParams 自动执行使用的策略参数（与 TUI settings 对齐）。
func WithAutoParams(p strategy.Params) ServerOption {
	return func(c *serverConfig) { c.autoParams = p }
}

// WithAutoFailures 自动失败记录的持久化（*store.Store 实现）。
// 不传 = 只保留进程内最近 10 条，重启后清空。
func WithAutoFailures(s FailureStore) ServerOption {
	return func(c *serverConfig) { c.failures = s }
}

// WithSettingsStore 启用 Web 设置页（资金总额 / 差值比例 / 策略阈值 / 授权码修改）。
func WithSettingsStore(s SettingsStore) ServerOption {
	return func(c *serverConfig) { c.settings = s }
}

// WithAuthCode 整站启用授权码（明文；启动时由 main 传入 --web-code 或随机生成值）。
func WithAuthCode(code string) ServerOption {
	return func(c *serverConfig) {
		if strings.TrimSpace(code) == "" {
			return
		}
		a := newAuthenticator()
		if err := a.setHash(HashCode(code)); err == nil {
			c.auth = a
		}
	}
}

// WithAuthHash 用 settings.json 里持久化的授权码摘要启用整站授权。
// 摘要非法时保持零摘要（任何授权码都登录不了）——宁可锁死，也不能裸奔。
func WithAuthHash(hashHex string) ServerOption {
	return func(c *serverConfig) {
		a := newAuthenticator()
		_ = a.setHash(hashHex) // 失败 → 零摘要 → check 恒为 false
		c.auth = a
	}
}

// decisionsResponse 在快照字段之上补充执行器状态（保持原有字段平铺）。
type decisionsResponse struct {
	*market.Snapshot
	Mode          string               `json:"mode"`
	TradeEnabled  bool                 `json:"tradeEnabled"`
	ManualTrade   bool                 `json:"manualTrade"`
	AutoAvailable bool                 `json:"autoAvailable"`
	AutoTrade     bool                 `json:"autoTrade"`
	AutoNote      string               `json:"autoNote,omitempty"`
	AutoCount     int                  `json:"autoCount"`
	AutoFailures  []store.AutoFailure  `json:"autoFailures"`
	AuthEnabled   bool                 `json:"authEnabled"`
	Guard         *executor.GuardState `json:"guard,omitempty"`
}

// settingsResponse 设置页/接口返回：用户可调项 + 生效中的风控上限。
// 授权码只回 authEnabled（是否启用），绝不回传摘要或明文。
type settingsResponse struct {
	CapitalUSDT     float64              `json:"capitalUsdt"`
	MinDiffPct      float64              `json:"minDiffPct"`
	MinOptionEdgePP float64              `json:"minOptionEdgePp"`
	MinAbsEdgePct   float64              `json:"minAbsEdgePct"`
	MaxStrikeGapPct float64              `json:"maxStrikeGapPct"`
	AutoTrade       bool                 `json:"autoTrade"`
	AutoAvailable   bool                 `json:"autoAvailable"`
	AuthEnabled     bool                 `json:"authEnabled"`
	Limits          executor.Limits      `json:"limits"`
	Guard           *executor.GuardState `json:"guard,omitempty"`
	Relogin         bool                 `json:"relogin,omitempty"`
}

// orderResponse 下单回执（camelCase，避免直接序列化 executor.Outcome 的字段名）。
type orderResponse struct {
	OK        bool    `json:"ok"`
	Mode      string  `json:"mode"`
	Kind      string  `json:"kind"`
	Ref       string  `json:"ref"`
	OrdID     string  `json:"ordId,omitempty"`
	QuoteID   string  `json:"quoteId,omitempty"`
	Amt       float64 `json:"amt"`
	InvestCcy string  `json:"investCcy,omitempty"`
	AmtUSDT   float64 `json:"amtUsdt"`
	Premium   float64 `json:"premium,omitempty"`
	Sim       bool    `json:"sim"`
	Detail    string  `json:"detail"`
}

// cacheTTL 快照缓存时长：WS 实时模式只缓存 1s，避免页面/WS 推送重复重组；
// REST 模式下 market.Fetcher 内部另有 15s 缓存，不会放大 OKX 请求量。
const cacheTTL = 1 * time.Second

type cacheEntry struct {
	snap *market.Snapshot
	at   time.Time
}

// cachedFetcher 带 TTL 的快照缓存 + 同标的合并请求。
//
// 注意：锁只保护缓存/在途表，拉行情时**不持锁**——否则市场接口卡住时，
// 设置保存（SetParams 需要清缓存）和下单都会一起被阻塞。
type cachedFetcher struct {
	f       SnapshotFetcher
	mu      sync.Mutex
	entries map[string]cacheEntry
	calls   map[string]*inflightCall
}

type inflightCall struct {
	done chan struct{}
	snap *market.Snapshot
	err  error
}

func (c *cachedFetcher) snapshot(uly string) (*market.Snapshot, error) {
	c.mu.Lock()
	if e, ok := c.entries[uly]; ok && time.Since(e.at) < cacheTTL {
		c.mu.Unlock()
		return e.snap, nil
	}
	if call, ok := c.calls[uly]; ok {
		c.mu.Unlock()
		<-call.done
		return call.snap, call.err
	}
	if c.calls == nil {
		c.calls = make(map[string]*inflightCall, 4)
	}
	call := &inflightCall{done: make(chan struct{})}
	c.calls[uly] = call
	c.mu.Unlock()

	snap, err := c.f.FetchSnapshot(uly)

	c.mu.Lock()
	call.snap, call.err = snap, err
	if err == nil {
		c.entries[uly] = cacheEntry{snap: snap, at: time.Now()}
	}
	delete(c.calls, uly)
	close(call.done)
	c.mu.Unlock()
	return snap, err
}

// SetParams 设置页保存后让行情层按新阈值重算，并清空快照缓存，
// 下一次请求（≤1s）立即看到新参数下的决策。在途请求不会被强杀。
func (c *cachedFetcher) SetParams(p strategy.Params) {
	if ps, ok := c.f.(interface{ SetParams(strategy.Params) }); ok {
		ps.SetParams(p)
	}
	c.mu.Lock()
	c.entries = make(map[string]cacheEntry, 4)
	c.mu.Unlock()
}

// indexHTML 决策看板页（内嵌，无需外部静态文件）。
const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>coinoption · DCY × 期权 决策台</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Archivo:wght@500;600;700;800;900&family=Hanken+Grotesk:wght@400;500;600;700&family=Space+Mono:wght@400;700&display=swap" rel="stylesheet">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/icons-webfont@3/dist/tabler-icons.min.css">
<style>
:root {
  --mint:#CFE8D6; --ink:#121210; --paper:#FFFFFF;
  --green:#2E8C42; --amber:#E8A030; --red:#C03A3A; --amber-stripe:#F4D89A;
  --line:3px solid var(--ink);
  --shadow-sm:3px 3px 0 var(--ink);
  --shadow:4px 4px 0 var(--ink);
  --shadow-lg:6px 6px 0 var(--ink);
  --shadow-hover:8px 8px 0 var(--ink);
  --font-display:'Archivo','Hanken Grotesk',-apple-system,sans-serif;
  --font-body:'Hanken Grotesk',-apple-system,'PingFang SC',sans-serif;
  --font-mono:'Space Mono',ui-monospace,SFMono-Regular,Menlo,monospace;
  --ease:cubic-bezier(.2,.7,.2,1);
  --dur:120ms;
}
* { box-sizing:border-box; }
html, body { height:100%; }
body { margin:0; background:var(--mint); color:var(--ink); font-family:var(--font-body); font-size:13px; line-height:1.5; -webkit-font-smoothing:antialiased; }
button, input, select { font-family:inherit; color:inherit; }
button { cursor:pointer; }
.mono, .font-mono { font-family:var(--font-mono); font-variant-numeric:tabular-nums; }
.font-display { font-family:var(--font-display); letter-spacing:-.01em; }
.hidden { display:none !important; }
::selection { background:var(--ink); color:var(--mint); }
.queue-list::-webkit-scrollbar, .detail::-webkit-scrollbar { width:10px; height:10px; }
.queue-list::-webkit-scrollbar-track, .detail::-webkit-scrollbar-track { background:var(--mint); }
.queue-list::-webkit-scrollbar-thumb, .detail::-webkit-scrollbar-thumb { background:var(--ink); }

/* ── brutalist primitives（与参考同名，同一套数值）── */
.brut { border:3px solid var(--ink); box-shadow:var(--shadow); }
.brut-sm { border:2px solid var(--ink); box-shadow:var(--shadow-sm); }
.brut-lg { border:3px solid var(--ink); box-shadow:var(--shadow-lg); }
.brut-card { border:3px solid var(--ink); box-shadow:var(--shadow); background:var(--paper); }
.btn-press:active { transform:translate(4px,4px); box-shadow:0 0 0 var(--ink) !important; }
.tag { display:inline-flex; align-items:center; gap:6px; padding:4px 10px; border:2px solid var(--ink); font-weight:600; font-size:11px; letter-spacing:.06em; text-transform:uppercase; line-height:1.4; white-space:nowrap; }
.tag-paper { background:var(--paper); }
.tag-green { background:var(--green); color:var(--paper); }
.tag-amber { background:var(--amber); }
.tag-red { background:var(--red); color:var(--paper); }
.tag-mint { background:var(--mint); }
.tag-muted { background:var(--paper); opacity:.6; }
.mono-label { font-family:var(--font-mono); font-size:10px; letter-spacing:.12em; text-transform:uppercase; opacity:.6; }
.chain-node { width:26px; height:26px; border:2px solid var(--ink); border-radius:999px; display:inline-flex; align-items:center; justify-content:center; font-size:10px; font-weight:700; flex:0 0 auto; }
.chain-line { width:16px; height:3px; background:var(--ink); flex:0 0 auto; }
.n-green { background:var(--green); color:var(--paper); }
.n-amber { background:var(--amber); }
.n-red { background:var(--red); color:var(--paper); }
.n-white { background:var(--paper); opacity:.5; }
.n-mint { background:var(--mint); }

/* ── app shell ── */
.app { display:flex; height:100vh; overflow:hidden; }

.sidebar { width:260px; flex:0 0 260px; border-right:var(--line); padding:20px; display:flex; flex-direction:column; gap:8px; background:var(--mint); }
.brand { display:flex; align-items:center; gap:10px; margin-bottom:20px; }
.brand-mark { width:44px; height:44px; background:var(--green); color:var(--paper); display:flex; align-items:center; justify-content:center; font-size:22px; }
.brand-name { font-family:var(--font-display); font-weight:800; font-size:18px; line-height:1; letter-spacing:.02em; }
.brand-sub { font-family:var(--font-mono); font-size:10px; letter-spacing:.16em; opacity:.6; margin-top:3px; }
.side-label { font-family:var(--font-mono); font-size:10px; letter-spacing:.16em; opacity:.5; padding:0 2px; margin-bottom:2px; }
.side-gap { margin-top:18px; }
.nav-item { display:flex; align-items:center; justify-content:space-between; gap:10px; width:100%; padding:11px 12px; background:var(--paper); border:3px solid var(--ink); box-shadow:var(--shadow); font-family:var(--font-display); font-weight:600; font-size:14px; text-align:left; transition:transform var(--dur) var(--ease), box-shadow var(--dur) var(--ease); }
.nav-item:hover { transform:translate(-1px,-1px); box-shadow:5px 5px 0 var(--ink); }
.nav-item:active { transform:translate(3px,3px); box-shadow:0 0 0 var(--ink); }
.nav-item.is-active { background:var(--ink); color:var(--paper); }
.nav-left { display:flex; align-items:center; gap:10px; }
.nav-left i { font-size:18px; }
.nav-badge { background:var(--ink); color:var(--paper); font-family:var(--font-mono); font-size:11px; padding:1px 7px; }
.nav-item.is-active .nav-badge { background:var(--paper); color:var(--ink); }
.nav-badge.is-muted { background:transparent; color:var(--ink); opacity:.6; }
.nav-item.is-active .nav-badge.is-muted { background:transparent; color:var(--paper); }
.side-card { background:var(--paper); padding:12px; }
.stat-row { display:flex; align-items:flex-end; gap:8px; margin-top:4px; }
.stat-num { font-family:var(--font-mono); font-weight:700; font-size:30px; line-height:1; }
.stat-sub { font-size:11px; opacity:.6; margin-bottom:3px; }
.stat-bar { display:flex; height:12px; border:2px solid var(--ink); margin-top:8px; overflow:hidden; background:var(--paper); }
.stat-bar .seg { display:block; height:100%; width:0; transition:width .3s var(--ease); }
.seg-green { background:var(--green); } .seg-amber { background:var(--amber); } .seg-red { background:var(--red); }
.stat-legend { display:flex; justify-content:space-between; gap:6px; margin-top:8px; font-family:var(--font-mono); font-size:10px; opacity:.75; }
.stat-legend b { font-weight:700; }
.side-status { display:flex; flex-direction:column; gap:6px; }
.side-status .trade-mode { display:flex; align-items:center; gap:6px; font-family:var(--font-mono); font-size:12px; }
.side-status .trade-mode b { font-weight:700; letter-spacing:.04em; }
.side-status .mode-note { font-size:10px; opacity:.65; }
.side-status.is-live { background:var(--red); color:var(--paper); }
.side-status.is-live .mono-label, .side-status.is-live .mode-note { opacity:.85; }
.dot { width:8px; height:8px; border:2px solid var(--ink); border-radius:999px; display:inline-block; }
.dot-green { background:var(--green); } .dot-amber { background:var(--amber); } .dot-red { background:var(--red); }
.side-status.is-live .dot { border-color:var(--paper); }
.side-note { font-size:10px; opacity:.6; line-height:1.5; }
.auto-row { display:flex; align-items:center; justify-content:space-between; gap:6px; margin-top:6px; }
.fail-list { display:flex; flex-direction:column; gap:10px; max-height:52vh; overflow-y:auto; margin-top:12px; padding-right:2px; }
.fail-item { border:2px solid var(--ink); background:var(--paper); box-shadow:var(--shadow-sm); padding:10px 12px; }
.fail-head { display:flex; align-items:center; gap:8px; flex-wrap:wrap; font-family:var(--font-mono); font-size:10px; }
.fail-head .tag { font-size:9px; padding:1px 6px; }
.fail-time { opacity:.6; }
.fail-ref { opacity:.75; word-break:break-all; }
.fail-count { margin-left:auto; border:2px solid var(--ink); padding:0 5px; font-weight:700; }
.fail-reason { margin-top:6px; font-size:12px; line-height:1.5; word-break:break-word; }
.fail-detail { margin-top:4px; font-family:var(--font-mono); font-size:10px; line-height:1.5; opacity:.55; word-break:break-word; }
.fail-empty { border:3px dashed rgba(18,18,16,.35); padding:22px 16px; text-align:center; font-family:var(--font-mono); font-size:11px; line-height:1.8; opacity:.75; }
.side-foot { margin-top:auto; font-family:var(--font-mono); font-size:10px; letter-spacing:.04em; opacity:.5; line-height:1.6; }

.main { flex:1; min-width:0; display:flex; flex-direction:column; min-height:0; }

/* ── top bar ── */
.topbar { border-bottom:var(--line); padding:16px 24px; display:flex; align-items:flex-start; justify-content:space-between; gap:16px; background:var(--mint); }
.title-row { display:flex; align-items:center; gap:10px; flex-wrap:wrap; }
h1 { margin:0; font-family:var(--font-display); font-weight:800; font-size:30px; line-height:1.05; letter-spacing:-.02em; }
.meta { font-size:11px; opacity:.65; margin-top:5px; }
.clock { font-size:11px; letter-spacing:.12em; opacity:.55; margin-top:3px; }
.top-actions { display:flex; gap:10px; align-items:center; flex-shrink:0; }
.search { display:flex; align-items:center; gap:8px; background:var(--paper); padding:8px 10px; width:300px; }
.search i { font-size:16px; opacity:.7; }
.search input { flex:1; min-width:0; border:0; background:transparent; font-size:12px; outline:none; }
.search input::placeholder { color:rgba(18,18,16,.45); }
.kbd { font-family:var(--font-mono); font-size:10px; border:2px solid var(--ink); padding:0 4px; line-height:1.5; }
.btn-black { background:var(--ink); color:var(--paper); border:3px solid var(--ink); box-shadow:var(--shadow); padding:9px 14px; font-family:var(--font-display); font-weight:700; font-size:13px; letter-spacing:.04em; display:inline-flex; align-items:center; gap:8px; transition:transform var(--dur) var(--ease), box-shadow var(--dur) var(--ease); }
.btn-black:hover { transform:translate(-2px,-2px); box-shadow:var(--shadow-lg); }
.btn-black:active { transform:translate(4px,4px); box-shadow:0 0 0 var(--ink); }
.flash { padding:8px 24px; font-family:var(--font-mono); font-size:11px; border-bottom:var(--line); background:var(--paper); }
.flash.is-ok { background:#E6F3E9; }
.flash.is-err { background:var(--red); color:var(--paper); }

/* ── tabs ── */
.tabs { display:flex; align-items:flex-end; gap:0; padding:12px 24px 0; border-bottom:var(--line); background:var(--mint); }
.tab { font-family:var(--font-display); font-weight:600; font-size:14px; padding:8px 20px; border:3px solid var(--ink); border-bottom:0; background:var(--paper); margin-bottom:-3px; transition:background var(--dur) var(--ease), color var(--dur) var(--ease); }
.tab + .tab { border-left:0; }
.tab:hover:not(.is-active) { background:var(--amber-stripe); }
.tab.is-active { background:var(--ink); color:var(--paper); }
.tabs-note { margin-left:18px; margin-bottom:10px; font-family:var(--font-mono); font-size:10px; letter-spacing:.04em; opacity:.6; }
.status-mono { margin-left:auto; margin-bottom:10px; font-family:var(--font-mono); font-size:10px; letter-spacing:.04em; color:var(--red); font-weight:700; }

/* ── split ── */
.split { flex:1; display:flex; min-height:0; }
.queue { width:44%; flex:0 0 44%; border-right:var(--line); display:flex; flex-direction:column; min-height:0; }
.queue-head { padding:12px 20px; border-bottom:var(--line); display:flex; flex-direction:column; gap:8px; background:var(--mint); }
.chip-row { display:flex; align-items:center; gap:6px; flex-wrap:wrap; }
.chip-sep { width:1px; height:16px; background:rgba(18,18,16,.25); margin:0 4px; }
.chip { border:2px solid var(--ink); background:var(--paper); padding:1px 8px; font-family:var(--font-mono); font-size:10px; letter-spacing:.02em; line-height:1.8; transition:transform var(--dur) var(--ease), box-shadow var(--dur) var(--ease), background var(--dur) var(--ease); }
.chip:hover { transform:translate(-1px,-1px); box-shadow:2px 2px 0 var(--ink); }
.chip:active { transform:translate(2px,2px); box-shadow:0 0 0 var(--ink); }
.chip.on { background:var(--ink); color:var(--paper); }
.chip:disabled { opacity:.4; cursor:default; transform:none; box-shadow:none; }
.day-strip { display:inline-flex; gap:4px; flex-wrap:wrap; }
.day { font-size:10px; }
.day.on { background:var(--amber); }
.date-filter { display:inline-flex; align-items:center; gap:4px; margin-left:auto; }
.date-filter input { border:2px solid var(--ink); background:var(--paper); font-family:var(--font-mono); font-size:10px; padding:0 6px; line-height:1.75; width:118px; }
.sort-row { display:flex; align-items:center; gap:6px; }
.sort-row .mono-label { margin-right:2px; }
.sort-dir { font-size:10px; }
.queue-count { margin-left:auto; font-size:10px; opacity:.55; }
.chain-cols { display:grid; grid-template-columns:minmax(0,1fr) 74px 64px 58px; gap:0; padding:6px 20px; border-bottom:2px solid var(--ink); background:var(--paper); font-family:var(--font-mono); font-size:9px; letter-spacing:.1em; opacity:.6; }
.chain-cols span:not(:first-child) { text-align:right; }
.queue-list { flex:1; overflow-y:auto; padding:20px; display:flex; flex-direction:column; gap:16px; background:var(--mint); }

/* ── request cards ── */
.req-card { position:relative; padding:16px; background:var(--paper); border:3px solid var(--ink); box-shadow:var(--shadow); cursor:pointer; transition:transform var(--dur) var(--ease), box-shadow var(--dur) var(--ease); }
.req-card:hover { transform:translate(-2px,-2px); box-shadow:var(--shadow-hover); }
.req-card.is-selected { box-shadow:var(--shadow-lg); transform:translate(-2px,-2px); }
.req-card.is-selected::before { content:""; position:absolute; left:-3px; top:-3px; bottom:-3px; width:8px; background:var(--ink); }
.req-id { position:absolute; top:-11px; left:-3px; border:2px solid var(--ink); padding:0 8px; font-family:var(--font-mono); font-size:10px; font-weight:700; letter-spacing:.04em; line-height:1.8; background:var(--paper); }
.req-id-green { background:var(--green); color:var(--paper); }
.req-id-amber { background:var(--amber); }
.req-id-red { background:var(--red); color:var(--paper); }
.req-top { display:flex; justify-content:space-between; align-items:flex-start; gap:12px; }
.req-main { min-width:0; }
.req-name { font-family:var(--font-display); font-weight:700; font-size:20px; line-height:1.15; }
.req-sub { font-size:12px; opacity:.7; margin-top:2px; }
.req-amt { text-align:right; flex-shrink:0; }
.req-num { font-family:var(--font-mono); font-weight:700; font-size:24px; line-height:1; }
.req-num.is-muted { opacity:.45; }
.req-unit { font-family:var(--font-mono); font-size:10px; opacity:.55; margin-top:3px; }
.req-tags { display:flex; gap:6px; flex-wrap:wrap; margin-top:12px; }
.req-foot { display:flex; justify-content:space-between; align-items:flex-end; gap:10px; margin-top:12px; }
.req-meta { display:flex; flex-direction:column; gap:2px; min-width:0; }
.req-opt { font-size:12px; white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
.req-bid { font-size:11px; opacity:.6; }
.chain { display:flex; align-items:center; flex-shrink:0; }

/* ── chain rows ── */
.chain-row { display:grid; grid-template-columns:minmax(0,1fr) 74px 64px 58px; align-items:center; gap:0; width:100%; padding:8px 10px; background:var(--paper); border:2px solid var(--ink); box-shadow:var(--shadow-sm); font-family:var(--font-mono); font-size:11px; text-align:left; transition:transform var(--dur) var(--ease), box-shadow var(--dur) var(--ease); }
.chain-row:hover { transform:translate(-2px,-2px); box-shadow:var(--shadow); }
.chain-row.is-selected { background:var(--ink); color:var(--paper); }
.chain-row .cr-name { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.chain-row .cr-cell { text-align:right; }
.chain-row .is-muted { opacity:.4; }

/* ── detail ── */
.detail { flex:1; min-width:0; overflow-y:auto; padding:24px; background-image:linear-gradient(rgba(18,18,16,.05) 1px,transparent 1px),linear-gradient(90deg,rgba(18,18,16,.05) 1px,transparent 1px); background-size:28px 28px; }
.doc-layout { display:flex; gap:20px; align-items:flex-start; }
.doc { flex:1; min-width:0; background:var(--paper); border:3px solid var(--ink); box-shadow:var(--shadow-lg); padding:28px; min-height:560px; position:relative; overflow:hidden; }
.doc.anim { animation:docIn .32s var(--ease) both; }
.doc-head { display:flex; justify-content:space-between; align-items:flex-start; gap:16px; border-bottom:3px solid var(--ink); padding-bottom:16px; }
.doc-title { font-size:30px; font-weight:800; line-height:1.05; letter-spacing:-.02em; }
.doc-sub { font-family:var(--font-mono); font-size:11px; opacity:.65; margin-top:6px; word-break:break-all; }
.doc-right { text-align:right; flex-shrink:0; }
.doc-amount { font-family:var(--font-mono); font-size:22px; font-weight:700; line-height:1.2; }
.doc-note { font-family:var(--font-mono); font-size:10px; opacity:.55; margin-top:2px; }
.doc-grid3 { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); gap:16px; margin-top:20px; }
.info-label { font-family:var(--font-mono); font-size:10px; letter-spacing:.12em; opacity:.55; text-transform:uppercase; }
.info-value { font-family:var(--font-display); font-weight:700; font-size:16px; line-height:1.25; margin-top:2px; word-break:break-all; }
.info-sub { font-size:11px; opacity:.65; margin-top:2px; word-break:break-word; }
.info-value.is-contract { font-family:var(--font-mono); font-size:13px; font-weight:700; line-height:1.4; word-break:break-all; }
.info-value.is-num { font-family:var(--font-mono); font-size:15px; font-weight:700; white-space:nowrap; }
.doc-table { width:100%; border-collapse:collapse; margin-top:22px; font-family:var(--font-mono); font-size:13px; }
.doc-table th { font-size:10px; letter-spacing:.1em; text-transform:uppercase; text-align:left; padding:8px 0; border-top:3px solid var(--ink); border-bottom:3px solid var(--ink); font-weight:700; }
.doc-table th.ta-r, .doc-table td.ta-r { text-align:right; }
.doc-table td { padding:8px 0; border-bottom:1px solid rgba(18,18,16,.2); vertical-align:middle; }
.doc-table td.note { font-family:var(--font-body); font-size:11px; opacity:.6; }
.doc-table td.val { font-weight:700; }
.doc-chain { margin-top:24px; border-top:3px solid var(--ink); padding-top:16px; }
.doc-chain .chain { gap:8px; flex-wrap:wrap; margin-top:10px; }
.chain-item { display:flex; align-items:center; gap:6px; }
.chain-box { border:2px solid var(--ink); padding:6px 10px; background:var(--paper); font-family:var(--font-mono); font-size:11px; line-height:1.35; }
.chain-box b { font-family:var(--font-display); font-size:12px; }
.chain-box.is-green { background:var(--green); color:var(--paper); }
.chain-box.is-amber { background:var(--amber); }
.chain-box.is-dashed { border-style:dashed; opacity:.6; }
.chain-arrow { font-size:15px; opacity:.6; }
.big-stamp { position:absolute; top:36px; right:36px; font-family:var(--font-display); font-weight:700; font-size:60px; letter-spacing:.1em; border:8px solid currentColor; padding:12px 28px; border-radius:6px; pointer-events:none; animation:stampDrop .5s var(--ease) both; z-index:2; }
@keyframes stampDrop { 0% { opacity:0; transform:scale(2.6) rotate(-18deg); } 60% { opacity:.9; transform:scale(.96) rotate(-7deg); } 100% { opacity:.85; transform:scale(1) rotate(-7deg); } }
@keyframes docIn { from { opacity:0; transform:translateY(12px); } to { opacity:1; transform:none; } }

/* ── rail ── */
.rail { width:176px; flex:0 0 176px; position:sticky; top:0; display:flex; flex-direction:column; gap:16px; }
.rail-label { margin-bottom:2px; }
.stamp-btn { display:flex; flex-direction:column; align-items:center; justify-content:center; gap:6px; width:100%; padding:16px 10px; border:3px solid var(--ink); box-shadow:var(--shadow); background:var(--paper); font-family:var(--font-display); font-weight:700; font-size:14px; letter-spacing:.06em; transition:transform var(--dur) var(--ease), box-shadow var(--dur) var(--ease), opacity var(--dur) var(--ease); }
.stamp-btn i { font-size:26px; }
.stamp-btn:hover:not(:disabled) { transform:translate(-2px,-2px); box-shadow:var(--shadow-lg); }
.stamp-btn:active:not(:disabled) { transform:translate(4px,4px); box-shadow:0 0 0 var(--ink); }
.stamp-btn:disabled { opacity:.42; cursor:not-allowed; box-shadow:var(--shadow-sm); }
.stamp-btn.is-green { background:var(--green); color:var(--paper); }
.stamp-btn.is-amber { background:var(--amber); }
.stamp-btn.is-red { background:var(--red); color:var(--paper); }
.stamp-key { font-family:var(--font-mono); font-size:10px; letter-spacing:0; text-transform:none; opacity:.8; font-weight:400; }
.policy { background:var(--paper); border:3px solid var(--ink); box-shadow:var(--shadow); padding:12px; }
.policy-row { display:flex; justify-content:space-between; gap:8px; font-size:11px; margin-top:6px; }
.policy-row span:first-child { opacity:.6; }
.policy-row b { font-family:var(--font-mono); font-weight:700; }
.policy-row b.is-green { color:var(--green); }
.policy-row b.is-amber { color:#A56A0C; }
.rail-hint { font-family:var(--font-mono); font-size:10px; line-height:1.5; opacity:.55; }

/* ── log ── */
.log-card { margin-top:24px; background:var(--ink); color:var(--paper); border:3px solid var(--ink); box-shadow:var(--shadow); padding:18px 20px; }
.log-head { display:flex; align-items:center; justify-content:space-between; gap:12px; margin-bottom:10px; }
.log-title { font-family:var(--font-display); font-weight:700; font-size:16px; display:flex; align-items:center; gap:8px; }
.log-note { font-family:var(--font-mono); font-size:10px; opacity:.55; letter-spacing:.06em; }
.log-body { font-family:var(--font-mono); font-size:12px; line-height:1.8; white-space:pre-wrap; word-break:break-word; opacity:.9; }

/* ── empty ── */
.empty { border:3px dashed rgba(18,18,16,.35); padding:28px 20px; text-align:center; font-family:var(--font-mono); font-size:11px; opacity:.8; }
.empty b { display:block; font-family:var(--font-display); font-size:15px; margin-bottom:4px; }
.doc.is-empty { min-height:340px; background:transparent; border:3px dashed rgba(18,18,16,.35); box-shadow:none; display:flex; align-items:center; justify-content:center; padding:36px; }
.empty-doc-big { text-align:center; max-width:420px; }
.empty-doc-big b { display:block; font-family:var(--font-display); font-size:22px; line-height:1.25; margin:8px 0 10px; }
.empty-doc-big p { margin:0; font-family:var(--font-mono); font-size:11px; line-height:1.9; opacity:.65; }

/* ── modal ── */
.modal { position:fixed; inset:0; background:rgba(18,18,16,.55); display:flex; align-items:center; justify-content:center; z-index:20; padding:20px; }
.modal.hidden { display:none !important; }
.dialog { background:var(--mint); border:3px solid var(--ink); box-shadow:8px 8px 0 var(--ink); padding:20px; width:min(520px,94vw); max-height:90vh; overflow:auto; }
.dialog-head { display:flex; justify-content:space-between; align-items:center; border-bottom:3px solid var(--ink); padding-bottom:10px; margin-bottom:12px; }
.dialog-head h3 { margin:0; font-family:var(--font-display); font-size:18px; font-weight:700; }
.icon-btn { border:2px solid var(--ink); background:var(--paper); width:28px; height:28px; display:flex; align-items:center; justify-content:center; }
.ominfo { font-family:var(--font-mono); font-size:11px; line-height:1.9; margin-bottom:12px; }
.omfield { display:flex; gap:10px; align-items:center; font-family:var(--font-mono); font-size:11px; }
.omfield span { flex:0 0 90px; opacity:.65; }
.omfield input { flex:1; background:var(--paper); border:2px solid var(--ink); padding:8px; font-family:var(--font-mono); font-size:14px; outline:none; }
.omnote { margin-top:12px; font-family:var(--font-mono); font-size:11px; line-height:1.6; opacity:.75; }
.omnote.err { color:var(--red); opacity:1; font-weight:700; }
.ombtns { display:flex; justify-content:flex-end; gap:10px; margin-top:16px; }
.ombtns button { border:3px solid var(--ink); padding:8px 16px; font-family:var(--font-display); font-weight:700; font-size:12px; box-shadow:var(--shadow-sm); background:var(--paper); }
.ombtns button.primary { background:var(--ink); color:var(--paper); }
.ombtns button:disabled { opacity:.45; cursor:default; }
.ombtns button:active:not(:disabled) { transform:translate(3px,3px); box-shadow:0 0 0 var(--ink); }
.dialog-wide { width:min(700px,96vw); }
.set-grid { display:grid; grid-template-columns:1fr 1fr; gap:18px 26px; align-items:start; }
.set-section { display:flex; flex-direction:column; gap:8px; min-width:0; }
.set-section .omfield span { flex:0 0 92px; }
.set-hint { font-family:var(--font-mono); font-size:10px; line-height:1.65; opacity:.6; }
.set-mono { font-family:var(--font-mono); font-size:11px; line-height:1.9; white-space:pre-line; }
.set-section .chip { align-self:flex-start; }
.set-section #setLogout { background:var(--paper); }
#authFoot { cursor:pointer; text-decoration:underline; text-underline-offset:2px; }
@media (max-width:700px) { .set-grid { grid-template-columns:1fr; } }

/* ── focus ── */
button:focus-visible { outline:2px solid var(--ink); outline-offset:2px; }
input:focus-visible { outline:none; box-shadow:inset 0 0 0 2px var(--ink); }
.search:focus-within { box-shadow:6px 6px 0 var(--ink); transform:translate(-2px,-2px); }

/* ── responsive（参考在 1350px 折叠右侧动作栏）── */
@media (max-width:1400px) {
  .sidebar { width:220px; flex-basis:220px; }
  .queue { width:40%; flex-basis:40%; }
  .search { width:240px; }
  .doc { padding:22px; }
  .doc-title { font-size:26px; }
  .req-name { font-size:18px; }
  .req-num { font-size:21px; }
}
@media (max-width:1480px) {
  .doc-layout { flex-direction:column; }
  .rail { width:100%; flex:none; position:static; display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:12px; }
  .rail .policy { grid-column:1 / -1; }
}
@media (max-width:1180px) {
  .split { flex-direction:column; }
  .queue { width:100%; flex:none; border-right:0; border-bottom:var(--line); max-height:52vh; }
  .detail { overflow:visible; }
}
@media (max-width:1100px) {
  .app { height:auto; overflow:visible; flex-direction:column; }
  .sidebar { width:100%; flex:none; border-right:0; border-bottom:var(--line); padding:14px 16px; }
  .brand { margin-bottom:10px; }
  .sidebar .side-label, .sidebar .side-card { display:none; }
  .sidebar .nav-item { flex:1; }
  .sidebar { flex-direction:row; flex-wrap:wrap; align-items:center; }
  .main { min-height:0; }
  .split { flex-direction:column; }
  .queue { width:100%; flex:none; border-right:0; border-bottom:var(--line); max-height:70vh; }
  .detail { overflow:visible; }
  .topbar { flex-direction:column; }
  .search { width:100%; }
  .top-actions { width:100%; }
  .doc { min-height:0; }
  .doc-grid3 { grid-template-columns:1fr 1fr; }
}
@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after { animation:none !important; transition:none !important; }
}
</style>
</head>
<body>
<div class="app">
  <aside class="sidebar">
    <div class="brand">
      <div class="brand-mark brut-sm"><i class="ti ti-coin"></i></div>
      <div>
        <div class="brand-name">COINOPTION</div>
        <div class="brand-sub">v2.4 · DCY / OPT</div>
      </div>
    </div>
    <div class="side-label">WORKSPACE</div>
    <button class="nav-item is-active" data-tab="decisions">
      <span class="nav-left"><i class="ti ti-layout-list"></i>对比决策</span>
      <span class="nav-badge" id="navDecN">0</span>
    </button>
    <button class="nav-item" data-tab="chain">
      <span class="nav-left"><i class="ti ti-list-details"></i>完整期权链</span>
      <span class="nav-badge is-muted" id="navChainN">0</span>
    </button>
    <button class="nav-item" id="settingsNav" type="button">
      <span class="nav-left"><i class="ti ti-adjustments-horizontal"></i>设置</span>
      <span class="nav-badge is-muted" id="navSetMark"><i class="ti ti-lock"></i></span>
    </button>
    <div class="side-label side-gap">SESSION</div>
    <div class="side-card brut">
      <div class="mono-label">本轮可执行</div>
      <div class="stat-row"><span class="stat-num" id="statExec">0</span><span class="stat-sub" id="statOf">of 0 决策</span></div>
      <div class="stat-bar"><i class="seg seg-green" id="segExec"></i><i class="seg seg-amber" id="segSub"></i><i class="seg seg-red" id="segUnaligned"></i></div>
      <div class="stat-legend"><span>卖期权 <b id="statBuy">0</b></span><span>申购 <b id="statSub">0</b></span><span>待对齐 <b id="statUnal">0</b></span></div>
    </div>
    <div class="side-card brut side-status" id="tradeBar"></div>
    <div class="side-foot">
      <div>仅卖期权 · CALL 看涨 / PUT 看跌</div>
      <div id="sideFoot">OKX v5 · 仅供研究，不构成投资建议</div>
      <div id="authFoot" class="hidden"></div>
    </div>
  </aside>

  <main class="main">
    <header class="topbar">
      <div class="title-block">
        <div class="title-row">
          <h1 id="pageTitle">对比决策</h1>
          <span class="tag tag-paper" id="chipExec">0 可执行</span>
          <span class="tag tag-red" id="chipUnaligned">0 不可对齐</span>
          <span class="tag tag-red hidden" id="warnTag" title="">⚠ 数据告警</span>
        </div>
        <div class="meta mono" id="meta">加载中…</div>
        <div class="clock" id="clock">—</div>
      </div>
      <div class="top-actions">
        <div class="search brut">
          <i class="ti ti-search"></i>
          <input id="search" placeholder="搜索合约 / 产品 / 行权价…" autocomplete="off" spellcheck="false">
          <span class="kbd">⌘K</span>
        </div>
        <button class="btn-black btn-press" id="refreshBtn"><i class="ti ti-refresh"></i><span>刷新</span></button>
      </div>
    </header>
    <div class="flash hidden" id="flash"></div>
    <nav class="tabs">
      <button class="tab is-active" data-tab="decisions">对比决策</button>
      <button class="tab" data-tab="chain">完整期权链</button>
      <span class="tabs-note mono" id="tabNote"></span>
      <span class="status-mono" id="status"></span>
    </nav>
    <div class="split">
      <section class="queue">
        <div class="queue-head">
          <div class="chip-row">
            <span class="mono-label">标的</span>
            <button class="chip cur" data-uly="BTC-USDT">BTC</button>
            <button class="chip cur" data-uly="ETH-USDT">ETH</button>
            <button class="chip cur" data-uly="SOL-USDT">SOL</button>
            <span class="chip-sep"></span>
            <span class="mono-label">方向</span>
            <button class="chip side" data-side="">全部</button>
            <button class="chip side" data-side="C">CALL</button>
            <button class="chip side" data-side="P">PUT</button>
            <span class="chip-sep"></span>
            <span class="mono-label">本位</span>
            <button class="chip settle" data-settle="">全部</button>
            <button class="chip settle" data-settle="B">币本位</button>
            <button class="chip settle" data-settle="U">U本位</button>
          </div>
          <div class="chip-row">
            <span class="mono-label">到期</span>
            <span class="day-strip" id="days"></span>
            <span class="date-filter"><input type="date" id="exp"><button class="chip" id="expApply">筛选</button><button class="chip" id="expClear">全部</button></span>
          </div>
          <div class="sort-row">
            <span class="mono-label">SORT</span>
            <button class="chip sort" data-sort="">默认</button>
            <button class="chip sort" data-sort="price">价格</button>
            <button class="chip sort" data-sort="apr">APR</button>
            <button class="chip sort" data-sort="diff">差%</button>
            <button class="chip sort-dir" id="sortDir" title="切换升序 / 降序">↓ 高→低</button>
            <span class="queue-count" id="queueCount"></span>
          </div>
        </div>
        <div class="chain-cols hidden" id="chainCols"><span>合约</span><span>行权价</span><span>买价</span><span>期权APR</span></div>
        <div class="queue-list" id="queue"></div>
      </section>
      <section class="detail">
        <div class="doc-layout">
          <article class="doc" id="doc"></article>
          <aside class="rail" id="rail"></aside>
        </div>
        <div class="log-card" id="log"></div>
      </section>
    </div>
  </main>
</div>

<div class="modal hidden" id="orderModal">
  <div class="dialog">
    <div class="dialog-head"><h3 id="omTitle">下单</h3><button class="icon-btn" id="omClose" title="关闭"><i class="ti ti-x"></i></button></div>
    <div class="ominfo" id="omInfo"></div>
    <label class="omfield"><span id="omAmtLabel">数量</span><input id="omAmt" type="number" step="any" min="0"></label>
    <div class="omnote" id="omNote"></div>
    <div class="ombtns"><button id="omCancel">取消</button><button id="omSubmit" class="primary">确认下单</button></div>
  </div>
</div>

<div class="modal hidden" id="settingsModal">
  <div class="dialog dialog-wide">
    <div class="dialog-head"><h3>设置</h3><button class="icon-btn" id="setClose" title="关闭"><i class="ti ti-x"></i></button></div>
    <div class="set-grid">
      <div class="set-section">
        <div class="mono-label">资金与自动交易</div>
        <label class="omfield"><span>资金总额 USDT</span><input id="setCapital" type="number" step="100" min="1"></label>
        <div class="set-hint" id="setDerived">单笔上限 = 资金总额 × 40%</div>
        <label class="omfield"><span>差值比例 %</span><input id="setDiff" type="number" step="0.5" min="0" max="1000"></label>
        <div class="set-hint">自动交易要求「期权APR − DCY APR」的绝对值达到该值；0 = 不设门槛。</div>
        <div class="auto-row"><span class="mono-label">自动交易</span><button id="setAuto" class="chip" type="button">停</button></div>
      </div>
      <div class="set-section">
        <div class="mono-label">策略阈值</div>
        <label class="omfield"><span>期权边际 pp</span><input id="setEdge" type="number" step="1" min="1" max="100"></label>
        <label class="omfield"><span>绝对边际 %</span><input id="setAbs" type="number" step="0.1" min="0.01" max="100"></label>
        <label class="omfield"><span>行权价偏差 %</span><input id="setGap" type="number" step="0.1" min="0.01" max="50"></label>
        <div class="set-hint">保存后立即按新阈值重算决策，不需重启。</div>
      </div>
      <div class="set-section">
        <div class="mono-label">风控上限（只读）</div>
        <div class="set-mono" id="setLimits">—</div>
        <div class="set-hint">资金总额 = 总敞口上限；单笔自动按 40% 派生，仍受并发/日限约束。</div>
      </div>
      <div class="set-section" id="setAuthSection">
        <div class="mono-label">访问授权</div>
        <label class="omfield"><span>新授权码</span><input id="setCode" type="password" autocomplete="new-password" placeholder="留空不修改" maxlength="64"></label>
        <div class="set-hint">4–64 位；修改后所有设备（含当前）需重新登录。</div>
        <button class="chip" id="setLogout" type="button">退出登录</button>
      </div>
    </div>
    <div class="omnote" id="setNote"></div>
    <div class="ombtns"><button id="setCancel">取消</button><button id="setSave" class="primary">保存设置</button></div>
  </div>
</div>

<div class="modal hidden" id="autoFailModal">
  <div class="dialog dialog-wide">
    <div class="dialog-head">
      <h3>自动失败记录 <span class="mono-label" id="autoFailCount"></span></h3>
      <button class="icon-btn" id="autoFailClose" title="关闭"><i class="ti ti-x"></i></button>
    </div>
    <div class="omnote">自动执行提交失败 / 行情失败时记录原因，只保留最近 10 条（<span class="font-mono">auto_failures.jsonl</span>，重启后仍可查看）。</div>
    <div class="fail-list" id="autoFailList"></div>
    <div class="ombtns"><button id="autoFailOk" class="primary">关闭</button></div>
  </div>
</div>
<script>

let curUly = "__DEFAULT_ULY__";
let curExp = "";
let curSide = "";
let curSettle = "";
let curTab = "decisions";
let sortKey = "";
let sortDesc = true;
let lastSnap = null;
let selDec = "";
let selChain = "";
let curQuery = "";
let actionCounts = { buy_option: 0, subscribe_dual: 0, unaligned: 0 };
let tradeEnabled = false;
let manualTrade = false;
let autoAvailable = false;
let autoOn = false;
let autoNote = "";
let autoCount = 0;
let autoFailures = [];
let curMode = "read-only";
let curGuard = null;
let pending = null;
let submitting = false;
let loading = false;
let ws = null;
let wsRetry = 0;
let wsTimer = null;
let wsDown = false;
let animateDoc = false;
let authEnabled = false;
let setAutoOn = false;
let setData = null;

const MONTHS = ["JAN","FEB","MAR","APR","MAY","JUN","JUL","AUG","SEP","OCT","NOV","DEC"];
const WEEKDAYS = ["SUN","MON","TUE","WED","THU","FRI","SAT"];

function $(id) { return document.getElementById(id); }
function pad(n) { return String(n).padStart(2, "0"); }
function h(tag, cls, txt) {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (txt !== undefined && txt !== null) el.textContent = txt;
  return el;
}
function tagEl(txt, cls) { return h("span", "tag " + (cls || ""), txt); }
function monoLabel(txt) { return h("div", "mono-label", txt); }

function expStr(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "-";
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) +
    " " + pad(d.getHours()) + ":" + pad(d.getMinutes());
}
function shortDate(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "-";
  return pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes());
}
function timeHM(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "-";
  return pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
}
function tsStr(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "-";
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) +
    " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
}
function instDay(instId) {
  const p = (instId || "").split("-");
  if (p.length < 4 || p[2].length !== 6) return "";
  return "20" + p[2].slice(0, 2) + "-" + p[2].slice(2, 4) + "-" + p[2].slice(4, 6);
}
function pct(v) { return v == null ? "-" : (v * 100).toFixed(2); }
function signed(v) { const d = Number(v) || 0; return (d > 0 ? "+" : "") + d.toFixed(2); }
function fmtNum(v) { if (v == null || isNaN(v)) return "-"; return Math.round(v).toLocaleString("en-US"); }
function shortProduct(pid) {
  const p = (pid || "").split("-");
  return p.length > 2 ? p.slice(2).join("-") : (pid || "");
}
function shortInst(id) {
  if (!id) return "";
  const um = id.indexOf("-USD_UM-");
  if (um >= 0) return "_UM-" + id.slice(um + 7);
  const usd = id.indexOf("-USD-");
  if (usd >= 0) return id.slice(usd + 5);
  return id;
}

function optTypeName(t) {
  const c = typeof t === "number" ? String.fromCharCode(t) : String(t || "");
  const u = c.toUpperCase();
  return u === "C" ? "CALL" : u === "P" ? "PUT" : "-";
}
function settleName(k) { return k === "U" || k === "u" ? "U本位" : k === "B" || k === "b" ? "币本位" : "-"; }
function settleKind(settleCcy, instId) {
  const c = (settleCcy || "").toUpperCase();
  if (c === "USDT" || c === "USDC" || c === "USD") return "U";
  if (c) return "B";
  return (instId || "").indexOf("_UM") >= 0 ? "U" : (instId ? "B" : "");
}
function dcySideName(t) {
  const u = (typeof t === "number" ? String.fromCharCode(t) : String(t || "")).toUpperCase();
  return u === "C" ? "高卖" : u === "P" ? "低买" : "—";
}
function sideLabel(t) {
  const u = (typeof t === "number" ? String.fromCharCode(t) : String(t || "")).toUpperCase();
  return u === "C" ? "高卖 CALL" : u === "P" ? "低买 PUT" : "—";
}
function failKindLabel(k) {
  if (k === "option") return "卖期权";
  if (k === "dcd") return "申购 DCY";
  if (k === "market") return "行情失败";
  return k || "未知";
}
function actionTitle(d) {
  if (d.action === "buy_option") return "卖出 " + optTypeName(d.row && d.row.optType);
  if (d.action === "subscribe_dual") return "申购" + dcySideName(d.row && d.row.optType);
  if (d.action === "unaligned") return "不可对齐";
  return "无动作";
}
function actionTagClass(d) {
  if (d.action === "buy_option") return "tag-green";
  if (d.action === "subscribe_dual") return "tag-amber";
  if (d.action === "unaligned") return "tag-red";
  return "tag-paper";
}
function badgeTone(d) {
  if (d.action === "buy_option") return "req-id-green";
  if (d.action === "subscribe_dual") return "req-id-amber";
  if (d.action === "unaligned") return "req-id-red";
  return "";
}
function reasonTxt(code) {
  switch (code) {
    case "option_apr_higher": return "期权更优";
    case "diff_small": return "差距不足";
    case "option_apr_worse": return "期权更差";
    case "strike_gap_too_big": return "行权价偏差过大";
    case "strike_wrong_side": return "行权价方向异常";
    case "no_option_quote": return "无期权报价";
    case "no_exact_contract": return "无同价同到期合约";
    case "edge_abs_small": return "单笔边际过薄";
    case "out_of_tenor": return "不在期限窗口";
  }
  return code || "-";
}

// ── 排序（客户端；服务端只负责默认顺序）──
function fnum(v) {
  if (v == null || v === "") return NaN;
  const n = parseFloat(String(v));
  return isFinite(n) ? n : NaN;
}
function premiumUSDT(raw, settleCcy, instId, spot) {
  const f = fnum(raw);
  if (!isFinite(f)) return NaN;
  const px = fnum(spot);
  return (settleKind(settleCcy, instId) === "B" && isFinite(px) && px > 0) ? f * px : f;
}
function sortValueFor(kind, x, decByOpt) {
  if (kind === "decisions") {
    const r = x.row || {};
    const hasBid = !!r.optBidPx;
    if (sortKey === "price") return premiumUSDT(r.optBidPx, r.optSettleCcy, r.optInstId, r.spot);
    if (sortKey === "apr") return hasBid ? fnum(r.optApr) : NaN;
    if (sortKey === "diff") return hasBid ? fnum(r.diff) : NaN;
    return NaN;
  }
  if (sortKey === "price") {
    const bid = premiumUSDT(x.bidPx, x.settleCcy, x.instId, x.spotPx);
    return isFinite(bid) ? bid : premiumUSDT(x.lastPx, x.settleCcy, x.instId, x.spotPx);
  }
  if (sortKey === "apr") return x.aprValid ? fnum(x.optApr) : NaN;
  if (sortKey === "diff") {
    if (!x.aprValid) return NaN;
    const d = decByOpt && decByOpt.get(x.instId);
    if (!d || !d.row || d.row.dcyApr == null) return NaN;
    return fnum(x.optApr) - fnum(d.row.dcyApr);
  }
  return NaN;
}
function sortList(kind, list, decByOpt) {
  if (!sortKey || list.length < 2) return list;
  const tagged = list.map(function (x, i) { return { x: x, i: i, v: sortValueFor(kind, x, decByOpt) }; });
  tagged.sort(function (a, b) {
    const aok = isFinite(a.v), bok = isFinite(b.v);
    if (aok !== bok) return aok ? -1 : 1;
    if (aok && a.v !== b.v) return sortDesc ? b.v - a.v : a.v - b.v;
    return a.i - b.i;
  });
  return tagged.map(function (t) { return t.x; });
}
function sortLabel() {
  if (!sortKey) return "默认";
  const n = sortKey === "price" ? "价格" : sortKey === "apr" ? "APR" : "差%";
  return n + (sortDesc ? "↓" : "↑");
}

// ── chrome / meta ──
function wsText() {
  if (wsDown) return "WS 断线";
  if (lastSnap && lastSnap.wsStatus === "degraded") return "WS 降级";
  if (lastSnap && lastSnap.wsConnected) return "WS 实时";
  if (lastSnap && lastSnap.wsStatus === "reconnecting") return "WS 重连中";
  if (lastSnap && lastSnap.wsStatus === "connecting") return "WS 连接中";
  return "REST";
}
function syncChips() {
  document.querySelectorAll(".cur").forEach(function (b) {
    b.classList.toggle("on", b.dataset.uly === curUly);
  });
  document.querySelectorAll(".side").forEach(function (b) {
    b.classList.toggle("on", (b.dataset.side || "") === curSide);
  });
  document.querySelectorAll(".settle").forEach(function (b) {
    b.classList.toggle("on", (b.dataset.settle || "") === curSettle);
  });
}
function syncChrome() {
  const isChain = curTab === "chain";
  $("pageTitle").textContent = isChain ? "完整期权链" : "对比决策";
  document.querySelectorAll(".tab").forEach(function (b) {
    b.classList.toggle("is-active", b.dataset.tab === curTab);
  });
  document.querySelectorAll(".nav-item").forEach(function (b) {
    b.classList.toggle("is-active", b.dataset.tab === curTab);
  });
  $("chainCols").classList.toggle("hidden", !isChain);
  $("tabNote").textContent = isChain
    ? "卖方有效链 · 参考视图 · 右侧可查 DCY 对照"
    : "卖期权 " + actionCounts.buy_option + " · 申购 " + actionCounts.subscribe_dual + " · 待对齐 " + actionCounts.unaligned;
}
function renderTradeBar() {
  const el = $("tradeBar");
  el.innerHTML = "";
  el.classList.toggle("is-live", curMode === "live");
  el.appendChild(monoLabel("MODE / 风控"));
  const row = h("div", "trade-mode");
  row.appendChild(h("i", "dot " + (curMode === "live" ? "dot-red" : (tradeEnabled ? "dot-green" : "dot-amber"))));
  row.appendChild(h("b", "", curMode));
  row.appendChild(h("span", "mode-note", tradeEnabled ? ("Web 可下单" + (manualTrade ? " · 手动" : "")) : "Web 只读"));
  el.appendChild(row);
  if (curGuard && curGuard.limits) {
    el.appendChild(policyRow("单笔", fmtNum(curGuard.limits.perOrderUsdt || 0) + " USDT"));
    el.appendChild(policyRow("敞口", fmtNum(curGuard.exposureUsdt || 0) + "/" + fmtNum(curGuard.limits.totalExposureUsdt || 0)));
    el.appendChild(policyRow("今日", (curGuard.dayCount || 0) + "/" + (curGuard.limits.maxSessionsPerDay || 0), "is-green"));
  }
  if (autoAvailable) {
    const row = h("div", "auto-row");
    row.appendChild(monoLabel("AUTO TRADE"));
    const btn = h("button", "chip" + (autoOn ? " on" : ""), autoOn ? "开" : "停");
    btn.id = "autoToggle";
    btn.type = "button";
    btn.title = autoOn ? "点击暂停自动交易" : "点击开启自动交易";
    btn.addEventListener("click", toggleAuto);
    row.appendChild(btn);
    el.appendChild(row);
    if (autoNote) el.appendChild(h("div", "side-note mono", autoNote.slice(0, 90)));
    if (autoCount > 0) el.appendChild(h("div", "side-note mono", "本次会话自动成交 " + autoCount + " 笔"));
    const failRow = h("div", "auto-row");
    failRow.appendChild(monoLabel("FAIL LOG"));
    const failBtn = h("button", "chip", "失败 " + autoFailures.length);
    failBtn.id = "autoFailBtn";
    failBtn.type = "button";
    failBtn.title = "查看自动失败记录（最近 10 条，落盘 auto_failures.jsonl）";
    failBtn.addEventListener("click", openAutoFailures);
    failRow.appendChild(failBtn);
    el.appendChild(failRow);
  } else {
    el.appendChild(h("div", "side-note", "只读：启动 --web --mode sim-local 即开启交易"));
  }
}

// ── 自动失败记录（最近 10 条，落盘）──
function openAutoFailures() {
  renderAutoFailures();
  $("autoFailModal").classList.remove("hidden");
}
function closeAutoFailures() {
  $("autoFailModal").classList.add("hidden");
}
function renderAutoFailures() {
  const box = $("autoFailList");
  box.innerHTML = "";
  $("autoFailCount").textContent = autoFailures.length + " / 10";
  if (!autoFailures.length) {
    box.appendChild(h("div", "fail-empty", "暂无失败记录。自动执行提交失败或行情拉取失败时会在这里记下原因。"));
    return;
  }
  autoFailures.forEach(function (f) {
    const item = h("div", "fail-item");
    const head = h("div", "fail-head");
    head.appendChild(tagEl(failKindLabel(f.kind), f.kind === "market" ? "tag-amber" : "tag-red"));
    head.appendChild(h("span", "fail-time", tsStr(f.ts)));
    if (f.ref) head.appendChild(h("span", "fail-ref", f.ref));
    if ((f.count || 1) > 1) head.appendChild(h("span", "fail-count", "×" + f.count));
    item.appendChild(head);
    item.appendChild(h("div", "fail-reason", f.reason || "-"));
    if (f.detail) item.appendChild(h("div", "fail-detail", "候选：" + f.detail));
    box.appendChild(item);
  });
}

// 自动交易开关：实时 POST /api/auto，状态随下一次推送刷新。
function toggleAuto() {
  const next = !autoOn;
  if (next && curMode === "live" && !window.confirm("实盘自动交易：会按策略自动提交真实订单，确认开启？")) {
    return;
  }
  fetch("/api/auto", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ enabled: next })
  }).then(function (r) {
    return r.json().then(function (j) { return { ok: r.ok, j: j }; });
  }).then(function (res) {
    if (!res.ok) throw new Error(res.j.error || "自动交易开关请求失败");
    autoOn = !!res.j.enabled;
    autoNote = res.j.note || "";
    setFlash(autoOn ? "⚙ 自动交易已开启" : "⏸ 自动交易已暂停", false);
    renderTradeBar();
  }).catch(function (e) {
    setFlash("✗ " + e.message, true);
  });
}

// ── 设置面板 ──
function setNote(msg, isErr) {
  const el = $("setNote");
  el.className = "omnote" + (isErr ? " err" : "");
  el.textContent = msg || "";
}
function renderAuthFoot() {
  const el = $("authFoot");
  if (!authEnabled) {
    el.classList.add("hidden");
    el.textContent = "";
    el.onclick = null;
    return;
  }
  el.classList.remove("hidden");
  el.textContent = "🔒 已授权 · 点击退出登录";
  el.title = "退出登录（当前设备）";
  el.onclick = logout;
}
function logout() {
  fetch("/api/logout", { method: "POST" })
    .then(function () { location.replace("/login"); })
    .catch(function () { location.replace("/login"); });
}
function openSettings() {
  setNote("");
  fetch("/api/settings", { cache: "no-store" })
    .then(function (r) {
      if (r.status === 401) { location.replace("/login"); throw new Error("会话已失效"); }
      return r.json().then(function (j) { return { ok: r.ok, j: j }; });
    })
    .then(function (res) {
      if (!res.ok) throw new Error(res.j.error || "读取设置失败");
      setData = res.j;
      $("setCapital").value = fmtInput(setData.capitalUsdt);
      $("setDiff").value = fmtInput(setData.minDiffPct);
      $("setEdge").value = fmtInput(setData.minOptionEdgePp);
      $("setAbs").value = fmtInput(setData.minAbsEdgePct);
      $("setGap").value = fmtInput(setData.maxStrikeGapPct);
      $("setCode").value = "";
      setAutoOn = !!setData.autoTrade;
      renderSetAuto();
      renderSetDerived();
      renderSetLimits();
      $("setAuthSection").classList.toggle("hidden", !setData.authEnabled);
      $("settingsModal").classList.remove("hidden");
      $("setCapital").focus();
    })
    .catch(function (e) { setFlash("✗ " + e.message, true); });
}
function fmtInput(v) {
  const n = Number(v);
  if (!isFinite(n)) return "";
  return String(Math.round(n * 10000) / 10000);
}
function renderSetAuto() {
  const b = $("setAuto");
  b.textContent = setAutoOn ? "开" : "停";
  b.classList.toggle("on", setAutoOn);
  b.title = setAutoOn ? "点击暂停自动交易" : "点击开启自动交易";
}
function renderSetDerived() {
  const c = Number($("setCapital").value) || 0;
  const per = Math.round(c * 0.4 * 100) / 100;
  $("setDerived").textContent = c > 0
    ? "单笔上限 = 资金总额 × 40% = " + fmtNum(per) + " USDT"
    : "资金总额必须大于 0";
}
function renderSetLimits() {
  const l = setData && setData.limits;
  if (!l || !l.totalExposureUsdt) {
    $("setLimits").textContent = "未接入执行器（只读看板）：资金总额会保存，重启后生效。";
    return;
  }
  $("setLimits").textContent =
    "单笔 " + fmtNum(l.perOrderUsdt) + " / 总敞口 " + fmtNum(l.totalExposureUsdt) + " USDT\n" +
    "并行 " + l.maxConcurrent + " 笔 · 日限 " + l.maxSessionsPerDay + " 笔";
}
function closeSettings() {
  $("settingsModal").classList.add("hidden");
}
function saveSettings() {
  const capital = Number($("setCapital").value);
  const diff = Number($("setDiff").value);
  const edge = Number($("setEdge").value);
  const abs = Number($("setAbs").value);
  const gap = Number($("setGap").value);
  if (!(capital > 0)) { setNote("资金总额必须为正数", true); return; }
  if (!(diff >= 0)) { setNote("差值比例不能为负数（0 = 不设门槛）", true); return; }
  const body = {
    capitalUsdt: capital,
    minDiffPct: diff,
    minOptionEdgePp: edge,
    minAbsEdgePct: abs,
    maxStrikeGapPct: gap,
    autoTrade: setAutoOn
  };
  const code = $("setCode").value.trim();
  if (code) body.newAuthCode = code;
  const btn = $("setSave");
  btn.disabled = true;
  setNote("保存中…");
  fetch("/api/settings", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body)
  }).then(function (r) {
    if (r.status === 401) { location.replace("/login"); throw new Error("会话已失效"); }
    return r.json().catch(function () { return {}; }).then(function (j) { return { ok: r.ok, j: j }; });
  }).then(function (res) {
    if (!res.ok) throw new Error(res.j.error || "保存失败");
    if (res.j.relogin) {
      location.replace("/login");
      return;
    }
    setData = res.j;
    autoOn = !!res.j.autoTrade;
    autoNote = autoOn ? "自动交易已开启" : "自动交易已暂停";
    closeSettings();
    setFlash("✓ 设置已保存并立即生效", false);
    renderTradeBar();
    refresh();
  }).catch(function (e) {
    setNote("✗ " + e.message, true);
  }).then(function () { btn.disabled = false; });
}

function renderMeta(j) {
  curMode = j.mode || "read-only";
  tradeEnabled = !!j.tradeEnabled;
  manualTrade = !!j.manualTrade;
  autoAvailable = !!j.autoAvailable;
  autoOn = !!j.autoTrade;
  autoNote = j.autoNote || "";
  autoCount = j.autoCount || 0;
  autoFailures = j.autoFailures || [];
  authEnabled = !!j.authEnabled;
  curGuard = j.guard || null;
  const decs = j.decisions || [];
  const opts = j.optionRows || [];
  let buy = 0, sub = 0, unal = 0;
  for (const d of decs) {
    if (d.action === "buy_option") buy++;
    else if (d.action === "subscribe_dual") sub++;
    else if (d.action === "unaligned") unal++;
  }
  actionCounts = { buy_option: buy, subscribe_dual: sub, unaligned: unal };
  $("navDecN").textContent = decs.length;
  $("navChainN").textContent = opts.length;
  $("chipExec").textContent = buy + " 可执行";
  $("chipUnaligned").textContent = unal + " 不可对齐";
  $("chipUnaligned").className = "tag " + (unal > 0 ? "tag-red" : "tag-paper");
  $("statExec").textContent = buy;
  $("statOf").textContent = "of " + decs.length + " 决策";
  $("statBuy").textContent = buy;
  $("statSub").textContent = sub;
  $("statUnal").textContent = unal;
  const total = Math.max(1, buy + sub + unal);
  $("segExec").style.width = (buy / total * 100) + "%";
  $("segSub").style.width = (sub / total * 100) + "%";
  $("segUnaligned").style.width = (unal / total * 100) + "%";
  const meta = (j.uly || "-") + " · 现货 " + fmtNum(j.spotPrice) +
    " · 期权 " + (j.optionCount == null ? "-" : j.optionCount) + " 张" +
    " · DCY " + (j.dcyAvailable ? (j.dcdCount || 0) + " 产品" : "未接入（需 API Key）") +
    " · " + wsText() + " · 更新 " + timeHM(j.at);
  $("meta").textContent = meta;
  // 长告警不挤进元信息行：折成标题旁的红标，完整文本放 title。
  const warnTag = $("warnTag");
  warnTag.classList.toggle("hidden", !j.warn);
  warnTag.title = j.warn || "";
  $("sideFoot").textContent = "OKX v5 · " + (j.optionUly || "BTC-USD") + " · 仅供研究，不构成投资建议";
  renderTradeBar();
  renderAuthFoot();
  if (!$("autoFailModal").classList.contains("hidden")) renderAutoFailures();
  syncChips();
  syncChrome();
}
function updateSortUI() {
  document.querySelectorAll(".sort").forEach(function (b) {
    b.classList.toggle("on", (b.dataset.sort || "") === sortKey);
  });
  const d = $("sortDir");
  d.textContent = sortDesc ? "↓ 高→低" : "↑ 低→高";
  d.disabled = !sortKey;
}
function setStatus(txt) { $("status").textContent = txt || ""; }
function setFlash(msg, isErr) {
  const el = $("flash");
  if (!msg) { el.className = "flash hidden"; el.textContent = ""; return; }
  el.className = "flash " + (isErr ? "is-err" : "is-ok");
  el.textContent = msg;
  clearTimeout(setFlash.timer);
  setFlash.timer = setTimeout(function () { setFlash(""); }, 8000);
}

// ── 本地偏好：标的 / 方向 / 排序 ──
// 刷新页面、重启浏览器（同一 origin）后恢复；读不到或格式不对一律回落默认，
// 不阻塞首屏。到期 / 本位 / 搜索是临时观察条件，不落盘。
const PREFS_KEY = "coinoption.prefs.v1";

function readPrefs() {
  try {
    if (!window.localStorage) return {};
    const raw = window.localStorage.getItem(PREFS_KEY);
    if (!raw) return {};
    const p = JSON.parse(raw);
    return p && typeof p === "object" ? p : {};
  } catch (e) { return {}; }
}
function savePrefs() {
  try {
    if (!window.localStorage) return;
    window.localStorage.setItem(PREFS_KEY, JSON.stringify({
      uly: curUly, side: curSide, sortKey: sortKey, sortDesc: sortDesc
    }));
  } catch (e) {}
}
function restorePrefs() {
  const p = readPrefs();
  if (p.uly === "BTC-USDT" || p.uly === "ETH-USDT" || p.uly === "SOL-USDT") curUly = p.uly;
  if (p.side === "" || p.side === "C" || p.side === "P") curSide = p.side;
  if (p.sortKey === "" || p.sortKey === "price" || p.sortKey === "apr" || p.sortKey === "diff") sortKey = p.sortKey;
  if (typeof p.sortDesc === "boolean") sortDesc = p.sortDesc;
}

// ── 小部件 ──
function policyRow(label, value, tone) {
  const row = h("div", "policy-row");
  row.appendChild(h("span", "", label));
  row.appendChild(h("b", tone || "", value));
  return row;
}
function infoCell(label, value, sub, valueCls) {
  const box = h("div", "info-cell");
  box.appendChild(monoLabel(label));
  box.appendChild(h("div", "info-value " + (valueCls || ""), value));
  if (sub) box.appendChild(h("div", "info-sub", sub));
  return box;
}
function docTable(headers, rows) {
  const table = h("table", "doc-table");
  const thead = document.createElement("thead");
  const tr = document.createElement("tr");
  headers.forEach(function (t, i) { tr.appendChild(h("th", i ? "ta-r" : "", t)); });
  thead.appendChild(tr);
  table.appendChild(thead);
  const tbody = document.createElement("tbody");
  rows.forEach(function (cells) {
    const row = document.createElement("tr");
    cells.forEach(function (c, i) {
      const cls = i === 1 ? "val ta-r" : (i === 2 ? "note ta-r" : "");
      row.appendChild(h("td", cls, c));
    });
    tbody.appendChild(row);
  });
  table.appendChild(tbody);
  return table;
}
function chainBox(title, sub, tone) {
  const box = h("div", "chain-box " + (tone || ""));
  box.appendChild(h("b", "", title));
  if (sub) box.appendChild(h("div", "", sub));
  return box;
}
function chainStrip(items) {
  const wrap = h("div", "chain");
  items.forEach(function (it, i) {
    if (i) wrap.appendChild(h("i", "ti ti-arrow-narrow-right chain-arrow"));
    const item = h("div", "chain-item");
    item.appendChild(chainBox(it.title, it.sub, it.tone));
    wrap.appendChild(item);
  });
  return wrap;
}
function chainNodes(nodes) {
  const wrap = h("div", "chain");
  nodes.forEach(function (n, i) {
    if (i) wrap.appendChild(h("span", "chain-line"));
    wrap.appendChild(h("span", "chain-node " + n.cls, n.txt));
  });
  return wrap;
}
function stampButton(icon, label, key, tone, onClick) {
  const b = h("button", "stamp-btn " + (tone || ""));
  b.type = "button";
  b.appendChild(h("i", "ti " + icon));
  b.appendChild(h("span", "", label));
  b.appendChild(h("span", "stamp-key", key || ""));
  if (onClick) b.addEventListener("click", onClick);
  return b;
}
function policyCard() {
  const box = h("div", "policy");
  box.appendChild(monoLabel("POLICY CHECK"));
  box.appendChild(policyRow("模式", curMode, curMode === "live" ? "" : "is-green"));
  box.appendChild(policyRow("下单范围", manualTrade ? "建议+手动" : "仅建议", manualTrade ? "is-amber" : ""));
  box.appendChild(policyRow("期权费率", "0.03% 普通档"));
  if (curGuard && curGuard.limits) {
    box.appendChild(policyRow("单笔上限", fmtNum(curGuard.limits.perOrderUsdt) + " U"));
    box.appendChild(policyRow("剩余敞口", fmtNum(curGuard.remainingUsdt || 0) + " U"));
    box.appendChild(policyRow("今日", (curGuard.dayCount || 0) + "/" + (curGuard.limits.maxSessionsPerDay || 0)));
  } else {
    box.appendChild(policyRow("执行器", "未接入"));
  }
  box.appendChild(policyRow("行情", wsText(), lastSnap && lastSnap.wsConnected ? "is-green" : "is-amber"));
  return box;
}
function emptyState(title, hint) {
  const box = h("div", "empty");
  box.appendChild(h("b", "", title));
  box.appendChild(h("div", "", hint || ""));
  return box;
}
function emptyDoc(title, hint) {
  const box = h("div", "empty-doc-big");
  box.appendChild(monoLabel("NO SELECTION"));
  box.appendChild(h("b", "", title));
  box.appendChild(h("p", "", hint || ""));
  return box;
}
function logCard(title, note, lines) {
  const box = h("div", "log-card");
  const head = h("div", "log-head");
  const t = h("div", "log-title");
  t.appendChild(h("i", "ti ti-list-details"));
  t.appendChild(h("span", "", title));
  head.appendChild(t);
  head.appendChild(h("span", "log-note", note || ""));
  box.appendChild(head);
  box.appendChild(h("div", "log-body", lines.join("\n")));
  return box;
}

// ── queue ──
function buildDecByOpt(j) {
  const map = new Map();
  for (const d of (j.decisions || [])) {
    const r = d.row || {};
    if (r.optInstId && !map.has(r.optInstId)) map.set(r.optInstId, d);
  }
  return map;
}
function badgeText(r) {
  const t = optTypeName(r.optType);
  return (t === "-" ? "" : t + " ") + fmtNum(r.strike);
}
function buildDecisionCard(d) {
  const r = d.row || {};
  const hasBid = !!r.optBidPx;
  const card = h("article", "req-card" + (r.productId === selDec ? " is-selected" : ""));
  card.dataset.dec = r.productId || "";
  card.appendChild(h("span", "req-id " + badgeTone(d), badgeText(r)));

  const top = h("div", "req-top");
  const main = h("div", "req-main");
  main.appendChild(h("div", "req-name", "行权 " + fmtNum(r.strike) + " · " + sideLabel(r.optType)));
  main.appendChild(h("div", "req-sub", "到期 " + (r.expTime ? expStr(r.expTime) : "-") +
    " · 剩余 " + Math.round(r.days || 0) + " 天 · 投入 " + (r.investCcy || "-")));
  const amt = h("div", "req-amt");
  amt.appendChild(h("div", "req-num" + (hasBid ? "" : " is-muted"), hasBid ? pct(r.optApr) + "%" : "—"));
  amt.appendChild(h("div", "req-unit", hasBid ? "期权APR" : "无买盘"));
  top.appendChild(main);
  top.appendChild(amt);

  const tags = h("div", "req-tags");
  tags.appendChild(tagEl(actionTitle(d), actionTagClass(d)));
  tags.appendChild(tagEl(settleName(settleKind(r.optSettleCcy, r.optInstId)), "tag-paper"));
  if (r.settleMismatch) {
    tags.appendChild(tagEl("抵押品不一致", "tag-red"));
  }
  const moveTxt = toStrikeText(r.optType, r.strike, r.spot);
  if (moveTxt !== "—") {
    tags.appendChild(tagEl(moveTxt, moveTxt.indexOf("已穿") === 0 ? "tag-red" : ((r.optType === 80 || r.optType === "P") ? "tag-mint" : "tag-paper")));
  }
  if (hasBid) {
    tags.appendChild(tagEl("差 " + signed(r.diff * 100) + "pp", r.diff >= 0 ? "tag-green" : "tag-paper"));
  } else {
    tags.appendChild(tagEl(reasonTxt(d.reasonCode), "tag-mint"));
  }

  const foot = h("div", "req-foot");
  const meta = h("div", "req-meta");
  meta.appendChild(h("span", "req-opt mono", r.optInstId ? "期权 " + shortInst(r.optInstId) : "期权 未对齐"));
  meta.appendChild(h("span", "req-bid mono", hasBid ? "买价 " + r.optBidPx : "买价 —"));
  foot.appendChild(meta);
  foot.appendChild(chainNodes([
    { txt: "D", cls: "n-green" },
    { txt: hasBid ? "O" : "!", cls: hasBid ? "n-green" : "n-amber" },
    { txt: hasBid ? (r.diff >= 0 ? "+" : "−") : "·", cls: hasBid ? (r.diff >= 0 ? "n-green" : "n-red") : "n-white" }
  ]));

  card.appendChild(top);
  card.appendChild(tags);
  card.appendChild(foot);
  return card;
}
function buildChainRow(r, d) {
  const row = h("button", "chain-row" + (r.instId === selChain ? " is-selected" : ""));
  row.type = "button";
  row.dataset.inst = r.instId || "";
  row.appendChild(h("span", "cr-name", r.instId || "-"));
  row.appendChild(h("span", "cr-cell", fmtNum(r.strike)));
  row.appendChild(h("span", "cr-cell", r.bidPx ? String(r.bidPx) : "—"));
  row.appendChild(h("span", "cr-cell" + (r.aprValid ? "" : " is-muted"), r.aprValid ? pct(r.optApr) + "%" : "—"));
  return row;
}
function matchQuery(text) {
  if (!curQuery) return true;
  return String(text || "").toLowerCase().indexOf(curQuery) >= 0;
}
function renderQueue(j, decByOpt) {
  const q = $("queue");
  const sc = q.scrollTop;
  q.innerHTML = "";
  let count = 0;
  if (curTab === "chain") {
    let rows = (j.optionRows || []).filter(function (r) { return matchQuery(r.instId); });
    rows = sortList("chain", rows, decByOpt);
    if (!rows.some(function (r) { return r.instId === selChain; })) {
      const sellable = rows.find(function (r) { return r.aprValid && r.bidPx; });
      selChain = sellable ? sellable.instId : (rows.length ? rows[0].instId : "");
    }
    for (const r of rows) q.appendChild(buildChainRow(r, decByOpt.get(r.instId)));
    count = rows.length;
    if (!rows.length) q.appendChild(emptyState("该组合下没有期权链数据", "重置筛选，或换标的 / 到期"));
  } else {
    let ds = (j.decisions || []).filter(function (d) {
      const r = d.row || {};
      return matchQuery(r.productId) || matchQuery(r.optInstId);
    });
    ds = sortList("decisions", ds, decByOpt);
    if (!ds.some(function (d) { return (d.row || {}).productId === selDec; })) {
      selDec = ds.length ? (ds[0].row || {}).productId : "";
    }
    for (const d of ds) q.appendChild(buildDecisionCard(d));
    count = ds.length;
    if (!ds.length) {
      q.appendChild(emptyState("本轮没有可展示的决策",
        j.dcyAvailable ? "调整筛选条件，或切到「完整期权链」看全部合约" : "DCY 需要实盘 API Key（模拟盘无金融产品）"));
    }
  }
  $("queueCount").textContent = count + " 行";
  q.scrollTop = sc;
}

// ── detail ──
function spotOf(j) { return Number(j.spotPrice) || 0; }
function distPct(strike, spot) { return spot > 0 ? (strike - spot) / spot * 100 : null; }

// 行权价相对现价还要「跌/涨」多少：PUT 看跌幅（安全垫），CALL 看涨幅。
// 已越过行权价（实值方向）显示「已穿」。
function toStrikeText(optType, strike, spot) {
  const s = Number(spot) || 0;
  if (!(s > 0) || !(strike > 0)) return "—";
  const d = (strike - s) / s * 100;
  const isPut = optType === 80 || optType === "P" || optType === "p";
  if (isPut) {
    const drop = -d;
    return drop >= 0 ? "跌 " + drop.toFixed(2) + "%" : "已穿 " + Math.abs(drop).toFixed(2) + "%";
  }
  return d >= 0 ? "涨 " + d.toFixed(2) + "%" : "已穿 " + Math.abs(d).toFixed(2) + "%";
}
function renderDecisionDoc(j, doc, rail, log) {
  const d = (j.decisions || []).find(function (x) { return (x.row || {}).productId === selDec; });
  if (!d) {
    doc.appendChild(emptyDoc("本轮没有可执行的决策",
      "DCY 需要实盘 API Key（模拟盘无金融产品）· 按「完整期权链」可查看全部合约"));
    return false;
  }
  const r = d.row || {};
  const hasBid = !!r.optBidPx;
  const spot = spotOf(j);

  const head = h("div", "doc-head");
  const left = h("div");
  left.appendChild(h("div", "doc-title font-display", actionTitle(d)));
  left.appendChild(h("div", "doc-sub", (r.productId || "-") + " · " + (r.expTime ? expStr(r.expTime) : "-")));
  const right = h("div", "doc-right");
  right.appendChild(monoLabel("建议金额"));
  right.appendChild(h("div", "doc-amount", d.suggestedAmt ? fmtNum(d.suggestedAmt) + " " + (r.investCcy || "") : "—"));
  right.appendChild(h("div", "doc-note",
    d.suggestedAmt ? "风控单笔上限内" : (d.action === "unaligned" ? "未对齐 · 可手动申购" : "超上限 / 无建议")));
  head.appendChild(left);
  head.appendChild(right);
  doc.appendChild(head);

  const grid = h("div", "doc-grid3");
  grid.appendChild(infoCell("DCY 产品", dcySideName(r.optType),
    shortProduct(r.productId) + " · 投入 " + (r.investCcy || "-") + " · " + reasonTxt(d.reasonCode)));
  grid.appendChild(infoCell("期权合约", r.optInstId ? shortInst(r.optInstId) : "未对齐",
    (r.optMatched ? settleName(settleKind(r.optSettleCcy, r.optInstId)) + " · 买价 " + (r.optBidPx || "—") : "±1 天窗口内无同向档") +
      (r.settleMismatch ? " · ⚠ 抵押品不一致" : ""), "is-contract"));
  grid.appendChild(infoCell("到期", r.expTime ? shortDate(r.expTime) : "-", "剩余 " + Math.round(r.days || 0) + " 天 · 08:00 结算", "is-num"));
  doc.appendChild(grid);

  const dist = r.strike && spot ? distPct(r.strike, spot) : null;
  doc.appendChild(docTable(["指标", "数值", "备注"], [
    ["DCY APR", pct(r.dcyApr) + "%", "双币赢年化"],
    ["期权 APR", hasBid ? pct(r.optApr) + "%" : "—", hasBid ? "按买一价计的净年化（已扣 0.03%）" : "无买盘，算不出"],
    ["差%", hasBid ? signed(r.diff * 100) + "pp" : "—", "期权 − DCY"],
    ["行权价", fmtNum(r.strike), dist == null ? "无现价" : "距现价 " + signed(dist) + "%"],
    ["到行权", toStrikeText(r.optType, r.strike, spot), r.optType === 80 || r.optType === "P" ? "现价需跌到行权价的比例（安全垫）" : "现价需涨到行权价的比例"],
    ["买一价", r.optBidPx || "—", (r.optSettleCcy || "本位") + " 计价权利金"],
    ["剩余天数", Math.round(r.days || 0) + " 天", "7–30 天窗口内才可执行"],
    ["原因码", d.reasonCode || "-", reasonTxt(d.reasonCode)]
  ]));

  const chainWrap = h("div", "doc-chain");
  chainWrap.appendChild(monoLabel("对照链"));
  chainWrap.appendChild(chainStrip([
    { title: "标的", sub: fmtNum(spot) + " USDT", tone: "" },
    { title: "DCY", sub: pct(r.dcyApr) + "%", tone: "is-green" },
    { title: hasBid ? "期权" : "无买盘", sub: hasBid ? pct(r.optApr) + "%" : "—", tone: hasBid ? "is-green" : "is-amber" },
    { title: hasBid ? signed(r.diff * 100) + "pp" : "不可执行", sub: hasBid ? "差%" : "仅对齐", tone: hasBid ? (r.diff >= 0 ? "is-green" : "is-amber") : "is-dashed" }
  ]));
  doc.appendChild(chainWrap);

  rail.appendChild(monoLabel("DECIDE"));
  const canSell = tradeEnabled && hasBid && d.action === "buy_option";
  const sell = stampButton("ti-stamp", "卖出期权", "⌘ + ↵", canSell ? "is-green" : "", function () { openOrder("option", d); });
  sell.disabled = !canSell;
  rail.appendChild(sell);
  const canDcy = tradeEnabled && (d.action === "subscribe_dual" || d.action === "unaligned");
  const dcd = stampButton("ti-coin", "申购 DCY", "⌘ + D", canDcy ? "is-amber" : "", function () { openOrder("dcd", d); });
  dcd.disabled = !canDcy;
  rail.appendChild(dcd);
  rail.appendChild(policyCard());
  if (!tradeEnabled) rail.appendChild(h("div", "rail-hint", "当前为只读看板（--read-only）"));
  else if (r.settleMismatch) rail.appendChild(h("div", "rail-hint", "抵押品本位与投资币种不一致：请手动核对保证金后再下单"));
  else if (d.action === "unaligned") rail.appendChild(h("div", "rail-hint", "未对齐：可手动申购 DCY，自动执行不会碰这一行"));

  log.appendChild(logCard("DECISION LOG", "REASON / MATCH / PRICE", [
    "REASON   " + (d.reasonCode || "-") + " · " + reasonTxt(d.reasonCode),
    "ACTION   " + actionTitle(d) + " · 建议 " + (d.suggestedAmt ? fmtNum(d.suggestedAmt) + " " + (r.investCcy || "") : "不下单"),
    "MATCH    " + (r.optInstId || "未对齐") + " · " + (r.optMatched ? "±1 天窗口内最近同向档" : "未找到同向合约"),
    "PRICE    " + (hasBid ? "买价 " + r.optBidPx + " · 期权APR " + pct(r.optApr) + "%" : "无买盘，仅展示对齐"),
    "STRIKE   " + fmtNum(r.strike) + " · 偏差 " + signed(r.strikeErrPct || 0) + "%",
    "UPDATED  " + timeHM(j.at) + " · 行情每秒刷新，仅作研究参考"
  ]));
  return true;
}
function renderChainDoc(j, decByOpt, doc, rail, log) {
  const r = (j.optionRows || []).find(function (x) { return x.instId === selChain; });
  if (!r) {
    doc.appendChild(emptyDoc("选择一张期权合约", "点选左侧任意合约，查看报价、卖方 APR 与 DCY 对照"));
    return false;
  }
  const d = decByOpt.get(r.instId);
  const hasDcy = !!(d && d.row);
  const spot = Number(r.spotPx) || spotOf(j);
  const dist = r.strike && spot ? distPct(r.strike, spot) : null;

  const head = h("div", "doc-head");
  const left = h("div");
  left.appendChild(h("div", "doc-title font-display", optTypeName(r.optType) + " " + fmtNum(r.strike)));
  left.appendChild(h("div", "doc-sub", (r.instId || "-") + " · " + (r.expiry ? expStr(r.expiry) : instDay(r.instId))));
  const right = h("div", "doc-right");
  right.appendChild(monoLabel("期权 APR"));
  right.appendChild(h("div", "doc-amount", r.aprValid ? pct(r.optApr) + "%" : "—"));
  right.appendChild(h("div", "doc-note", r.aprValid ? "卖方年化" : "无买盘 / 算不出"));
  head.appendChild(left);
  head.appendChild(right);
  doc.appendChild(head);

  const grid = h("div", "doc-grid3");
  grid.appendChild(infoCell("结算本位", settleName(settleKind(r.settleCcy, r.instId)), r.settleCcy || "-"));
  grid.appendChild(infoCell("行权价", fmtNum(r.strike), dist == null ? "无现价" : "距现价 " + signed(dist) + "%"));
  grid.appendChild(infoCell("剩余天数", Math.round(r.days || 0) + " 天", "到期 " + expStr(r.expiry)));
  doc.appendChild(grid);

  doc.appendChild(docTable(["指标", "数值", "备注"], [
    ["买一价", r.bidPx ? String(r.bidPx) : "—", "可成交卖价"],
    ["卖一价", r.askPx ? String(r.askPx) : "—", "对手方报价"],
    ["最新成交", r.lastPx ? String(r.lastPx) : "—", r.bidPx ? "参考" : "无买盘，仅成交价"],
    ["到行权", toStrikeText(r.optType, r.strike, spot), r.optType === 67 || r.optType === "C" ? "现价需涨到行权价的比例" : "现价需跌到行权价的比例（安全垫）"],
    ["期权 APR", r.aprValid ? pct(r.optApr) + "%" : "—", r.aprValid ? "净年化（已扣 0.03%）" : "无买盘"],
    ["DCY APR", hasDcy ? pct(d.row.dcyApr) + "%" : "—", hasDcy ? "对应 " + d.row.productId : "无对应产品"],
    ["差%", hasDcy && r.aprValid ? signed((r.optApr - d.row.dcyApr) * 100) + "pp" : "—", "期权 − DCY"],
    ["卖方方向", r.optType === 67 || r.optType === "C" ? "CALL ≥ 现价" : "PUT ≤ 现价", "实值档已在列表过滤"]
  ]));

  const chainWrap = h("div", "doc-chain");
  chainWrap.appendChild(monoLabel("对照链"));
  chainWrap.appendChild(chainStrip([
    { title: "标的", sub: fmtNum(spot) + " USDT", tone: "" },
    { title: "期权", sub: r.aprValid ? pct(r.optApr) + "%" : "无买盘", tone: r.aprValid ? "is-green" : "is-amber" },
    { title: "DCY", sub: hasDcy ? pct(d.row.dcyApr) + "%" : "无对应", tone: hasDcy ? "is-green" : "is-dashed" },
    { title: hasDcy && r.aprValid ? signed((r.optApr - d.row.dcyApr) * 100) + "pp" : "—", sub: "差%", tone: hasDcy && r.aprValid ? "is-green" : "is-dashed" }
  ]));
  doc.appendChild(chainWrap);

  rail.appendChild(monoLabel("DECIDE"));
  const suggested = !!(hasDcy && d.action === "buy_option");
  const canSell = tradeEnabled && !!r.aprValid && !!r.bidPx;
  // 无 DCY 建议时构造一个仅用于下单弹窗的伪决策（服务端只认 instId/sz）。
  const target = hasDcy ? d : {
    action: "buy_option",
    row: {
      productId: "", optInstId: r.instId, optType: r.optType, strike: r.strike,
      expTime: r.expiry, optBidPx: r.bidPx, optApr: r.optApr, optSettleCcy: r.settleCcy,
      spot: r.spotPx, days: r.days, investCcy: "USDT", optMatched: false
    }
  };
  const sell = stampButton("ti-stamp", "卖出期权", "⌘ + ↵", canSell ? "is-green" : "", function () { openOrder("option", target); });
  sell.disabled = !canSell;
  rail.appendChild(sell);
  if (hasDcy) {
    const jump = stampButton("ti-arrow-back-up", "查看 DCY", "", "", function () { jumpToDecision(d.row.productId); });
    rail.appendChild(jump);
  }
  rail.appendChild(policyCard());
  let hintTxt = "";
  if (!tradeEnabled) hintTxt = "当前为只读看板（--read-only）";
  else if (!r.aprValid || !r.bidPx) hintTxt = "该档无买盘，无法卖出";

  if (hintTxt) rail.appendChild(h("div", "rail-hint", hintTxt));

  log.appendChild(logCard("CONTRACT LOG", "CHAIN / QUOTE", [
    "CONTRACT " + (r.instId || "-") + " · " + settleName(settleKind(r.settleCcy, r.instId)),
    "QUOTE    " + (r.bidPx ? "买 " + r.bidPx : "无买盘") + " · " + (r.askPx ? "卖 " + r.askPx : "无卖盘") + " · 最新 " + (r.lastPx || "—"),
    "APR      " + (r.aprValid ? pct(r.optApr) + "%" : "不可算") + " · " + (hasDcy ? "DCY " + pct(d.row.dcyApr) + "%" : "无对应 DCY"),
    "GREEKS   —（接口未提供，按权利金/天数估算）",
    "UPDATED  " + timeHM(j.at)
  ]));
  return true;
}
function renderDoc(j, decByOpt) {
  const doc = $("doc");
  const rail = $("rail");
  const log = $("log");
  doc.innerHTML = "";
  rail.innerHTML = "";
  log.innerHTML = "";
  const has = curTab === "chain"
    ? renderChainDoc(j, decByOpt, doc, rail, log)
    : renderDecisionDoc(j, doc, rail, log);
  doc.classList.toggle("is-empty", !has);
  log.classList.toggle("hidden", !has);
  if (!has) {
    rail.appendChild(monoLabel("DECIDE"));
    // 无数据/未选中时也把两枚下单按钮画出来（置灰），让「有没有手动下单」一眼可见。
    const s1 = stampButton("ti-stamp", "卖出期权", "⌘ + ↵", "", null);
    s1.disabled = true;
    rail.appendChild(s1);
    const s2 = stampButton("ti-coin", "申购 DCY", "⌘ + D", "", null);
    s2.disabled = true;
    rail.appendChild(s2);
    rail.appendChild(policyCard());
    rail.appendChild(h("div", "rail-hint", tradeEnabled ? "先从左侧选择一条决策 / 合约" : "当前为只读看板（--read-only）"));
  }
  if (animateDoc) {
    doc.classList.remove("anim");
    void doc.offsetWidth;
    doc.classList.add("anim");
    animateDoc = false;
  }
}

// ── 整体渲染 ──
function renderRows(j) {
  lastSnap = j;
  const decByOpt = buildDecByOpt(j);
  renderQueue(j, decByOpt);
  renderDoc(j, decByOpt);
  updateSortUI();
}
function renderDays(days) {
  const box = $("days");
  box.innerHTML = "";
  for (const pair of days) {
    const day = pair[0], n = pair[1];
    const b = h("button", "chip day" + (day === curExp ? " on" : ""), day.slice(5) + "(" + n + ")");
    b.type = "button";
    b.addEventListener("click", function () { setExp(day); });
    box.appendChild(b);
  }
}
function expiryDays(decisions, optionRows) {
  const m = new Map();
  const add = function (day) { if (!day) return; m.set(day, (m.get(day) || 0) + 1); };
  for (const d of decisions) {
    const r = d.row || {};
    add(r.expTime ? r.expTime.slice(0, 10) : instDay(r.optInstId));
  }
  for (const or of optionRows) add(instDay(or.instId));
  return Array.from(m.entries()).sort(function (a, b) { return a[0] < b[0] ? -1 : 1; });
}
function applySnapshot(j) {
  lastSnap = j;
  renderMeta(j);
  renderDays(expiryDays(j.decisions || [], j.optionRows || []));
  renderRows(j);
  setStatus(wsDown ? "WebSocket 断线，重连中…" : "");
}

// ── 数据 ──
async function load() {
  if (loading) return;
  loading = true;
  setStatus("刷新中…");
  const params = new URLSearchParams();
  params.set("uly", curUly);
  if (curExp) params.set("exp", curExp);
  if (curSide) params.set("side", curSide);
  if (curSettle) params.set("settle", curSettle);
  try {
    const r = await fetch("/api/decisions?" + params.toString(), { cache: "no-store" });
    if (r.status === 401) { location.replace("/login"); return; }
    const j = await r.json();
    if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
    applySnapshot(j);
  } catch (e) {
    $("meta").textContent = "加载失败：" + e.message;
    setStatus("");
  } finally {
    loading = false;
  }
}
function wsEndpoint() {
  const params = new URLSearchParams();
  params.set("uly", curUly);
  if (curExp) params.set("exp", curExp);
  if (curSide) params.set("side", curSide);
  if (curSettle) params.set("settle", curSettle);
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return proto + "//" + location.host + "/ws?" + params.toString();
}
function connectWS() {
  if (!window.WebSocket) { load(); return; }
  if (wsTimer) { clearTimeout(wsTimer); wsTimer = null; }
  if (ws) {
    ws.onclose = null;
    ws.onerror = null;
    try { ws.close(); } catch (e) {}
    ws = null;
  }
  let sock;
  try { sock = new WebSocket(wsEndpoint()); } catch (e) { load(); return; }
  ws = sock;
  sock.onopen = function () { wsRetry = 0; wsDown = false; setStatus(""); };
  sock.onmessage = function (ev) {
    if (ws !== sock) return;
    let j;
    try { j = JSON.parse(ev.data); } catch (e) { return; }
    if (j.error) { setStatus("WS: " + j.error); return; }
    applySnapshot(j);
  };
  sock.onclose = function () {
    if (ws === sock) ws = null;
    wsDown = true;
    const wait = Math.min(10000, 1000 * Math.pow(2, Math.min(wsRetry, 4)));
    wsRetry++;
    setStatus("WebSocket 断线，重连中…");
    wsTimer = setTimeout(connectWS, wait);
    load();
  };
  sock.onerror = function () { try { sock.close(); } catch (e) {} };
}
function refresh() {
  if (window.WebSocket) connectWS(); else load();
}
function setExp(day) {
  curExp = day || "";
  $("exp").value = curExp;
  refresh();
}
function setSide(side) {
  curSide = side || "";
  savePrefs();
  refresh();
}
function setSettle(settle) {
  curSettle = settle || "";
  refresh();
}
function switchTab(name) {
  const next = name === "chain" ? "chain" : "decisions";
  if (next !== curTab) animateDoc = true;
  curTab = next;
  syncChrome();
  if (lastSnap) {
    const m = buildDecByOpt(lastSnap);
    renderQueue(lastSnap, m);
    renderDoc(lastSnap, m);
    updateSortUI();
  }
}
function jumpToDecision(productId) {
  curTab = "decisions";
  selDec = productId || "";
  animateDoc = true;
  syncChrome();
  if (!lastSnap) return;
  const m = buildDecByOpt(lastSnap);
  renderQueue(lastSnap, m);
  renderDoc(lastSnap, m);
  updateSortUI();
  document.querySelectorAll("#queue [data-dec]").forEach(function (el) {
    if (el.dataset.dec === productId) el.scrollIntoView({ block: "center" });
  });
}
function selectItem(kind, id) {
  if (!id || !lastSnap) return;
  if (kind === "dec") { if (selDec === id) return; selDec = id; }
  else { if (selChain === id) return; selChain = id; }
  animateDoc = true;
  document.querySelectorAll("#queue [data-dec], #queue [data-inst]").forEach(function (el) {
    const v = kind === "dec" ? el.dataset.dec : el.dataset.inst;
    el.classList.toggle("is-selected", v === id);
  });
  renderDoc(lastSnap, buildDecByOpt(lastSnap));
}
function refreshSort() {
  savePrefs();
  updateSortUI();
  if (!lastSnap) return;
  const m = buildDecByOpt(lastSnap);
  renderQueue(lastSnap, m);
  renderDoc(lastSnap, m);
  renderMeta(lastSnap);
}

// ── 下单 ──
function openOrder(kind, d) {
  if (!tradeEnabled || submitting || !d || !d.row) return;
  const r = d.row;
  pending = { kind: kind, d: d };
  $("omTitle").textContent = kind === "option" ? "卖出期权 · SELL" : "申购双币赢 · DCY";
  const info = [];
  if (kind === "option") {
    info.push("合约 " + r.optInstId);
    info.push("方向 " + optTypeName(r.optType) + " · 行权 " + fmtNum(r.strike) + " · 到期 " + expStr(r.expTime));
    info.push("买一价 " + (r.optBidPx || "-") + " · 期权APR " + pct(r.optApr) + "%");
    info.push("按买一价挂限价单；风控按合约名义折算，超上限会被拒单");
    $("omAmtLabel").textContent = "卖出张数";
    $("omAmt").value = "1";
    $("omAmt").min = "0";
  } else {
    info.push("产品 " + r.productId);
    info.push("方向 " + dcySideName(r.optType) + " · 行权 " + fmtNum(r.strike) + " · 到期 " + expStr(r.expTime));
    info.push("DCY APR " + pct(r.dcyApr) + "% · 投入 " + (r.investCcy || "-"));
    info.push("可投区间 " + (r.dcyMinSize || "-") + " – " + (r.dcyMaxSize || "-") + " " + (r.investCcy || ""));
    $("omAmtLabel").textContent = "投入 " + (r.investCcy || "");
    const def = Number(d.suggestedAmt || r.dcyMinSize || 0);
    $("omAmt").value = def > 0 ? String(Math.round(def * 10000) / 10000) : "";
    $("omAmt").min = r.dcyMinSize > 0 ? String(r.dcyMinSize) : "0";
  }
  const box = $("omInfo");
  box.innerHTML = "";
  for (const line of info) box.appendChild(h("div", "", line));
  $("omNote").className = "omnote";
  $("omNote").textContent = curMode === "live"
    ? "⚠ 实盘模式：确认后将向 OKX 提交真实订单，请再次核对合约与数量。"
    : "当前为 " + curMode + " 模式：不会提交真实订单。";
  $("omSubmit").textContent = curMode === "live" ? "确认实盘下单" : "确认下单";
  $("omSubmit").disabled = false;
  $("orderModal").classList.remove("hidden");
  $("omAmt").focus();
}
function closeOrder() {
  if (submitting) return;
  pending = null;
  $("orderModal").classList.add("hidden");
}
async function submitOrder() {
  if (!pending || submitting) return;
  const amt = Number($("omAmt").value);
  if (!(amt > 0)) {
    $("omNote").className = "omnote err";
    $("omNote").textContent = "数量必须为正数";
    return;
  }
  const r = pending.d.row || {};
  const kind = pending.kind;
  submitting = true;
  $("omSubmit").disabled = true;
  $("omNote").className = "omnote";
  $("omNote").textContent = "提交中…";
  try {
    const url = kind === "option" ? "/api/order/option" : "/api/order/dcd";
    const body = kind === "option"
      ? { uly: curUly, instId: r.optInstId, sz: amt, px: 0 }
      : { uly: curUly, productId: r.productId, amt: amt };
    const resp = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    });
    const j = await resp.json().catch(function () { return {}; });
    if (!resp.ok) throw new Error(j.error || ("HTTP " + resp.status));
    $("orderModal").classList.add("hidden");
    pending = null;
    setFlash("✓ " + (j.detail || "已提交"), false);
    dropStamp(kind === "option" ? "SELL" : "DCY", "#2E8C42");
    setTimeout(refresh, 2500);
  } catch (e) {
    $("omNote").className = "omnote err";
    $("omNote").textContent = "✗ " + e.message;
    $("omSubmit").disabled = false;
  } finally {
    submitting = false;
  }
}
function dropStamp(label, color) {
  const doc = $("doc");
  const el = h("div", "big-stamp", label);
  el.style.color = color;
  doc.appendChild(el);
  setTimeout(function () { el.remove(); }, 1500);
}

// ── clock ──
function updateClock() {
  const d = new Date();
  $("clock").textContent = WEEKDAYS[d.getDay()] + " · " + pad(d.getDate()) + " " + MONTHS[d.getMonth()] + " " +
    d.getFullYear() + " · " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds()) + " LOCAL";
}

// ── 事件绑定 ──
document.querySelectorAll(".cur").forEach(function (b) {
  b.addEventListener("click", function () {
    curUly = b.dataset.uly;
    savePrefs();
    animateDoc = true;
    refresh();
  });
});
document.querySelectorAll(".tab").forEach(function (b) {
  b.addEventListener("click", function () { switchTab(b.dataset.tab); });
});
document.querySelectorAll(".nav-item").forEach(function (b) {
  b.addEventListener("click", function () { if (b.dataset.tab) switchTab(b.dataset.tab); });
});
document.querySelectorAll(".side").forEach(function (b) {
  b.addEventListener("click", function () { setSide(b.dataset.side); });
});
document.querySelectorAll(".settle").forEach(function (b) {
  b.addEventListener("click", function () { setSettle(b.dataset.settle); });
});
document.querySelectorAll(".sort").forEach(function (b) {
  b.addEventListener("click", function () {
    const v = b.dataset.sort || "";
    if (v === sortKey && v !== "") sortDesc = !sortDesc;
    else { sortKey = v; sortDesc = true; }
    refreshSort();
  });
});
$("sortDir").addEventListener("click", function () {
  if (!sortKey) return;
  sortDesc = !sortDesc;
  refreshSort();
});
$("queue").addEventListener("click", function (e) {
  const dec = e.target.closest("[data-dec]");
  if (dec) { selectItem("dec", dec.dataset.dec); return; }
  const inst = e.target.closest("[data-inst]");
  if (inst) selectItem("chain", inst.dataset.inst);
});
$("search").addEventListener("input", function () {
  curQuery = $("search").value.trim().toLowerCase();
  if (!lastSnap) return;
  const m = buildDecByOpt(lastSnap);
  renderQueue(lastSnap, m);
  renderDoc(lastSnap, m);
});
$("refreshBtn").addEventListener("click", function () { setFlash(""); animateDoc = true; refresh(); });
$("expApply").addEventListener("click", function () { setExp($("exp").value); });
$("exp").addEventListener("change", function () { setExp($("exp").value); });
$("expClear").addEventListener("click", function () { setExp(""); });
$("omCancel").addEventListener("click", closeOrder);
$("omClose").addEventListener("click", closeOrder);
$("omSubmit").addEventListener("click", submitOrder);
$("omAmt").addEventListener("keydown", function (e) { if (e.key === "Enter") submitOrder(); });
$("orderModal").addEventListener("click", function (e) { if (e.target === $("orderModal")) closeOrder(); });
$("settingsNav").addEventListener("click", openSettings);
$("setClose").addEventListener("click", closeSettings);
$("setCancel").addEventListener("click", closeSettings);
$("setSave").addEventListener("click", saveSettings);
$("setCapital").addEventListener("input", renderSetDerived);
$("setAuto").addEventListener("click", function () {
  if (!setAutoOn && curMode === "live" &&
      !window.confirm("实盘自动交易：会按策略自动提交真实订单，确认开启？")) return;
  setAutoOn = !setAutoOn;
  renderSetAuto();
});
$("setLogout").addEventListener("click", logout);
$("settingsModal").addEventListener("click", function (e) { if (e.target === $("settingsModal")) closeSettings(); });
$("autoFailClose").addEventListener("click", closeAutoFailures);
$("autoFailOk").addEventListener("click", closeAutoFailures);
$("autoFailModal").addEventListener("click", function (e) { if (e.target === $("autoFailModal")) closeAutoFailures(); });
document.addEventListener("keydown", function (e) {
  const meta = e.metaKey || e.ctrlKey;
  if (meta && (e.key === "k" || e.key === "K")) {
    e.preventDefault();
    $("search").focus();
    return;
  }
  if (e.key === "Escape") {
    if (!$("autoFailModal").classList.contains("hidden")) { closeAutoFailures(); return; }
    if (!$("settingsModal").classList.contains("hidden")) { closeSettings(); return; }
    if (!$("orderModal").classList.contains("hidden")) { closeOrder(); return; }
    if ($("search") === document.activeElement) { $("search").blur(); }
    if (curQuery) {
      curQuery = "";
      $("search").value = "";
      if (lastSnap) { const m = buildDecByOpt(lastSnap); renderQueue(lastSnap, m); renderDoc(lastSnap, m); }
    }
    return;
  }
  if (!meta) return;
  if (e.key === "Enter") {
    e.preventDefault();
    const b = document.querySelector("#rail .stamp-btn.is-green:not(:disabled)");
    if (b) b.click();
  }
  if (e.key === "d" || e.key === "D") {
    e.preventDefault();
    const b = document.querySelector("#rail .stamp-btn.is-amber:not(:disabled)");
    if (b) b.click();
  }
});

restorePrefs();
syncChips();
updateSortUI();
updateClock();
setInterval(updateClock, 1000);
$("doc").classList.add("is-empty");
$("log").classList.add("hidden");
$("doc").appendChild(emptyDoc("正在拉取行情…", "连接 OKX 行情与 DCY 产品列表"));
$("rail").appendChild(monoLabel("DECIDE"));
$("log").appendChild(logCard("DECISION LOG", "WAITING", ["等待第一帧行情…"]));
load();
connectWS();
if (location.hash === "#settings") setTimeout(openSettings, 200);
setInterval(function () { if (!ws) load(); }, 15000);
</script>
</body>
</html>
`

// New 组装 HTTP server：addr 监听；fetcher 负责拉取决策快照。
// 默认只读；WithTrader 后开放 /api/order/option 与 /api/order/dcd。
func New(fetcher SnapshotFetcher, uly string, addr string, opts ...ServerOption) *http.Server {
	var cfg serverConfig
	for _, o := range opts {
		o(&cfg)
	}
	cfg.settingsMu = &sync.Mutex{}
	c := &cachedFetcher{f: fetcher, entries: make(map[string]cacheEntry, 4)}
	if cfg.trader != nil && cfg.autoOn {
		cfg.auto = newAutoEngine(cfg.trader, c, uly, cfg.autoParams, cfg.autoDefault, cfg.failures)
		go cfg.auto.run()
	}
	// Web 端可能被多标签/多设备同时操作：下单串行化，避免并发提交
	// 交叉写审计/纸面单，也让「先校验决策、再提交」保持原子语义。
	var orderMu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.ReplaceAll(indexHTML, "__DEFAULT_ULY__", uly)))
	})
	mux.HandleFunc("/api/decisions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		snapUly := q.Get("uly")
		if snapUly == "" {
			snapUly = uly
		}
		day, side, settle, err := parseFilters(q)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		snap, err := c.snapshot(snapUly)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, makeResponse(snap, day, side, settle, cfg))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		snapUly := q.Get("uly")
		if snapUly == "" {
			snapUly = uly
		}
		day, side, settle, err := parseFilters(q)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		up := websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true }, // 本机看板，无跨站下单能力
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(1024)

		// 读泵只用于感知客户端关闭 / 处理控制帧；页面不会发送业务消息。
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		send := func() bool {
			snap, err := c.snapshot(snapUly)
			if err != nil {
				_ = conn.WriteJSON(map[string]any{"error": err.Error()})
				return true
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			return conn.WriteJSON(makeResponse(snap, day, side, settle, cfg)) == nil
		}
		if !send() {
			return
		}
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-done:
				return
			case <-t.C:
				if !send() {
					return
				}
			}
		}
	})
	mux.HandleFunc("/api/order/option", func(w http.ResponseWriter, r *http.Request) {
		if cfg.trader == nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Web 下单未启用（看板以 --read-only 启动）"})
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req struct {
			Uly    string  `json:"uly"`
			InstID string  `json:"instId"`
			Sz     float64 `json:"sz"`
			Px     float64 `json:"px"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体需为 JSON: " + err.Error()})
			return
		}
		req.InstID = strings.TrimSpace(req.InstID)
		if req.InstID == "" || req.Sz <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "instId 不能为空且 sz 必须为正"})
			return
		}
		uly := orderUly(req.Uly, uly)
		if !validUly(uly) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uly 需形如 BTC-USDT"})
			return
		}
		orderMu.Lock()
		defer orderMu.Unlock()
		snap, err := c.snapshot(uly)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		if snap == nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "快照为空"})
			return
		}
		// 默认与 TUI 一致：只允许对当前明确建议「卖出期权」的合约下单，
		// 防止绕过实值档过滤 / 期限窗口直接 POST 任意合约。
		// 交易启用时，期权链里「有买盘」的合约也可手动卖出。
		if !hasBuyOptionDecision(snap, req.InstID) && !optionSellable(snap, req.InstID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "该合约不在当前期权链或没有买盘（实值档已被过滤）"})
			return
		}
		out, err := cfg.trader.SubmitOption(req.InstID, req.Sz, req.Px)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, orderResponseFrom(out))
	})
	mux.HandleFunc("/api/order/dcd", func(w http.ResponseWriter, r *http.Request) {
		if cfg.trader == nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Web 下单未启用（看板以 --read-only 启动）"})
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req struct {
			Uly       string  `json:"uly"`
			ProductID string  `json:"productId"`
			Amt       float64 `json:"amt"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体需为 JSON: " + err.Error()})
			return
		}
		req.ProductID = strings.TrimSpace(req.ProductID)
		if req.ProductID == "" || req.Amt <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "productId 不能为空且 amt 必须为正"})
			return
		}
		uly := orderUly(req.Uly, uly)
		if !validUly(uly) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uly 需形如 BTC-USDT"})
			return
		}
		orderMu.Lock()
		defer orderMu.Unlock()
		snap, err := c.snapshot(uly)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		if snap == nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "快照为空"})
			return
		}
		// 手动申购：只要产品仍在当前快照里就允许（不可对齐/无建议的产品也能
		// 手动申购）；自动执行仍只挑 subscribe_dual 建议。
		prod, ok := snap.Products[req.ProductID]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "产品快照缺失（DCY 未接入或已下架），请刷新"})
			return
		}
		out, err := cfg.trader.SubmitDCD(prod, req.Amt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, orderResponseFrom(out))
	})
	mux.HandleFunc("/api/auto", func(w http.ResponseWriter, r *http.Request) {
		if cfg.auto == nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "自动交易未启用（--read-only 或未装配执行器）"})
			return
		}
		if r.Method == http.MethodGet {
			enabled, note, count := cfg.auto.Status()
			writeJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "note": note, "count": count})
			return
		}
		if r.Method != http.MethodPost || !requireJSON(w, r) {
			return
		}
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil || req.Enabled == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体需为 {\"enabled\": true|false}"})
			return
		}
		cfg.auto.SetEnabled(*req.Enabled)
		// 开关落盘：重启后 Web/TUI 都沿用最后一次选择（live 模式启动时仍强制关闭）。
		if cfg.settings != nil {
			cfg.settingsMu.Lock()
			if st, err := cfg.settings.LoadSettings(); err == nil {
				st.AutoTrade = *req.Enabled
				_ = cfg.settings.SaveSettings(st)
			}
			cfg.settingsMu.Unlock()
		}
		enabled, note, count := cfg.auto.Status()
		writeJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "note": note, "count": count})
	})
	// 自动失败记录：最新在前，最多 store.AutoFailureLimit 条。
	mux.HandleFunc("/api/auto/failures", func(w http.ResponseWriter, r *http.Request) {
		if cfg.auto == nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "自动交易未启用（--read-only 或未装配执行器）"})
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
			return
		}
		fails := cfg.auto.Failures()
		if fails == nil {
			fails = []store.AutoFailure{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"limit":    store.AutoFailureLimit,
			"count":    len(fails),
			"failures": fails,
		})
	})
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		if cfg.settings == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "设置未启用（未接入配置存储）"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			st, err := cfg.settings.LoadSettings()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读取设置失败: " + err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, makeSettingsResponse(cfg, st))
		case http.MethodPost:
			handleSaveSettings(w, r, cfg, c)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET / POST"})
		}
	})
	var handler http.Handler = mux
	if cfg.auth != nil {
		handler = cfg.auth.wrap(mux)
	}
	return &http.Server{Addr: addr, Handler: handler}
}

// handleSaveSettings 合并保存设置：只覆盖请求里出现的字段，校验后落盘并热更新。
func handleSaveSettings(w http.ResponseWriter, r *http.Request, cfg serverConfig, c *cachedFetcher) {
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		CapitalUSDT     *float64 `json:"capitalUsdt"`
		MinDiffPct      *float64 `json:"minDiffPct"`
		MinOptionEdgePP *float64 `json:"minOptionEdgePp"`
		MinAbsEdgePct   *float64 `json:"minAbsEdgePct"`
		MaxStrikeGapPct *float64 `json:"maxStrikeGapPct"`
		AutoTrade       *bool    `json:"autoTrade"`
		NewAuthCode     *string  `json:"newAuthCode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体需为 JSON: " + err.Error()})
		return
	}
	if cfg.settingsMu != nil {
		cfg.settingsMu.Lock()
		defer cfg.settingsMu.Unlock()
	}
	st, err := cfg.settings.LoadSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读取设置失败: " + err.Error()})
		return
	}
	bad := func(msg string) { writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg}) }

	if req.CapitalUSDT != nil {
		v := *req.CapitalUSDT
		if v < 1 || v > 1e9 {
			bad("资金总额需在 1 – 1,000,000,000 USDT 之间")
			return
		}
		st.CapitalUSDT = v
	}
	if req.MinDiffPct != nil {
		v := *req.MinDiffPct
		if v < 0 || v > 1000 {
			bad("差值比例需在 0 – 1000% 之间（0 = 不设门槛）")
			return
		}
		st.MinDiffPct = v
	}
	if req.MinOptionEdgePP != nil {
		v := *req.MinOptionEdgePP
		if v < 1 || v > 100 {
			bad("期权边际阈值需在 1 – 100 pp 之间")
			return
		}
		st.MinOptionEdgePP = v
	}
	if req.MinAbsEdgePct != nil {
		v := *req.MinAbsEdgePct
		if v < 0.01 || v > 100 {
			bad("单笔绝对边际需在 0.01 – 100% 之间")
			return
		}
		st.MinAbsEdgePct = v
	}
	if req.MaxStrikeGapPct != nil {
		v := *req.MaxStrikeGapPct
		if v < 0.01 || v > 50 {
			bad("行权价最大偏差需在 0.01 – 50% 之间")
			return
		}
		st.MaxStrikeGapPct = v
	}
	if req.AutoTrade != nil {
		st.AutoTrade = *req.AutoTrade
	}

	relogin := false
	if req.NewAuthCode != nil && strings.TrimSpace(*req.NewAuthCode) != "" {
		code := strings.TrimSpace(*req.NewAuthCode)
		if len(code) < 4 || len(code) > 64 {
			bad("新授权码长度需为 4 – 64 位")
			return
		}
		if cfg.auth == nil {
			bad("当前未启用 Web 授权码")
			return
		}
		st.WebAuthCodeHash = HashCode(code)
		relogin = true
	}

	if err := cfg.settings.SaveSettings(st); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存设置失败: " + err.Error()})
		return
	}
	applySettings(cfg, c, st)
	if relogin {
		// 改码即注销所有会话：本次响应后页面会跳回 /login。
		_ = cfg.auth.setHash(st.WebAuthCodeHash)
	}
	resp := makeSettingsResponse(cfg, st)
	resp.Relogin = relogin
	writeJSON(w, http.StatusOK, resp)
}

// paramsFromSettings settings.json → 策略参数（与 TUI 的映射保持一致）。
func paramsFromSettings(st store.Settings, cfg serverConfig) strategy.Params {
	p := strategy.DefaultParams()
	p.MinOptionEdge = st.MinOptionEdgePP / 100
	p.MinAbsEdge = st.MinAbsEdgePct / 100
	p.MaxStrikeGapPct = st.MaxStrikeGapPct
	p.AutoMinDiff = st.MinDiffPct / 100
	if cfg.trader != nil {
		if lim := cfg.trader.State().Limits.PerOrderUSDT; lim > 0 {
			p.CapitalCap = lim
		}
	}
	return p
}

// applySettings 把设置热更新到风控（资金总额）、行情层（阈值）与自动引擎。
func applySettings(cfg serverConfig, c *cachedFetcher, st store.Settings) {
	if cfg.trader != nil {
		if cs, ok := cfg.trader.(interface{ SetCapital(float64) }); ok {
			cs.SetCapital(st.CapitalUSDT)
		}
	}
	p := paramsFromSettings(st, cfg)
	c.SetParams(p)
	if cfg.auto != nil {
		cfg.auto.SetParams(p)
		cfg.auto.SetEnabled(st.AutoTrade)
	}
}

func makeSettingsResponse(cfg serverConfig, st store.Settings) settingsResponse {
	resp := settingsResponse{
		CapitalUSDT:     st.CapitalUSDT,
		MinDiffPct:      st.MinDiffPct,
		MinOptionEdgePP: st.MinOptionEdgePP,
		MinAbsEdgePct:   st.MinAbsEdgePct,
		MaxStrikeGapPct: st.MaxStrikeGapPct,
		AuthEnabled:     cfg.auth != nil,
	}
	if cfg.trader != nil {
		g := cfg.trader.State()
		resp.Limits = g.Limits
		resp.Guard = &g
	}
	if cfg.auto != nil {
		resp.AutoAvailable = true
		resp.AutoTrade, _, _ = cfg.auto.Status()
	} else {
		resp.AutoTrade = st.AutoTrade
	}
	return resp
}

// parseFilters 解析 Web 页面/WS 共用的筛选参数。
func parseFilters(q url.Values) (time.Time, rune, rune, error) {
	var day time.Time
	if expStr := q.Get("exp"); expStr != "" {
		t, err := strategy.ParseExpiry(expStr)
		if err != nil {
			return day, 0, 0, err
		}
		day = t
	}
	var side rune
	switch s := strings.ToUpper(strings.TrimSpace(q.Get("side"))); s {
	case "":
	case "C":
		side = 'C'
	case "P":
		side = 'P'
	default:
		return day, 0, 0, fmt.Errorf("side 只能是 C（CALL）或 P（PUT）")
	}
	var settle rune
	switch s := strings.ToUpper(strings.TrimSpace(q.Get("settle"))); s {
	case "", "ALL":
	case "B", "COIN":
		settle = 'B'
	case "U", "USDT":
		settle = 'U'
	default:
		return day, 0, 0, fmt.Errorf("settle 只能是 B（币本位）或 U（U本位）")
	}
	return day, side, settle, nil
}

// makeResponse 在快照上应用筛选并补充执行器状态（REST / WS 共用）。
func makeResponse(snap *market.Snapshot, day time.Time, side, settle rune, cfg serverConfig) decisionsResponse {
	resp := decisionsResponse{
		Snapshot:     market.FilterSnapshot(snap, market.Filter{Day: day, Side: side, Settle: settle}),
		Mode:         "read-only",
		TradeEnabled: cfg.trader != nil,
		ManualTrade:  cfg.trader != nil,
		AuthEnabled:  cfg.auth != nil,
	}
	if cfg.trader != nil {
		resp.Mode = string(cfg.trader.Mode())
		g := cfg.trader.State()
		resp.Guard = &g
	}
	if cfg.auto != nil {
		enabled, note, count := cfg.auto.Status()
		resp.AutoAvailable = true
		resp.AutoTrade = enabled
		resp.AutoNote = note
		resp.AutoCount = count
		resp.AutoFailures = cfg.auto.Failures()
	}
	return resp
}

// requireJSON 只接受 POST + application/json：既统一契约，也避免跨站简单表单
// 直接触发下单（跨域 JSON 请求会先走 CORS 预检，而本服务不返回 CORS 头）。
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return false
	}
	if ct := strings.ToLower(r.Header.Get("Content-Type")); !strings.HasPrefix(ct, "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type 需为 application/json"})
		return false
	}
	return true
}

// orderUly 下单使用的标的：空值回落启动参数；仅接受 {BASE}-USDT 形态。
func orderUly(req, def string) string {
	u := strings.ToUpper(strings.TrimSpace(req))
	if u == "" {
		u = strings.ToUpper(def)
	}
	return u
}

// validUly 限制为 {BASE}-USDT（页面只提供 BTC/ETH/SOL，避免任意标的打上游）。
func validUly(u string) bool {
	if !strings.HasSuffix(u, "-USDT") {
		return false
	}
	base := strings.TrimSuffix(u, "-USDT")
	if base == "" || len(base) > 10 {
		return false
	}
	for _, r := range base {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func hasBuyOptionDecision(snap *market.Snapshot, instID string) bool {
	for _, d := range snap.Decisions {
		if d.Action == strategy.ActionBuyOption && d.Row.OptInstID == instID {
			return true
		}
	}
	return false
}

// optionSellable 判断该合约是否在期权链里且当前有买盘（手动卖出准入）。
// 实值档已在 RankOptions 过滤，这里只保证「确实存在且卖得掉」。
func optionSellable(snap *market.Snapshot, instID string) bool {
	for _, r := range snap.OptionRows {
		if r.InstID == instID && strings.TrimSpace(r.BidPx) != "" {
			return true
		}
	}
	return false
}

func orderResponseFrom(o executor.Outcome) orderResponse {
	return orderResponse{
		OK: true, Mode: string(o.Mode), Kind: o.Kind, Ref: o.Ref,
		OrdID: o.OrdID, QuoteID: o.QuoteID, Amt: o.Amt, InvestCcy: o.InvestCcy,
		AmtUSDT: o.AmtUSDT, Premium: o.Premium, Sim: o.Sim, Detail: o.Detail,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
