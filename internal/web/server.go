// Package web 提供 Web 展示服务：GET / 为决策看板页，GET /api/decisions 返回 JSON。
package web

import (
	"encoding/json"
	"net/http"

	"github.com/tp/coinoption/internal/market"
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
  main { padding:16px 20px; }
  table { border-collapse: collapse; width:100%; }
  th, td { border:1px solid var(--line); padding:6px 10px; text-align:right; white-space:nowrap; }
  th { background:#0d1117; color:var(--mut); font-weight:600; position:sticky; top:0; }
  td:first-child, th:first-child, td:nth-child(2), th:nth-child(2) { text-align:left; }
  tbody tr:hover { background:#161b26; }
  .buy { color:var(--ok); } .dual { color:var(--accent); } .none { color:var(--mut); } .unaligned { color:var(--warn); }
  #meta { margin-bottom:12px; color:var(--mut); font-size:12px; }
</style>
</head>
<body>
<header>
  <h1>◈ coinoption · OKX 双币赢 vs 期权 APR 对比</h1>
  <div class="sub" id="meta">加载中…</div>
</header>
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
async function load() {
  const meta = document.getElementById("meta");
  try {
    const r = await fetch("/api/decisions", { cache: "no-store" });
    const j = await r.json();
    const decisions = j.Decisions || [];
    meta.textContent = "标的 " + (j.Uly || "") + " · 期权底层 " + (j.OptionUly || "") + " · 现货 " + (j.SpotPrice ?? "-") +
      " · 期权合约 " + (j.OptionCount ?? "-") + " · DCY 产品 " + (j.DcdCount ?? "-") + " · 更新 " + (j.At ? new Date(j.At).toLocaleTimeString("zh-CN", { hour12: false }) : "-") +
      (j.Warn ? " · " + j.Warn : "");
    const tb = document.querySelector("#rows");
    tb.innerHTML = "";
    for (const d of decisions) {
      const r = d.Row || {};
      const tr = document.createElement("tr");
      const tds = [
        ["建议", actTxt(d.Action), cls(d.Action)],
        ["产品/合约", r.ProductID || "-", ""],
        ["到期", expStr(r.ExpTime) || "-", ""],
        ["DCY APR", r.DcyAPR != null ? (r.DcyAPR * 100).toFixed(2) + "%" : "-", ""],
        ["期权 APR", r.OptAPR != null ? (r.OptAPR * 100).toFixed(2) + "%" : "-", ""],
        ["差", r.Diff != null ? (r.Diff * 100).toFixed(2) + "pp" : "-", ""],
        ["建议金额", r.SuggestedAmt != null ? (r.SuggestedAmt | 0) + " " + (r.InvestCcy || "") : "-", ""],
      ];
      for (const [k, v, c] of tds) {
        const td = document.createElement("td");
        td.textContent = v;
        if (c) td.className = c;
        if (k === "建议") td.textContent = v;
        tr.appendChild(td);
      }
      tb.appendChild(tr);
    }
  } catch (e) {
    meta.textContent = "加载失败: " + e;
  }
}
setInterval(load, 20000);
load();
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
		snap, err := fetcher.FetchSnapshot(uly)
		if err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(snap); err != nil {
			http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		}
	})
	return &http.Server{Addr: addr, Handler: mux}
}
