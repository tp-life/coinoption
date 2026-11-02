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
	SubmitDCD(p okx.DCDProduct, amt float64) (executor.Outcome, error)
}

// ServerOption 可选能力（当前只有手动下单）。
type ServerOption func(*serverConfig)

type serverConfig struct {
	trader Trader
}

// WithTrader 启用 Web 手动下单（不传 = 只读，与旧行为一致）。
func WithTrader(t Trader) ServerOption {
	return func(c *serverConfig) { c.trader = t }
}

// decisionsResponse 在快照字段之上补充执行器状态（保持原有字段平铺）。
type decisionsResponse struct {
	*market.Snapshot
	Mode         string               `json:"mode"`
	TradeEnabled bool                 `json:"tradeEnabled"`
	Guard        *executor.GuardState `json:"guard,omitempty"`
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

// cachedFetcher 带 TTL 的快照缓存（单锁串行化，个人工具场景足够）。
type cachedFetcher struct {
	f       SnapshotFetcher
	mu      sync.Mutex
	entries map[string]cacheEntry
}

func (c *cachedFetcher) snapshot(uly string) (*market.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[uly]; ok && time.Since(e.at) < cacheTTL {
		return e.snap, nil
	}
	snap, err := c.f.FetchSnapshot(uly)
	if err != nil {
		return nil, err
	}
	c.entries[uly] = cacheEntry{snap: snap, at: time.Now()}
	return snap, nil
}

// indexHTML 决策看板页（内嵌，无需外部静态文件）。
const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>coinoption · OKX 双币赢 vs 期权 APR 对比</title>
<style>
  :root { --bg:#0b0e14; --panel:#12161f; --line:#1f2633; --txt:#d7dce6; --mut:#8b93a5; --ok:#3fb950; --warn:#d29922; --err:#f85149; --accent:#58a6ff; }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--txt); font:13px/1.5 ui-monospace,"SF Mono",Menlo,Consolas,monospace; }
  header { padding:14px 20px; border-bottom:1px solid var(--line); background:var(--panel); }
  header h1 { font-size:15px; margin:0 0 6px; font-weight:600; }
  header .sub { color:var(--mut); font-size:12px; }
  .controls { padding:10px 20px; border-bottom:1px solid var(--line); display:flex; gap:8px; align-items:center; flex-wrap:wrap; }
  .controls .lbl { color:var(--mut); font-size:12px; }
  .controls .sep { color:var(--line); }
  .controls button { background:#0d1117; color:var(--txt); border:1px solid var(--line); padding:4px 12px; border-radius:4px; cursor:pointer; font-family:inherit; font-size:12px; }
  .controls button:hover { border-color:var(--accent); }
  .controls button.on { border-color:var(--accent); color:var(--accent); }
  .controls button.side.on[data-side="C"] { border-color:var(--ok); color:var(--ok); }
  .controls button.side.on[data-side="P"] { border-color:var(--err); color:var(--err); }
  .controls button.day { padding:4px 10px; }
  .controls input[type=date] { background:#0d1117; color:var(--txt); border:1px solid var(--line); padding:4px 8px; border-radius:4px; font-family:inherit; font-size:12px; }
  .controls .status { color:var(--warn); font-size:12px; }
  .legend { padding:6px 20px; border-bottom:1px solid var(--line); color:var(--mut); font-size:12px; }
  .legend .call { color:var(--ok); font-weight:600; } .legend .put { color:var(--err); font-weight:600; }
  .legend .sell { color:var(--ok); } .legend .sub { color:var(--accent); }
  main { padding:12px 20px 20px; }
  .wrap { overflow-x:auto; }
  table { border-collapse: collapse; width:100%; }
  th, td { border:1px solid var(--line); padding:6px 10px; text-align:right; white-space:nowrap; }
  th { background:#0d1117; color:var(--mut); font-weight:600; position:sticky; top:0; z-index:1; }
  td:nth-child(1), th:nth-child(1), td:nth-child(2), th:nth-child(2) { text-align:left; }
  #decWrap td:nth-child(3), #decWrap th:nth-child(3) { text-align:left; }
  tbody tr:hover { background:#161b26; }
  .buy { color:var(--ok); font-weight:600; } .dual { color:var(--accent); font-weight:600; } .none { color:var(--mut); } .unaligned { color:var(--warn); }
  .pos { color:var(--ok); } .neg { color:var(--mut); }
  .empty td { text-align:center; color:var(--mut); padding:24px; }
  .tabs { display:flex; gap:8px; align-items:flex-end; margin:0 0 10px; border-bottom:1px solid var(--line); }
  .tabs .tab { background:transparent; color:var(--mut); border:1px solid transparent; border-bottom:none; padding:6px 14px; border-radius:6px 6px 0 0; cursor:pointer; font-family:inherit; font-size:13px; }
  .tabs .tab:hover { color:var(--txt); }
  .tabs .tab.on { color:var(--accent); border-color:var(--line); background:var(--panel); }
  .tabs .tab .n { color:var(--mut); margin-left:6px; font-size:11px; }
  .tabs .tabhint { color:var(--mut); font-size:11px; margin-left:auto; padding-bottom:7px; }
  .hidden { display:none; }
  .tradebar { padding:6px 20px; border-bottom:1px solid var(--line); color:var(--mut); font-size:12px; }
  .tradebar.on { color:var(--ok); }
  .tradebar.live { color:var(--err); font-weight:600; }
  .status.ok { color:var(--ok); }
  button.ord { background:#0d1117; color:var(--accent); border:1px solid var(--line); padding:2px 10px; border-radius:4px; cursor:pointer; font-family:inherit; font-size:12px; }
  button.ord:hover { border-color:var(--accent); }
  .modal { position:fixed; inset:0; background:rgba(0,0,0,.62); display:flex; align-items:center; justify-content:center; z-index:10; }
  .modal.hidden { display:none; }
  .dialog { background:var(--panel); border:1px solid var(--line); border-radius:8px; padding:18px 20px; width:min(480px,92vw); box-shadow:0 10px 40px rgba(0,0,0,.5); }
  .dialog h3 { margin:0 0 10px; font-size:14px; color:var(--accent); }
  .ominfo { font-size:12px; line-height:1.9; margin-bottom:10px; }
  .omfield { display:flex; gap:10px; align-items:center; font-size:12px; color:var(--mut); }
  .omfield input { flex:1; background:#0d1117; color:var(--txt); border:1px solid var(--line); padding:6px 8px; border-radius:4px; font-family:inherit; font-size:13px; }
  .omnote { margin-top:10px; font-size:12px; color:var(--mut); line-height:1.6; }
  .omnote.err { color:var(--err); }
  .ombtns { display:flex; justify-content:flex-end; gap:8px; margin-top:16px; }
  .ombtns button { background:#0d1117; color:var(--txt); border:1px solid var(--line); padding:6px 16px; border-radius:4px; cursor:pointer; font-family:inherit; font-size:12px; }
  .ombtns button.primary { border-color:var(--accent); color:var(--accent); }
  .ombtns button:disabled { opacity:.5; cursor:default; }
</style>
</head>
<body>
<header>
  <h1>◈ coinoption · OKX 双币赢 vs 期权 APR 对比</h1>
  <div class="sub" id="meta">加载中…</div>
</header>
<div class="controls">
  <span class="lbl">标的</span>
  <button class="cur" data-uly="BTC-USDT">BTC</button>
  <button class="cur" data-uly="ETH-USDT">ETH</button>
  <button class="cur" data-uly="SOL-USDT">SOL</button>
  <span class="sep">|</span>
  <span class="lbl">方向</span>
  <button class="side" data-side="">全部</button>
  <button class="side" data-side="C">CALL</button>
  <button class="side" data-side="P">PUT</button>
  <span class="sep">|</span>
  <span class="lbl">本位</span>
  <button class="settle" data-settle="">全部</button>
  <button class="settle" data-settle="B">币本位</button>
  <button class="settle" data-settle="U">U本位</button>
  <span class="sep">|</span>
  <span class="lbl">到期(UTC)</span>
  <span id="days"></span>
  <input type="date" id="exp" value="" />
  <button id="expApply">筛选</button>
  <button id="expClear">全部</button>
  <span class="status" id="status"></span>
</div>
<div class="legend">
  <span class="call">CALL 看涨</span> / <span class="put">PUT 看跌</span>
  <span class="sep">｜</span>
  <span class="sell">卖CALL / 卖PUT = SELL（卖出期权，收权利金）</span>
  <span class="sep">｜</span>
  <span class="sub">申购DCY</span><span>（高卖→到期卖币 / 低买→到期买币）</span>
  <span class="sep">｜</span>
  <span>本工具仅卖期权，不买入期权</span>
</div>
<div class="tradebar" id="tradeBar"></div>
<main>
  <div class="tabs">
    <button class="tab on" data-tab="decisions">对比决策 <span class="n" id="decN">0</span></button>
    <button class="tab" data-tab="chain">完整期权链 <span class="n" id="optN">0</span></button>
    <span class="tabhint" id="tabHint"></span>
  </div>
  <div class="wrap" id="decWrap">
    <table>
      <thead>
        <tr>
          <th>操作</th><th>DCY 产品</th><th>对应期权</th><th>到期(UTC)</th><th>方向</th><th>本位</th><th>行权价</th>
          <th>距现价%</th><th>买价</th><th>DCY APR%</th><th>期权 APR%</th><th>差%</th><th>偏差%</th><th>天</th><th>原因</th><th>建议金额</th><th>下单</th>
        </tr>
      </thead>
      <tbody id="decRows"></tbody>
    </table>
  </div>
  <div class="wrap hidden" id="chainWrap">
    <table>
      <thead>
        <tr>
          <th>操作</th><th>期权合约</th><th>到期(UTC)</th><th>方向</th><th>本位</th><th>行权价</th>
          <th>距现价%</th><th>买价</th><th>卖价</th><th>期权 APR%</th><th>天</th><th>对应 DCY</th><th>DCY APR%</th><th>差%</th>
        </tr>
      </thead>
      <tbody id="optRows"></tbody>
    </table>
  </div>
</main>
<div class="modal hidden" id="orderModal">
  <div class="dialog">
    <h3 id="omTitle">下单</h3>
    <div class="ominfo" id="omInfo"></div>
    <label class="omfield"><span id="omAmtLabel">数量</span><input id="omAmt" type="number" step="any" min="0"></label>
    <div class="omnote" id="omNote"></div>
    <div class="ombtns">
      <button id="omCancel">取消</button>
      <button id="omSubmit" class="primary">确认下单</button>
    </div>
  </div>
</div>
<script>
let curUly = "__DEFAULT_ULY__";
let curExp = "";
let curSide = "";
let curSettle = "";
let curTab = "decisions";
let actionCounts = { buy_option: 0, subscribe_dual: 0, unaligned: 0 };
let tradeEnabled = false;
let curMode = "read-only";
let curGuard = null;
let pending = null;
let submitting = false;
let loading = false;
let ws = null;
let wsRetry = 0;
let wsTimer = null;
let wsDown = false;

function $(id) { return document.getElementById(id); }

function pad(n) { return String(n).padStart(2, "0"); }

function expStr(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "-";
  return d.getUTCFullYear() + "-" + pad(d.getUTCMonth() + 1) + "-" + pad(d.getUTCDate()) +
    " " + pad(d.getUTCHours()) + ":" + pad(d.getUTCMinutes());
}

function instDay(instId) {
  const p = (instId || "").split("-");
  if (p.length < 4 || p[2].length !== 6) return "";
  return "20" + p[2].slice(0, 2) + "-" + p[2].slice(2, 4) + "-" + p[2].slice(4, 6);
}

function optDay(instId) {
  const day = instDay(instId);
  return day ? expStr(day + "T08:00:00Z") : "-";
}

function pct(v) { return v == null ? "-" : (v * 100).toFixed(2); }

// Go 的 rune（'C'/'P'）序列化成数字 67/80，这里同时兼容数字与字符串。
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
  const c = typeof t === "number" ? String.fromCharCode(t) : String(t || "");
  const u = c.toUpperCase();
  return u === "C" ? "高卖" : u === "P" ? "低买" : "—";
}
function actTxt(d) {
  if (d.action === "buy_option") return "卖" + optTypeName(d.row && d.row.optType);
  if (d.action === "subscribe_dual") return "申购" + dcySideName(d.row && d.row.optType);
  if (d.action === "unaligned") return "不可对齐";
  return "—";
}
function cls(a) { return a === "buy_option" ? "buy" : a === "subscribe_dual" ? "dual" : a === "unaligned" ? "unaligned" : "none"; }
function typeCls(t) { return optTypeName(t) === "CALL" ? "buy" : optTypeName(t) === "PUT" ? "unaligned" : "none"; }
function reasonTxt(code) {
  switch (code) {
    case "option_apr_higher": return "期权更优";
    case "diff_small": return "差距不足";
    case "option_apr_worse": return "期权更差";
    case "strike_gap_too_big": return "行权价偏差过大";
    case "strike_wrong_side": return "行权价方向异常";
    case "no_option_quote": return "无期权报价";
    case "out_of_tenor": return "不在期限窗口";
  }
  return code || "-";
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

function renderDays(days) {
  const box = $("days");
  box.innerHTML = "";
  for (const [day, n] of days) {
    const b = document.createElement("button");
    b.className = "day" + (day === curExp ? " on" : "");
    b.textContent = day.slice(5) + "(" + n + ")";
    b.addEventListener("click", function () { setExp(day); });
    box.appendChild(b);
  }
}

function renderMeta(j) {
  const t = j.at ? new Date(j.at) : null;
  curMode = j.mode || "read-only";
  tradeEnabled = !!j.tradeEnabled;
  curGuard = j.guard || null;
  renderTradeBar();
  $("meta").textContent = "标的 " + curUly + " · 现货 " + (j.spotPrice ?? "-") +
    " · 期权 " + (j.optionCount ?? "-") + "（" + (j.optionUly || "-") + "）" +
    " · DCY " + (j.dcyAvailable ? (j.dcdCount ?? 0) + " 个产品" : "未接入") +
    " · 模式 " + curMode + (tradeEnabled ? "（Web 下单）" : "（Web 只读）") +
    " · 组合 " + curUly + " · " + (curSide || "全部方向") + " · " + (curSettle ? settleName(curSettle) : "全部本位") + " · " + (curExp || "全部到期") +
    " · 更新 " + (t ? t.toLocaleTimeString("zh-CN", { hour12: false }) : "-") +
    " · " + (j.wsStatus === "degraded" ? "WS 降级" : (j.wsConnected ? "WS 实时" : (j.wsStatus === "reconnecting" ? "WS 重连中" : (j.wsStatus === "connecting" ? "WS 连接中" : "REST")))) +
    (j.warn ? " · ⚠ " + j.warn : "");
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

function renderTradeBar() {
  const b = $("tradeBar");
  if (!tradeEnabled) {
    b.className = "tradebar";
    b.textContent = "Web 只读：启动时加 --web-trade 才能在页面下单（TUI 下单不受影响）";
    return;
  }
  let s = "Web 手动下单已启用 · 模式 " + curMode;
  if (curGuard && curGuard.limits) {
    s += " · 敞口 " + Math.round(curGuard.exposureUsdt || 0) + "/" + Math.round(curGuard.limits.totalExposureUsdt || 0) + " USDT";
    s += " · 单笔上限 " + Math.round(curGuard.limits.perOrderUsdt || 0) + " USDT";
    s += " · 今日 " + (curGuard.dayCount || 0) + "/" + (curGuard.limits.maxSessionsPerDay || 0) + " 笔";
  }
  if (curMode === "live") {
    b.className = "tradebar live";
    s = "⚠ 实盘下单已启用 · " + s + " · 能访问本页的人都可以下单，请确认监听地址安全";
  } else {
    b.className = "tradebar on";
  }
  b.textContent = s;
}

function renderRows(j) {
  const decisions = j.decisions || [];
  const optionRows = j.optionRows || [];
  const spot = Number(j.spotPrice) || 0;
  // 期权合约 → 决策（用于在期权链里标出「对应 DCY」）。
  const decByOpt = new Map();
  for (const d of decisions) {
    const r = d.row || {};
    if (r.optInstId && !decByOpt.has(r.optInstId)) decByOpt.set(r.optInstId, d);
  }
  renderDecisions(decisions, spot);
  renderChain(optionRows, decByOpt, spot);
  $("decN").textContent = decisions.length;
  $("optN").textContent = optionRows.length;
  actionCounts = { buy_option: 0, subscribe_dual: 0, unaligned: 0 };
  for (const d of decisions) {
    if (actionCounts[d.action] != null) actionCounts[d.action]++;
  }
  setTab(curTab);
}

function distInfo(isDec, r, spot) {
  let abs = null, p = null;
  if (isDec && spot > 0 && r.strike != null) {
    abs = r.strike - spot;
    p = abs / spot * 100;
  } else if (!isDec && r.strikeDiffPct != null && r.spotPx > 0) {
    abs = r.strikeDiff;
    p = r.strikeDiffPct;
  }
  return {
    txt: p == null ? "-" : (p >= 0 ? "+" : "") + p.toFixed(2) + "%",
    cls: p == null ? "" : (p >= 0 ? "pos" : "neg"),
    title: abs == null ? "" : "绝对差 " + (abs >= 0 ? "+" : "") + Math.round(abs),
  };
}

function addRow(tb, cells) {
  const tr = document.createElement("tr");
  for (const c of cells) {
    const td = document.createElement("td");
    if (c.el) td.appendChild(c.el); else td.textContent = c.v;
    if (c.c) td.className = c.c;
    if (c.title) td.title = c.title;
    tr.appendChild(td);
  }
  tb.appendChild(tr);
}

function addEmpty(tb, cols, text) {
  const tr = document.createElement("tr");
  tr.className = "empty";
  const td = document.createElement("td");
  td.colSpan = cols;
  td.textContent = text;
  tr.appendChild(td);
  tb.appendChild(tr);
}

// 主视图：一条 DCY 产品一行，直接给出「DCY APR ↔ 同档期权 APR」的对比结果。
function renderDecisions(decisions, spot) {
  const tb = $("decRows");
  tb.innerHTML = "";
  for (const d of decisions) {
    const r = d.row || {};
    const dist = distInfo(true, r, spot);
    const hasOptBid = !!r.optBidPx;
    addRow(tb, [
      { v: actTxt(d), c: cls(d.action) },
      { v: r.productId || "-" },
      { v: r.optInstId || "-", c: r.optInstId ? "" : "none" },
      { v: expStr(r.expTime) },
      { v: optTypeName(r.optType), c: typeCls(r.optType) },
      { v: settleName(settleKind(r.optSettleCcy, r.optInstId)) },
      { v: r.strike != null ? Math.round(r.strike) : "-" },
      { v: dist.txt, c: dist.cls, title: dist.title },
      { v: hasOptBid ? String(r.optBidPx) : "-" },
      { v: pct(r.dcyApr) },
      { v: hasOptBid ? pct(r.optApr) : "-" },
      { v: hasOptBid ? pct(r.diff) : "-", c: hasOptBid ? (r.diff > 0 ? "pos" : (r.diff < 0 ? "neg" : "")) : "" },
      { v: r.strikeErrPct != null ? r.strikeErrPct.toFixed(2) : "-" },
      { v: r.days != null ? Math.round(r.days) : "-" },
      { v: d.reasonCode ? reasonTxt(d.reasonCode) : "-", c: "none", title: d.reasonCode || "" },
      { v: d.suggestedAmt ? Math.round(d.suggestedAmt) + " " + (r.investCcy || "") : "-" },
      orderCellFor(d),
    ]);
  }
  if (decisions.length === 0) {
    addEmpty(tb, 17, "该组合下没有 DCY 决策（切换「完整期权链」可查看全部期权，或重置筛选）");
  }
}

// 下单列：只有当前决策明确建议的动作才可出现按钮（与 TUI 的 o/d 一致）。
function orderCellFor(d) {
  if (!tradeEnabled) return { v: "—", c: "none" };
  const r = d.row || {};
  if (d.action === "buy_option" && r.optInstId) {
    const b = document.createElement("button");
    b.className = "ord";
    b.textContent = "卖出期权";
    b.addEventListener("click", function () { openOrder("option", d); });
    return { el: b };
  }
  if (d.action === "subscribe_dual" && r.productId) {
    const b = document.createElement("button");
    b.className = "ord";
    b.textContent = "申购 DCY";
    b.addEventListener("click", function () { openOrder("dcd", d); });
    return { el: b };
  }
  return { v: "—", c: "none" };
}

function openOrder(kind, d) {
  if (!tradeEnabled || submitting) return;
  const r = d.row || {};
  pending = { kind: kind, d: d };
  $("omTitle").textContent = kind === "option" ? "卖出期权（SELL · 开空收权利金）" : "申购双币赢（DCY）";
  const info = [];
  if (kind === "option") {
    info.push("合约 " + r.optInstId);
    info.push("方向 " + optTypeName(r.optType) + " · 行权 " + Math.round(r.strike) + " · 到期 " + expStr(r.expTime));
    info.push("买一价 " + (r.optBidPx || "-") + " · 期权 APR " + pct(r.optApr) + "%");
    info.push("按当前买一价挂单（限价）；风控按合约名义折算，超过单笔上限会被拒单");
    const one = Number(r.spot) || 0;
    if (curGuard && curGuard.limits) {
      info.push("风控：单笔上限 " + Math.round(curGuard.limits.perOrderUsdt || 0) +
        " USDT · 剩余敞口 " + Math.round(curGuard.remainingUsdt || 0) + " USDT");
      if (one > 0 && (curGuard.limits.perOrderUsdt || 0) < one) {
        info.push("⚠ 单笔上限低于 1 张名义≈" + Math.round(one) + " USDT，提交会被风控拒单；需用 --limits 放宽单笔/总敞口");
      }
    }
    $("omAmtLabel").textContent = "卖出张数";
    $("omAmt").value = "1";
    $("omAmt").min = "0";
  } else {
    info.push("产品 " + r.productId);
    info.push("方向 " + dcySideName(r.optType) + " · 行权 " + Math.round(r.strike) + " · 到期 " + expStr(r.expTime));
    info.push("DCY APR " + pct(r.dcyApr) + "% · 投资币种 " + (r.investCcy || "-"));
    info.push("可投区间 " + (r.dcyMinSize || "-") + " – " + (r.dcyMaxSize || "-") + " " + (r.investCcy || ""));
    $("omAmtLabel").textContent = "投入 " + (r.investCcy || "");
    const def = Number(d.suggestedAmt || r.dcyMinSize || 0);
    $("omAmt").value = def > 0 ? String(Math.round(def * 10000) / 10000) : "";
    $("omAmt").min = r.dcyMinSize > 0 ? String(r.dcyMinSize) : "0";
  }
  const box = $("omInfo");
  box.innerHTML = "";
  for (const line of info) {
    const div = document.createElement("div");
    div.textContent = line;
    box.appendChild(div);
  }
  $("omNote").className = "omnote";
  $("omNote").textContent = curMode === "live"
    ? "⚠ 实盘模式：确认后将向 OKX 提交真实订单，请再次核对合约与数量。"
    : "当前为 " + curMode + " 模式：不会提交真实订单（sim-okx 的期权走 OKX Demo）。";
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
      body: JSON.stringify(body),
    });
    const j = await resp.json().catch(function () { return {}; });
    if (!resp.ok) throw new Error(j.error || ("HTTP " + resp.status));
    $("orderModal").classList.add("hidden");
    pending = null;
    $("status").className = "status ok";
    $("status").textContent = "✓ " + (j.detail || "已提交");
    setTimeout(refresh, 3000);
  } catch (e) {
    $("omNote").className = "omnote err";
    $("omNote").textContent = "✗ " + e.message;
    $("omSubmit").disabled = false;
  } finally {
    submitting = false;
  }
}

// 参考视图：完整卖方有效期权链；有 DCY 对应档的顺带给出 DCY APR 与差值。
function renderChain(optionRows, decByOpt, spot) {
  const tb = $("optRows");
  tb.innerHTML = "";
  for (const r of optionRows) {
    const dist = distInfo(false, r, spot);
    const d = decByOpt.get(r.instId);
    const hasDcy = !!(d && d.row);
    addRow(tb, [
      { v: optionAct(r), c: "buy" },
      { v: r.instId || "-" },
      { v: optDay(r.instId) },
      { v: optTypeName(r.optType), c: typeCls(r.optType) },
      { v: settleName(settleKind(r.settleCcy, r.instId)) },
      { v: r.strike != null ? Math.round(r.strike) : "-" },
      { v: dist.txt, c: dist.cls, title: dist.title },
      { v: r.bidPx ? String(r.bidPx) : "-" },
      { v: r.askPx ? String(r.askPx) : "-" },
      { v: r.aprValid ? pct(r.optApr) : "-" },
      { v: r.days != null ? Math.round(r.days) : "-" },
      { v: hasDcy ? d.row.productId : "-", c: hasDcy ? "dual" : "none" },
      { v: hasDcy && d.row.dcyApr != null ? pct(d.row.dcyApr) : "-" },
      { v: hasDcy && r.aprValid ? pct(r.optApr - d.row.dcyApr) : "-" },
    ]);
  }
  if (optionRows.length === 0) {
    addEmpty(tb, 14, "该组合下没有期权链数据（重置筛选，或换标的/到期）");
  }
}

function setTab(name) {
  curTab = name === "chain" ? "chain" : "decisions";
  $("decWrap").classList.toggle("hidden", curTab !== "decisions");
  $("chainWrap").classList.toggle("hidden", curTab !== "chain");
  document.querySelectorAll(".tab").forEach(function (b) {
    b.classList.toggle("on", b.dataset.tab === curTab);
  });
  $("tabHint").textContent = curTab === "decisions"
    ? "卖期权 " + actionCounts.buy_option + " · 申购 " + actionCounts.subscribe_dual + " · 不可对齐 " + actionCounts.unaligned
    : "参考视图：完整卖方有效期权链（实值档已过滤）·「对应 DCY」列可回查";
}

// optionAct 期权榜操作列：可卖机会 / 仅有报价（无买盘）。实值档已在榜单过滤。
function optionAct(r) {
  if (!r.aprValid) return "仅报价";
  return "卖出期权";
}

function applySnapshot(j) {
  renderMeta(j);
  renderDays(expiryDays(j.decisions || [], j.optionRows || []));
  renderRows(j);
  $("status").textContent = wsDown ? "WebSocket 断线，重连中…" : "";
}

async function load() {
  if (loading) return;
  loading = true;
  $("status").textContent = "刷新中…";
  const params = new URLSearchParams();
  params.set("uly", curUly);
  if (curExp) params.set("exp", curExp);
  if (curSide) params.set("side", curSide);
  if (curSettle) params.set("settle", curSettle);
  try {
    const r = await fetch("/api/decisions?" + params.toString(), { cache: "no-store" });
    const j = await r.json();
    if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
    applySnapshot(j);
  } catch (e) {
    $("meta").textContent = "加载失败：" + e.message;
    $("status").textContent = "";
  } finally {
    loading = false;
  }
}

// WebSocket 推送：服务端每秒推送当前筛选下的完整快照（与 /api/decisions 同构）。
// 断线自动重连，期间用 REST 兜底；不支持 WS 的环境直接回退轮询。
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
  sock.onopen = function () { wsRetry = 0; wsDown = false; $("status").textContent = ""; };
  sock.onmessage = function (ev) {
    if (ws !== sock) return;
    let j;
    try { j = JSON.parse(ev.data); } catch (e) { return; }
    if (j.error) { $("status").textContent = "WS: " + j.error; return; }
    applySnapshot(j);
  };
  sock.onclose = function () {
    if (ws === sock) ws = null;
    wsDown = true;
    const wait = Math.min(10000, 1000 * Math.pow(2, Math.min(wsRetry, 4)));
    wsRetry++;
    $("status").textContent = "WebSocket 断线，重连中…";
    wsTimer = setTimeout(connectWS, wait);
    load(); // 重连期间 REST 兜底
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
  refresh();
}

function setSettle(settle) {
  curSettle = settle || "";
  refresh();
}

document.querySelectorAll(".cur").forEach(function (b) {
  b.addEventListener("click", function () { curUly = b.dataset.uly; refresh(); });
});
document.querySelectorAll(".tab").forEach(function (b) {
  b.addEventListener("click", function () { setTab(b.dataset.tab); });
});
document.querySelectorAll(".side").forEach(function (b) {
  b.addEventListener("click", function () { setSide(b.dataset.side); });
});
document.querySelectorAll(".settle").forEach(function (b) {
  b.addEventListener("click", function () { setSettle(b.dataset.settle); });
});
$("expApply").addEventListener("click", function () { setExp($("exp").value); });
$("exp").addEventListener("change", function () { setExp($("exp").value); });
$("expClear").addEventListener("click", function () { setExp(""); });
$("omCancel").addEventListener("click", closeOrder);
$("omSubmit").addEventListener("click", submitOrder);
$("omAmt").addEventListener("keydown", function (e) { if (e.key === "Enter") submitOrder(); });
$("orderModal").addEventListener("click", function (e) { if (e.target === $("orderModal")) closeOrder(); });
document.addEventListener("keydown", function (e) {
  if (e.key === "Escape" && !$("orderModal").classList.contains("hidden")) closeOrder();
});
// 首次 REST 渲染后开启 WS；WS 不可用时每 15s 轮询兜底。
load();
connectWS();
setInterval(function () { if (!ws) load(); }, 15000);
</script>
</body>
</html>`

// New 组装 HTTP server：addr 监听；fetcher 负责拉取决策快照。
// 默认只读；WithTrader 后开放 /api/order/option 与 /api/order/dcd。
func New(fetcher SnapshotFetcher, uly string, addr string, opts ...ServerOption) *http.Server {
	var cfg serverConfig
	for _, o := range opts {
		o(&cfg)
	}
	c := &cachedFetcher{f: fetcher, entries: make(map[string]cacheEntry, 4)}
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
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Web 下单未启用（启动时加 --web-trade）"})
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
		// 与 TUI 一致：只允许对当前明确建议「卖出期权」的合约下单，
		// 防止绕过实值档过滤 / 期限窗口直接 POST 任意合约。
		if !hasBuyOptionDecision(snap, req.InstID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "该合约当前没有「卖出期权」建议（请刷新决策后从行内按钮下单）"})
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
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Web 下单未启用（启动时加 --web-trade）"})
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
		if !hasSubscribeDualDecision(snap, req.ProductID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "该产品当前没有「申购双币赢」建议（请刷新决策后从行内按钮下单）"})
			return
		}
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
	return &http.Server{Addr: addr, Handler: mux}
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
	}
	if cfg.trader != nil {
		resp.Mode = string(cfg.trader.Mode())
		g := cfg.trader.State()
		resp.Guard = &g
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

func hasSubscribeDualDecision(snap *market.Snapshot, productID string) bool {
	for _, d := range snap.Decisions {
		if d.Action == strategy.ActionSubscribeDual && d.Row.ProductID == productID {
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
