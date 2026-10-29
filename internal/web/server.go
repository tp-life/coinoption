// Package web 提供 Web 展示服务：GET / 为决策看板页，GET /api/decisions 返回 JSON。
package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/strategy"
)

// SnapshotFetcher 拉取指定标的的快照（*market.Fetcher 实现；便于测试注入）。
type SnapshotFetcher interface {
	FetchSnapshot(uly string) (*market.Snapshot, error)
}

// cacheTTL 快照缓存时长：避免每次页面轮询都打 OKX（DCD 接口限速 1 req/s）。
const cacheTTL = 15 * time.Second

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
  tbody tr:hover { background:#161b26; }
  .buy { color:var(--ok); font-weight:600; } .dual { color:var(--accent); font-weight:600; } .none { color:var(--mut); } .unaligned { color:var(--warn); }
  .pos { color:var(--ok); } .neg { color:var(--mut); }
  .empty td { text-align:center; color:var(--mut); padding:24px; }
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
  <span class="sell">卖=卖出期权收权利金（开空）</span>
  <span class="sep">｜</span>
  <span class="sub">申购=双币赢</span><span>（高卖投入币 / 低买投入 USDT）</span>
</div>
<main>
  <div class="wrap">
    <table>
      <thead>
        <tr>
          <th>操作</th><th>产品/合约</th><th>到期(UTC)</th><th>方向</th><th>行权价</th>
          <th>DCY APR%</th><th>期权 APR%</th><th>差%</th><th>天</th>
          <th>偏差%</th><th>原因</th><th>建议金额</th>
        </tr>
      </thead>
      <tbody id="rows"></tbody>
    </table>
  </div>
</main>
<script>
let curUly = "__DEFAULT_ULY__";
let curExp = "";
let curSide = "";
let loading = false;

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

function optTypeName(t) { return t === "C" || t === "c" ? "CALL" : t === "P" || t === "p" ? "PUT" : "-"; }
function dcySideName(t) { return t === "C" || t === "c" ? "高卖" : t === "P" || t === "p" ? "低买" : "—"; }
function actTxt(d) {
  if (d.action === "buy_option") return "卖" + optTypeName(d.row && d.row.optType);
  if (d.action === "subscribe_dual") return "申购" + dcySideName(d.row && d.row.optType);
  if (d.action === "unaligned") return "不可对齐";
  return "—";
}
function cls(a) { return a === "buy_option" ? "buy" : a === "subscribe_dual" ? "dual" : a === "unaligned" ? "unaligned" : "none"; }
function typeCls(t) { return optTypeName(t) === "CALL" ? "buy" : optTypeName(t) === "PUT" ? "unaligned" : "none"; }

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
  for (const [day, n] of days.slice(0, 12)) {
    const b = document.createElement("button");
    b.className = "day" + (day === curExp ? " on" : "");
    b.textContent = day.slice(5) + "(" + n + ")";
    b.addEventListener("click", function () { setExp(day); });
    box.appendChild(b);
  }
}

function renderMeta(j) {
  const t = j.at ? new Date(j.at) : null;
  $("meta").textContent = "标的 " + curUly + " · 现货 " + (j.spotPrice ?? "-") +
    " · 期权 " + (j.optionCount ?? "-") + "（" + (j.optionUly || "-") + "）" +
    " · DCY " + (j.dcyAvailable ? (j.dcdCount ?? 0) + " 个产品" : "未接入") +
    " · 组合 " + curUly + " · " + (curSide || "全部方向") + " · " + (curExp || "全部到期") +
    " · 更新 " + (t ? t.toLocaleTimeString("zh-CN", { hour12: false }) : "-") +
    (j.warn ? " · ⚠ " + j.warn : "");
  document.querySelectorAll(".cur").forEach(function (b) {
    b.classList.toggle("on", b.dataset.uly === curUly);
  });
  document.querySelectorAll(".side").forEach(function (b) {
    b.classList.toggle("on", (b.dataset.side || "") === curSide);
  });
}

function renderRows(j) {
  const decisions = j.decisions || [];
  const optionRows = j.optionRows || [];
  const rows = decisions.length > 0 ? decisions : optionRows;
  const tb = $("rows");
  tb.innerHTML = "";
  for (const d of rows) {
    const isDec = !!d.row;
    const r = isDec ? d.row : d;
    const tr = document.createElement("tr");
    const cells = [
      { v: isDec ? actTxt(d) : "期权榜", c: isDec ? cls(d.action) : "none" },
      { v: r.productId || r.instId || "-" },
      { v: isDec ? expStr(r.expTime) : optDay(r.instId) },
      { v: optTypeName(r.optType), c: typeCls(r.optType) },
      { v: r.strike != null ? Math.round(r.strike) : "-" },
      { v: isDec ? pct(r.dcyApr) : "-" },
      { v: pct(r.optApr) },
      { v: isDec ? pct(r.diff) : "-", c: isDec ? (r.diff > 0 ? "pos" : (r.diff < 0 ? "neg" : "")) : "" },
      { v: r.days != null ? Math.round(r.days) : "-" },
      { v: isDec && r.strikeErrPct != null ? r.strikeErrPct.toFixed(2) : "-" },
      { v: isDec ? (d.reasonCode || "-") : "options_rank" },
      { v: isDec && d.suggestedAmt ? Math.round(d.suggestedAmt) + " " + (r.investCcy || "") : "-" },
    ];
    for (const c of cells) {
      const td = document.createElement("td");
      td.textContent = c.v;
      if (c.c) td.className = c.c;
      tr.appendChild(td);
    }
    tb.appendChild(tr);
  }
  if (rows.length === 0) {
    const tr = document.createElement("tr");
    tr.className = "empty";
    const td = document.createElement("td");
    td.colSpan = 12;
    const combo = curUly + " · " + (curSide || "全部方向") + " · " + (curExp || "全部到期");
    td.textContent = "组合「" + combo + "」下无决策（试试切换方向/到期，或「全部」重置）";
    tr.appendChild(td);
    tb.appendChild(tr);
  }
}

async function load() {
  if (loading) return;
  loading = true;
  $("status").textContent = "刷新中…";
  const params = new URLSearchParams();
  params.set("uly", curUly);
  if (curExp) params.set("exp", curExp);
  if (curSide) params.set("side", curSide);
  try {
    const r = await fetch("/api/decisions?" + params.toString(), { cache: "no-store" });
    const j = await r.json();
    if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
    renderMeta(j);
    renderDays(expiryDays(j.decisions || [], j.optionRows || []));
    renderRows(j);
    $("status").textContent = "";
  } catch (e) {
    $("meta").textContent = "加载失败：" + e.message;
    $("status").textContent = "";
  } finally {
    loading = false;
  }
}

function setExp(day) {
  curExp = day || "";
  $("exp").value = curExp;
  load();
}

function setSide(side) {
  curSide = side || "";
  load();
}

document.querySelectorAll(".cur").forEach(function (b) {
  b.addEventListener("click", function () { curUly = b.dataset.uly; load(); });
});
document.querySelectorAll(".side").forEach(function (b) {
  b.addEventListener("click", function () { setSide(b.dataset.side); });
});
$("expApply").addEventListener("click", function () { setExp($("exp").value); });
$("exp").addEventListener("change", function () { setExp($("exp").value); });
$("expClear").addEventListener("click", function () { setExp(""); });
setInterval(load, 20000);
load();
</script>
</body>
</html>`

// New 组装 HTTP server：addr 监听；fetcher 负责拉取决策快照。
func New(fetcher SnapshotFetcher, uly string, addr string) *http.Server {
	c := &cachedFetcher{f: fetcher, entries: make(map[string]cacheEntry, 4)}
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
		var day time.Time
		if expStr := q.Get("exp"); expStr != "" {
			t, err := strategy.ParseExpiry(expStr)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
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
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "side 只能是 C（CALL）或 P（PUT）"})
			return
		}
		snap, err := c.snapshot(snapUly)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, market.FilterSnapshot(snap, day, side))
	})
	return &http.Server{Addr: addr, Handler: mux}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
