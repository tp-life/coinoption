// Package web 提供 Web 展示服务：GET / 为决策看板页，GET /api/decisions 返回 JSON。
package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/strategy"
)

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
  .controls .lbl { color:var(--mut); }
  .controls .cur { background:#0d1117; color:var(--txt); border:1px solid var(--line); padding:4px 12px; border-radius:4px; cursor:pointer; font-family:inherit; font-size:12px; }
  .controls .cur.on { border-color: var(--accent); color: var(--accent); }
  .controls input[type=date] { background:#0d1117; color:var(--txt); border:1px solid var(--line); padding:4px 8px; border-radius:4px; font-family:inherit; font-size:12px; }
  .controls .mini { background:#0d1117; color:var(--txt); border:1px solid var(--line); padding:4px 12px; border-radius:4px; cursor:pointer; font-family:inherit; font-size:12px; }
  main { padding:16px 20px; }
  table { border-collapse: collapse; width:100%; }
  th, td { border:1px solid var(--line); padding:6px 10px; text-align:right; white-space:nowrap; }
  th { background:#0d1117; color:var(--mut); font-weight:600; position:sticky; top:0; }
  td:first-child, th:first-child, td:nth-child(2), th:nth-child(2) { text-align:left; }
  tbody tr:hover { background:#161b26; }
  .buy { color: var(--ok); } .dual { color: var(--accent); } .none { color: var(--mut); } .unaligned { color: var(--warn); }
  #meta { margin-bottom:12px; color:var(--mut); font-size:12px; }
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
  <span class="lbl">到期</span>
  <input type="date" id="exp" value="" />
  <button class="mini" id="expApply">筛选</button>
</div>
<main>
  <table>
    <thead>
      <tr><th>建议</th><th>产品/合约</th><th>到期</th><th>DCY APR</th><th>期权 APR</th><th>差</th><th>建议金额</th></tr>
    </thead>
    <tbody id="rows"></tbody>
  </table>
</main>
<script>
function expStr(iso) { return (iso || "").slice(0, 16).replace("T", " "); }
function actTxt(a) { return a === "buy_option" ? "押注期权" : a === "subscribe_dual" ? "投注双币赢" : a === "unaligned" ? "不可对齐" : "—"; }
function cls(a) { return a === "buy_option" ? "buy" : a === "subscribe_dual" ? "dual" : a === "unaligned" ? "unaligned" : "none"; }
async function load(uly, exp) {
  const meta = document.getElementById("meta");
  const params = new URLSearchParams();
  params.set("uly", uly);
  if (exp) params.set("exp", exp);
  try {
    const r = await fetch("/api/decisions?" + params.toString(), { cache: "no-store" });
    const j = await r.json();
    const decisions = j.Decisions || [];
    const optionRows = j.OptionRows || [];
    const rows = decisions.length > 0 ? decisions : optionRows;
    meta.textContent = "标的 " + uly + " · 期权底层 " + (j.OptionUly || "-") + " · 现货 " + (j.SpotPrice ?? "-") +
      " · 期权合约 " + (j.OptionCount ?? "-") + " · DCY 产品 " + (j.DcdCount ?? "-") + " · 到期 " + (exp || "全部") +
      " · 更新 " + (j.At ? new Date(j.At).toLocaleTimeString("zh-CN", { hour12: false }) : "-") +
      (j.Warn ? " · " + j.Warn : "");
    document.querySelectorAll(".cur").forEach(function (b) { b.classList.toggle("on", b.dataset.uly === uly); });
    const tb = document.querySelector("#rows");
    tb.innerHTML = "";
    for (const d of rows) {
      const r = d.Row || d;
      const instId = r.InstID || r.ProductID || "-";
      const expTime = r.ExpTime || null;
      const tr = document.createElement("tr");
      const tds = [
        ["建议", actTxt(d.Action), cls(d.Action)],
        ["产品/合约", instId, ""],
        ["到期", expTime ? expStr(expTime) : "-", ""],
        ["DCY APR", r.DcyAPR != null ? (r.DcyAPR * 100).toFixed(2) + "%" : "-"],
        ["期权 APR", r.OptAPR != null ? (r.OptAPR * 100).toFixed(2) + "%" : "-"],
        ["差", r.Diff != null ? (r.Diff * 100).toFixed(2) + "pp" : "-"],
        ["建议金额", r.SuggestedAmt != null ? (r.SuggestedAmt | 0) + " " + (r.InvestCcy || "") : "-"],
      ];
      for (const t of tds) {
        const td = document.createElement("td");
        td.textContent = t[1];
        if (t[2]) td.className = t[2];
        if (t[0] === "建议") td.textContent = t[1];
        tr.appendChild(td);
      }
      tb.appendChild(tr);
    }
    if (rows.length === 0) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 7;
      td.textContent = "暂无符合条件的行（DCY 可能不可用或无决策）";
      tr.appendChild(td);
      tb.appendChild(tr);
    }
  } catch (e) {
    meta.textContent = "加载失败: " + e;
  }
}
function setUly(u) {
  const btns = document.querySelectorAll(".cur");
  for (const b of btns) { b.classList.toggle("on", b.dataset.uly === u); }
  return load(u, curExp);
}
document.getElementById("expApply").addEventListener("click", function () { curExp = document.getElementById("exp").value; load(curUly, curExp); });
document.getElementById("exp").addEventListener("change", function () { curExp = document.getElementById("exp").value; load(curUly, curExp); });
document.querySelectorAll(".cur").forEach(function (b) { b.addEventListener("click", function () { curUly = b.dataset.uly; load(curUly, curExp); }); });
setInterval(function () { load(curUly, curExp); }, 20000);
load("BTC-USDT", "");
</script>
</body>
</html>`

// New 组装 HTTP server：addr 监听；fetcher 负责拉取决策快照。
func New(fetcher *market.Fetcher, uly string, addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(indexHTML))
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
			if t, err := strategy.ParseExpiry(expStr); err == nil {
				day = t
			}
		}
		snap, err := fetcher.FetchSnapshot(snapUly)
		if err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		snap = market.FilterByExpiryDay(snap, day)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(snap); err != nil {
			http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		}
	})
	return &http.Server{Addr: addr, Handler: mux}
}
