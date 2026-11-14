"use strict";

const $ = (s, el = document) => el.querySelector(s);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const QUALITY = ["Poor", "Common", "Uncommon", "Rare", "Epic", "Legendary"];

const store = {
  get(k, d) { try { const v = localStorage.getItem(k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* private mode */ } },
};

const state = {
  houses: [],
  house: store.get("house", ""),
  q: "", cls: "", quality: new Set(store.get("quality", [])),
  reqMin: "", reqMax: "", ilMin: "", ilMax: "", minSold: "",
  sort: "value", dir: "desc", offset: 0, rows: [], total: 0,
  range: store.get("range", "7d"),
};

async function api(path) {
  const r = await fetch(path);
  const j = await r.json().catch(() => ({ error: `HTTP ${r.status}` }));
  if (!r.ok || j.error) throw new Error(j.error || `HTTP ${r.status}`);
  return j;
}

/* formatting */
function money(c, { zero = "—" } = {}) {
  if (!c) return `<span class="none">${zero}</span>`;
  const neg = c < 0; c = Math.abs(Math.round(c));
  const g = Math.floor(c / 10000), s = Math.floor((c % 10000) / 100), cp = c % 100;
  const parts = [];
  if (g) parts.push(`<span class="g">${g.toLocaleString("en-GB")}<i></i></span>`);
  if (s || (g && cp)) parts.push(`<span class="s">${s}<i></i></span>`);
  if (!g && cp) parts.push(`<span class="c">${cp}<i></i></span>`);
  if (g >= 100) parts.splice(1); // big prices: gold only, like the in-game tooltip
  return `<span class="money">${neg ? "−" : ""}${parts.join("")}</span>`;
}
const num = (v) => v == null ? "—" : v >= 100 ? Math.round(v).toLocaleString("en-GB") : v >= 10 ? v.toFixed(0) : v === 0 ? "0" : v < 0.1 ? "<0.1" : v.toFixed(1);
const count = (v) => v == null ? "—" : Math.round(v).toLocaleString("en-GB");
const pct = (v) => v == null || v < 0 ? "—" : `${Math.round(v * 100)}%`;
const range = (est, lo, hi) => `${num(est)} <span class="sub-num">${num(lo)}–${num(hi)}</span>`;
function ago(unix) {
  if (!unix) return "never";
  const m = Math.round((Date.now() / 1000 - unix) / 60);
  if (m < 1) return "just now";
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  return h < 48 ? `${h} h ago` : `${Math.round(h / 24)} days ago`;
}
const berlin = new Intl.DateTimeFormat("en-GB", { timeZone: "Europe/Berlin", weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
function change(v) {
  if (v == null) return `<span class="badge flat">—</span>`;
  if (Math.abs(v) < 0.005) return `<span class="badge flat">0.0%</span>`;
  return `<span class="badge ${v > 0 ? "up" : "down"}">${v > 0 ? "▲" : "▼"} ${Math.abs(v * 100).toFixed(1)}%</span>`;
}
function icon(row, big) {
  const style = row.icon ? ` style="background-image:url('${esc(row.icon)}')"` : "";
  return `<span class="icon ${big ? "big" : ""} q${row.quality ?? 1}"${style} aria-hidden="true"></span>`;
}
function crest(faction) {
  if (faction === "horde") {
    return `<svg class="crest" viewBox="0 0 32 32" aria-hidden="true"><circle cx="16" cy="16" r="15" fill="#2a120f" stroke="#7a2a20"/><path d="M9 7h14v14l-7 5-7-5z" fill="#b8352b"/><path d="M16 10v11M12 13l4 3 4-3" stroke="#f3c2a4" stroke-width="1.6" fill="none"/></svg>`;
  }
  if (faction === "alliance") {
    return `<svg class="crest" viewBox="0 0 32 32" aria-hidden="true"><circle cx="16" cy="16" r="15" fill="#101c33" stroke="#2d4c86"/><path d="M9 7h14v14l-7 5-7-5z" fill="#2b56a8"/><path d="M16 10l3 5h-6z" fill="#e8d28a"/></svg>`;
  }
  return `<svg class="crest" viewBox="0 0 32 32" aria-hidden="true"><circle cx="16" cy="16" r="15" fill="#2a2110" stroke="#8a6a2a"/><circle cx="16" cy="16" r="9.5" fill="#d8b25c"/><circle cx="16" cy="16" r="6" fill="none" stroke="#7a5a1c" stroke-width="1.6"/></svg>`;
}
const factionName = (f) => f ? f[0].toUpperCase() + f.slice(1) : "";

/* tooltip */
const tip = $("#tooltip");
function showTip(html, ev) {
  tip.innerHTML = html; tip.hidden = false;
  const r = tip.getBoundingClientRect();
  let x = ev.clientX + 14, y = ev.clientY + 14;
  if (x + r.width > innerWidth - 8) x = ev.clientX - r.width - 14;
  if (y + r.height > innerHeight - 8) y = ev.clientY - r.height - 14;
  tip.style.left = `${x}px`; tip.style.top = `${y}px`;
}
const hideTip = () => { tip.hidden = true; };

/* houses + hero */
async function loadHouses() {
  state.houses = await api("/api/houses");
  const tradeable = state.houses.filter((h) => !h.view_only);
  if (!tradeable.some((h) => h.faction === state.house)) state.house = tradeable[0]?.faction || "";
  renderHouses();
}
function currentHouse() { return state.houses.find((h) => h.faction === state.house) || state.houses.find((h) => !h.view_only); }
function renderHouses() {
  $("#houses").innerHTML = state.houses.filter((h) => !h.view_only).map((h) => `
    <button class="house" role="tab" aria-selected="${h.faction === state.house}" data-house="${h.faction}">
      ${crest(h.faction)}<span><strong>${factionName(h.faction)}</strong><small>${count(h.listings)} listings</small></span>
    </button>`).join("");
  const h = currentHouse();
  const latest = Math.max(0, ...state.houses.map((x) => x.last_scan));
  const fresh = latest && Date.now() / 1000 - latest < 75 * 60;
  $("#scan").innerHTML = `<span class="dot ${latest ? (fresh ? "" : "warn") : "bad"}"></span>Last scan ${ago(latest)}`;
  $("#hero-kicker").textContent = `WoW Forever · EU · PvP${h ? ` · ${factionName(h.faction)}` : ""}`;
  $("#hero-stats").innerHTML = h ? `
    <div><dt>Listings</dt><dd>${count(h.listings)}</dd></div>
    <div><dt>Items</dt><dd>${count(h.items)}</dd></div>
    <div><dt>Last scan</dt><dd><span class="dot ${fresh ? "" : "warn"}"></span>${ago(h.last_scan)}</dd></div>` : "";
}
$("#houses").addEventListener("click", (e) => {
  const b = e.target.closest("[data-house]");
  if (!b) return;
  state.house = b.dataset.house; store.set("house", state.house);
  renderHouses(); route();
});

/* refine controls */
function renderQuality() {
  $("#quality").innerHTML = QUALITY.map((q, i) =>
    `<button class="chip q${i}" aria-pressed="${state.quality.has(i)}" data-q="${i}">${q}</button>`).join("");
}
$("#quality").addEventListener("click", (e) => {
  const b = e.target.closest("[data-q]");
  if (!b) return;
  const q = +b.dataset.q;
  state.quality.has(q) ? state.quality.delete(q) : state.quality.add(q);
  store.set("quality", [...state.quality]);
  renderQuality(); refreshList();
});
let debounce;
const onFilter = () => { clearTimeout(debounce); debounce = setTimeout(refreshList, 220); };
for (const [id, key] of [["req-min", "reqMin"], ["req-max", "reqMax"], ["il-min", "ilMin"], ["il-max", "ilMax"], ["min-sold", "minSold"]]) {
  $(`#${id}`).addEventListener("input", (e) => { state[key] = e.target.value.trim(); onFilter(); });
}
$("#search").addEventListener("input", (e) => {
  state.q = e.target.value;
  if (!location.hash.startsWith("#/browse")) location.hash = "#/browse";
  onFilter();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "/" && document.activeElement.tagName !== "INPUT") { e.preventDefault(); $("#search").focus(); }
});
$("#browse").addEventListener("click", (e) => {
  const b = e.target.closest("[data-class]");
  if (!b) return;
  state.cls = b.dataset.class;
  if (location.hash !== "#/browse") location.hash = "#/browse";
  refreshList();
});

function listQuery() {
  const p = new URLSearchParams({ house: state.house, sort: state.sort, dir: state.dir, offset: state.offset, limit: 100 });
  if (state.q) p.set("q", state.q);
  if (state.cls) p.set("class", state.cls);
  if (state.quality.size) p.set("quality", [...state.quality].join(","));
  for (const [k, v] of [["req_min", state.reqMin], ["req_max", state.reqMax], ["ilvl_min", state.ilMin], ["ilvl_max", state.ilMax], ["min_sold", state.minSold]]) if (v) p.set(k, v);
  return p;
}

/* list view */
const COLS = [
  { key: "name", label: "Item" },
  { key: "min", label: "Buyout" },
  { key: "fair", label: "Market value", opt: true },
  { key: "available", label: "Available" },
  { key: "sold", label: "Sold / day" },
  { key: "change", label: "24h", opt: true },
];
async function refreshList(append) {
  if (!append) state.offset = 0;
  const view = $("#view");
  if (!view.querySelector(".items")) view.innerHTML = `<div class="panel table-panel"><div class="empty-state">Loading the auction house…</div></div>`;
  let data;
  try { data = await api(`/api/items?${listQuery()}`); }
  catch (err) { view.innerHTML = emptyState("No auction data yet", `${esc(err.message)}. Start the collector and give it one cycle (about a minute).`); return; }
  state.rows = append ? state.rows.concat(data.rows) : data.rows;
  state.total = data.total;
  renderBrowse(data.classes, data.all_count);
  const sorted = { value: "gold on the market", sold: "units sold per day", turnover: "gold traded per day", min: "buyout", fair: "market value", available: "quantity available", change: "24h change", name: "name" }[state.sort];
  view.innerHTML = `
    <div class="panel table-panel">
      <div class="list-head">
        <h2>${esc(state.cls || "All items")}</h2>
        <p>${count(state.total)} items · sorted by ${sorted}</p>
        <span class="pill">${crest(state.house).replace('class="crest"', 'class="crest" style="width:18px;height:18px"')}${factionName(state.house)} · PvP · EU</span>
      </div>
      ${data.unnamed > 20 ? `<p class="fine names-note">Still looking up item names and icons: ${count(data.unnamed)} of ${count(data.all_count)} to go. They fill in with each scan (every 30 minutes); until then items show as "Item #id".</p>` : ""}
      ${state.rows.length ? `<div class="scroll-x"><table class="items">
        <thead><tr>${COLS.map((c) => `<th class="${c.opt ? "opt" : ""}" ${state.sort === c.key ? `aria-sort="${state.dir === "asc" ? "ascending" : "descending"}"` : ""}><button data-sort="${c.key}">${c.label}</button></th>`).join("")}</tr></thead>
        <tbody>${state.rows.map(rowHTML).join("")}</tbody>
      </table></div>
      ${state.rows.length < state.total ? `<div class="more"><button class="btn" id="more">Show 100 more</button></div>` : ""}`
      : emptyState("Nothing matches", "Loosen the filters or clear the search.")}
    </div>`;
  $("#more")?.addEventListener("click", () => { state.offset += 100; refreshList(true); });
}
function rowHTML(r) {
  const href = `#/item/${r.id}${r.variant ? `?variant=${encodeURIComponent(r.variant)}` : ""}`;
  const sub = [r.ilvl ? `Item level <b>${r.ilvl}</b>` : "", r.req_level > 1 ? `Requires level <b>${r.req_level}</b>` : "", r.variant ? esc(r.variant) : ""].filter(Boolean).join(" · ");
  return `<tr tabindex="0" data-href="${href}">
    <td><div class="item-cell">${icon(r)}<div><div class="item-name q${r.quality}">${esc(r.name)}</div><div class="item-sub">${sub || esc(r.class)}</div></div></div></td>
    <td>${money(r.min)}</td>
    <td class="opt">${money(r.fair)}</td>
    <td>${count(r.available)}${r.auctions > 1 ? `<span class="sub-num">${count(r.auctions)} auctions</span>` : ""}</td>
    <td>${r.sold_hi ? range(r.sold_est, r.sold_lo, r.sold_hi) : `<span class="none">—</span>`}${r.volatile ? `<span class="vol" title="Price swings a lot (coefficient of variation above 0.5)">volatile</span>` : ""}</td>
    <td class="opt">${change(r.change_24h)}</td>
  </tr>`;
}
$("#view").addEventListener("click", (e) => {
  const s = e.target.closest("[data-sort]");
  if (s) {
    const k = s.dataset.sort;
    state.dir = state.sort === k && state.dir === "desc" ? "asc" : "desc";
    if (k === "name" && state.sort !== k) state.dir = "asc";
    state.sort = k; refreshList(); return;
  }
  const tr = e.target.closest("tr[data-href]");
  if (tr) location.hash = tr.dataset.href;
});
$("#view").addEventListener("keydown", (e) => {
  const tr = e.target.closest("tr[data-href]");
  if (tr && e.key === "Enter") location.hash = tr.dataset.href;
});
function renderBrowse(classes, all) {
  if (classes) { state.classes = classes; state.allCount = all; } else { classes = state.classes || []; all = state.allCount || 0; }
  const items = [{ name: "", label: "All items", count: all }, ...classes.map((c) => ({ name: c.name, label: c.name, count: c.count }))];
  $("#browse").innerHTML = items.map((c) =>
    `<li><button data-class="${esc(c.name)}" aria-pressed="${state.cls === c.name}"><span>${esc(c.label)}</span><span class="count">${count(c.count)}</span></button></li>`).join("");
}
const emptyState = (h, p) => `<div class="empty-state"><h3>${h}</h3><p>${p}</p></div>`;

/* item view */
async function renderItem(id, variant) {
  const view = $("#view");
  view.innerHTML = `<div class="panel"><div class="empty-state">Loading item…</div></div>`;
  const vq = variant ? `&variant=${encodeURIComponent(variant)}` : "";
  let item, hist, sell;
  try {
    [item, hist, sell] = await Promise.all([
      api(`/api/item/${id}?house=${state.house}${vq}`),
      api(`/api/item/${id}/history?house=${state.house}&range=${state.range}${vq}`),
      api(`/api/item/${id}/sell?house=${state.house}&days=14${vq}`).catch((e) => ({ error: e.message })),
    ]);
  } catch (err) {
    view.innerHTML = `<a class="back" href="#/browse">← Back to the auction house</a>` + `<div class="panel">${emptyState("Item not found", esc(err.message))}</div>`;
    return;
  }
  const it = item.item, h = item.house;
  if (state.classes) renderBrowse();
  else api(`/api/items?house=${state.house}&limit=1`).then((d) => renderBrowse(d.classes, d.all_count)).catch(() => {});
  const sub = [`<span class="q${it.quality}">${QUALITY[it.quality] || ""}</span>`, it.ilvl ? `Item level <b>${it.ilvl}</b>` : "",
    it.req_level > 1 ? `Requires level <b>${it.req_level}</b>` : "", it.subclass ? esc(it.subclass) : "", `Item ID <b>${it.id}</b>`].filter(Boolean);
  view.innerHTML = `
    <a class="back" href="#/browse">← Back to the auction house</a>
    <section class="item-head qh${it.quality}">
      ${icon(it, true)}
      <div><h2 class="q${it.quality}">${esc(it.name)}${it.variant ? ` <small class="item-sub">${esc(it.variant)}</small>` : ""}</h2><div class="meta">${sub.join("<span aria-hidden='true'>◆</span>")}</div></div>
      <div class="best"><small>Lowest buyout · ${factionName(h.faction)}</small>${money(it.min, { zero: "Not listed" })}</div>
    </section>
    <dl class="stats">
      <div class="stat"><dt>Lowest buyout</dt><dd>${money(it.min, { zero: "Not listed" })}<span class="note">${factionName(h.faction)} auction house</span></dd></div>
      <div class="stat"><dt>Market value</dt><dd>${money(it.fair)}<span class="note">cheapest 20% of units, 7-day median</span></dd></div>
      <div class="stat"><dt>Median</dt><dd>${money(it.median)}<span class="note">of all listed units</span></dd></div>
      <div class="stat"><dt>Available</dt><dd>${count(it.available)}<span class="note">${count(it.auctions)} auctions · scanned ${ago(h.last_scan)}</span></dd></div>
      <div class="stat"><dt>24h change</dt><dd>${change(it.change_24h)}<span class="note">market value</span></dd></div>
      <div class="stat"><dt>Sold per day</dt><dd>${it.sold_hi ? num(it.sold_est) : "—"}<span class="note">${it.sold_hi ? `range ${num(it.sold_lo)}–${num(it.sold_hi)}, this house` : "no sales seen yet"}</span></dd></div>
    </dl>
    <div class="item-grid">
      <div class="stack">
        <section class="panel">
          <h3 class="panel-title">Price history
            <span class="seg" role="group" aria-label="Range">${["24h", "7d", "30d", "all"].map((r) => `<button data-range="${r}" aria-pressed="${state.range === r}">${r === "all" ? "All" : r}</button>`).join("")}</span>
          </h3>
          <div class="chart" id="chart"></div>
          <div class="legend"><span>Market value (p20)</span><span class="l-min">Lowest buyout</span><span class="l-bar">Units listed</span></div>
        </section>
        <section class="panel when" id="when"></section>
      </div>
      <div class="stack">
        <section class="panel">
          <h3 class="panel-title">Auction houses <span class="aside">PvP · EU</span></h3>
          ${item.houses.map((x) => `
            <a class="house-card ${x.house.id === h.id ? "here" : ""}" href="#/item/${it.id}${variant ? `?variant=${encodeURIComponent(variant)}` : ""}" data-switch="${x.house.faction}">
              ${crest(x.house.faction)}
              <div><strong>${factionName(x.house.faction)}</strong><small>${x.row ? `${count(x.row.available)} available · market ${plain(x.row.fair)}` : "Not listed"}</small></div>
              <div class="right">${x.row ? money(x.row.min) : `<span class="none">—</span>`}<small style="display:block">${x.house.id === h.id ? "you are here" : x.house.view_only ? "view only" : `${Math.round(x.house.cut * 100)}% cut`}</small></div>
            </a>`).join("")}
        </section>
        <section class="panel">
          <h3 class="panel-title">How it sells <span class="aside">this house, 7 days</span></h3>
          <div class="kv"><div><b>Sold per day</b><small>best estimate, with the floor and ceiling</small></div><div class="val">${it.sold_hi ? range(it.sold_est, it.sold_lo, it.sold_hi) : "—"}</div></div>
          <div class="kv"><div><b>Sale rate</b><small>share of ended listings that sold</small></div><div class="val">${it.sell_through >= 0 ? `<span class="meter"><i style="width:${Math.min(100, it.sell_through * 100)}%"></i></span>${pct(it.sell_through)}` : "—"}</div></div>
          <div class="kv"><div><b>Typical sale price</b><small>median of auctions that sold, 14 days</small></div><div class="val">${money(medianSold(sell))}</div></div>
          <p class="fine">Sales are inferred from listings that vanish between scans, so every figure is a range. Items posted and bought between two scans are never seen.</p>
        </section>
      </div>
    </div>`;
  drawChart(hist.points);
  renderWhen(sell);
  view.querySelectorAll("[data-range]").forEach((b) => b.addEventListener("click", async () => {
    state.range = b.dataset.range; store.set("range", state.range);
    view.querySelectorAll("[data-range]").forEach((x) => x.setAttribute("aria-pressed", x === b));
    const h2 = await api(`/api/item/${id}/history?house=${state.house}&range=${state.range}${vq}`);
    drawChart(h2.points);
  }));
  view.querySelectorAll("[data-switch]").forEach((a) => a.addEventListener("click", () => {
    state.house = a.dataset.switch; store.set("house", state.house); renderHouses();
    setTimeout(route, 0);
  }));
}
function plain(c) {
  if (!c) return "—";
  const g = Math.floor(c / 10000), sv = Math.floor((c % 10000) / 100), cp = c % 100;
  return g ? `${g.toLocaleString("en-GB")}g${g < 100 && sv ? ` ${sv}s` : ""}` : sv ? `${sv}s${cp ? ` ${cp}c` : ""}` : `${cp}c`;
}
function medianSold(sell) {
  if (!sell || sell.error) return 0;
  const prices = sell.blocks.filter((b) => b.median_sold_price).map((b) => b.median_sold_price).sort((a, b) => a - b);
  return prices.length ? prices[Math.floor(prices.length / 2)] : 0;
}

/* chart */
function drawChart(points) {
  const el = $("#chart");
  if (!el) return;
  const pts = points.filter((p) => p.p20 || p.min || p.listed);
  if (pts.length < 2) { el.innerHTML = emptyState("Not enough history yet", "The chart fills in as the collector takes snapshots every 30 minutes."); return; }
  const W = el.clientWidth, H = el.clientHeight, padL = 54, padR = 12, padT = 10, padB = 26, barH = 56;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const prices = pts.flatMap((p) => [p.p20, p.min]).filter(Boolean);
  const lo = Math.min(...prices) * 0.92, hi = Math.max(...prices) * 1.05;
  const maxListed = Math.max(1, ...pts.map((p) => p.listed));
  const x = (t) => padL + ((t - t0) / Math.max(1, t1 - t0)) * (W - padL - padR);
  const y = (v) => padT + (1 - (v - lo) / Math.max(1, hi - lo)) * (H - padT - padB - barH - 8);
  const step = (key) => {
    let d = "", pen = false;
    pts.forEach((p, i) => {
      const v = p[key];
      if (!v) { pen = false; return; }
      d += pen ? ` H${x(p.t).toFixed(1)} V${y(v).toFixed(1)}` : ` M${x(p.t).toFixed(1)} ${y(v).toFixed(1)}`;
      pen = true;
      if (i === pts.length - 1) d += ` H${x(p.t).toFixed(1)}`;
    });
    return d;
  };
  const ticks = 4, grid = [], labels = [];
  for (let i = 0; i <= ticks; i++) {
    const v = lo + ((hi - lo) * i) / ticks;
    grid.push(`<line x1="${padL}" x2="${W - padR}" y1="${y(v)}" y2="${y(v)}"/>`);
    labels.push(`<text x="${padL - 8}" y="${y(v) + 4}" text-anchor="end">${shortGold(v)}</text>`);
  }
  const tlabels = [];
  const nT = Math.min(6, pts.length);
  for (let i = 0; i < nT; i++) {
    const t = t0 + ((t1 - t0) * i) / (nT - 1);
    const d = new Date(t * 1000);
    const lbl = t1 - t0 > 2 * 86400 ? d.toLocaleDateString("en-GB", { day: "numeric", month: "short", timeZone: "Europe/Berlin" }) : d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", timeZone: "Europe/Berlin" });
    tlabels.push(`<text x="${x(t)}" y="${H - 6}" text-anchor="${i === 0 ? "start" : i === nT - 1 ? "end" : "middle"}">${lbl}</text>`);
  }
  // Bar width follows the spacing between snapshots (hourly), capped so a sparse series
  // doesn't turn into slabs; bars are clipped to the plot area.
  let gap = Infinity;
  for (let i = 1; i < pts.length; i++) gap = Math.min(gap, x(pts[i].t) - x(pts[i - 1].t));
  const bw = Math.max(1.5, Math.min(10, (isFinite(gap) ? gap : 10) * 0.7));
  const bars = pts.map((p) => {
    const h = (p.listed / maxListed) * barH;
    return `<rect class="bar" x="${(x(p.t) - bw / 2).toFixed(1)}" y="${(H - padB - h).toFixed(1)}" width="${bw.toFixed(1)}" height="${h.toFixed(1)}" rx="1"/>`;
  }).join("");
  const area = step("p20");
  const fillPath = area ? `${area} V${H - padB - barH - 8} H${x(pts.find((p) => p.p20)?.t ?? t0)} Z` : "";
  el.innerHTML = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Price history">
    <defs><linearGradient id="fill" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="#d8b25c" stop-opacity=".28"/><stop offset="1" stop-color="#d8b25c" stop-opacity="0"/></linearGradient>
      <clipPath id="plot"><rect x="${padL}" y="0" width="${W - padL - padR}" height="${H}"/></clipPath></defs>
    <g class="grid">${grid.join("")}</g><g class="axis">${labels.join("")}${tlabels.join("")}</g>
    <g clip-path="url(#plot)">${bars}</g>
    <path d="${fillPath}" fill="url(#fill)"/>
    <path class="minline" d="${step("min")}"/><path class="p20" d="${area}"/>
    <line class="cross" id="cross" y1="${padT}" y2="${H - padB}" visibility="hidden"/>
  </svg>`;
  const svg = el.querySelector("svg"), cross = $("#cross");
  svg.addEventListener("mousemove", (e) => {
    const r = svg.getBoundingClientRect();
    const mx = ((e.clientX - r.left) / r.width) * W;
    let best = pts[0];
    for (const p of pts) if (Math.abs(x(p.t) - mx) < Math.abs(x(best.t) - mx)) best = p;
    cross.setAttribute("x1", x(best.t)); cross.setAttribute("x2", x(best.t)); cross.setAttribute("visibility", "visible");
    showTip(`<b>${berlin.format(new Date(best.t * 1000))}</b>
      <div class="row"><span>Market value</span>${money(best.p20)}</div>
      <div class="row"><span>Lowest buyout</span>${money(best.min)}</div>
      <div class="row"><span>Units listed</span>${count(best.listed)}</div>`, e);
  });
  svg.addEventListener("mouseleave", () => { cross.setAttribute("visibility", "hidden"); hideTip(); });
}
function shortGold(c) {
  const g = c / 10000;
  if (g >= 1000) return `${(g / 1000).toFixed(1)}k g`;
  if (g >= 1) return `${g >= 100 ? g.toFixed(0) : g.toFixed(1)}g`;
  return c >= 100 ? `${(c / 100).toFixed(0)}s` : `${Math.round(c)}c`;
}

/* when to post */
function renderWhen(a) {
  const el = $("#when");
  if (!a || a.error) {
    el.innerHTML = `<h3 class="panel-title">When to post</h3>${emptyState("No selling history yet", esc(a?.error || "Come back after the collector has run for a few days."))}`;
    return;
  }
  const cols = a.blocks.length;
  const cells = a.heat.flat();
  const metric = (c) => c.ranked ? c.expected_net : null;
  const vals = cells.map(metric).filter((v) => v != null);
  const useRate = vals.length < 3;
  const val = (c) => useRate ? (c.sell_rate >= 0 ? c.sell_rate : null) : metric(c);
  const all = cells.map(val).filter((v) => v != null);
  const vmin = Math.min(...all), vmax = Math.max(...all);
  const nowD = (new Date(new Date().toLocaleString("en-US", { timeZone: "Europe/Berlin" })).getDay() + 6) % 7;
  const nowB = Math.floor(new Date(new Date().toLocaleString("en-US", { timeZone: "Europe/Berlin" })).getHours() / a.block_hours);
  let bestD = -1, bestB = -1, bestV = -Infinity;
  a.heat.forEach((row, d) => row.forEach((c, b) => { const v = val(c); if (v != null && v > bestV) { bestV = v; bestD = d; bestB = b; } }));
  const color = (v) => {
    if (v == null) return "";
    const t = vmax > vmin ? (v - vmin) / (vmax - vmin) : 0.6;
    const stops = [[42, 33, 23], [107, 81, 36], [216, 178, 92], [255, 241, 196]];
    const s = t * (stops.length - 1), i = Math.min(stops.length - 2, Math.floor(s)), f = s - i;
    const c = stops[i].map((x, k) => Math.round(x + (stops[i + 1][k] - x) * f));
    return `background:rgb(${c.join(",")})`;
  };
  const days = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
  const grid = [`<div></div>`, ...a.blocks.map((b) => `<div class="hh">${b.label.slice(0, 2)}h</div>`)];
  a.heat.forEach((row, d) => {
    grid.push(`<div class="hl">${days[d]}</div>`);
    row.forEach((c, b) => {
      const v = val(c);
      const cls = ["cell", v == null ? "empty" : "", d === nowD && b === nowB ? "now" : "", d === bestD && b === bestB ? "best" : ""].join(" ");
      grid.push(`<div class="${cls}" style="${color(v)}" data-d="${d}" data-b="${b}" tabindex="${v == null ? -1 : 0}" aria-label="${days[d]} ${c.label}"></div>`);
    });
  });
  const bestBlock = a.blocks.find((b) => b.label === a.best);
  el.innerHTML = `
    <h3 class="panel-title">When to post <span class="aside">last ${a.days} days · Berlin time · ${count(a.ended_units)} units ended</span></h3>
    <div class="verdict">${a.verdict.map((l) => `<p>${esc(l)}</p>`).join("")}</div>
    <div class="heat" style="--cols:${cols}">${grid.join("")}</div>
    <div class="heat-legend"><span>${useRate ? "Sell rate" : "Expected net per listing"}</span><span class="ramp"></span><span>low → high</span>
      <span>· outlined white: now · glowing: best so far · hatched: no data yet</span></div>
    <h3 class="panel-title" style="margin-top:22px">By time of day</h3>
    <div class="scroll-x"><table class="mini">
      <thead><tr><th>Posted</th><th>Typical lowest</th><th>Listed</th><th>Sell rate</th><th>Sold price</th><th>Time to sell</th><th>Undercut ≤2h</th><th>Expected net</th></tr></thead>
      <tbody>${a.blocks.map((b) => `<tr class="${b.label === a.best ? "best-row" : ""}"><td>${b.label}</td><td>${money(b.median_min)}</td><td>${b.median_min || b.median_listed ? count(b.median_listed) : "—"}</td>
        <td>${b.sell_rate >= 0 ? `${pct(b.sell_rate)} <span class="sub-num">${pct(b.sell_lo)}–${pct(b.sell_hi)}</span>` : "—"}</td>
        <td>${money(b.median_sold_price)}</td><td>${dur(b.median_sell_secs)}</td><td>${pct(b.undercut_2h)}</td><td>${b.ranked ? money(b.expected_net) : "—"}</td></tr>`).join("")}</tbody>
    </table></div>
    ${a.positions?.length ? `<h3 class="panel-title" style="margin-top:22px">How hard to undercut</h3>
    <table class="mini"><thead><tr><th>Price when posted</th><th>Posted</th><th>Sell rate</th></tr></thead>
      <tbody>${a.positions.map((p) => `<tr><td>${esc(p.label)}</td><td>${count(p.posted)}</td><td>${p.rate >= 0 ? `${pct(p.rate)} <span class="sub-num">${pct(p.lo)}–${pct(p.hi)}</span>` : "—"}</td></tr>`).join("")}</tbody></table>` : ""}
    ${bestBlock ? "" : `<p class="fine">Rankings appear once each time block has a few finished auctions; expect that after one to two weeks of collection.</p>`}`;
  const heat = el.querySelector(".heat");
  const cellTip = (e) => {
    const t = e.target.closest(".cell");
    if (!t) return hideTip();
    const c = a.heat[t.dataset.d][t.dataset.b];
    showTip(`<b>${days[t.dataset.d]} ${c.label.split(" ").pop()}</b>
      <div class="row"><span>Posted here</span>${count(c.posted)}</div>
      <div class="row"><span>Sell rate</span>${c.sell_rate >= 0 ? `${pct(c.sell_rate)} (${pct(c.sell_lo)}–${pct(c.sell_hi)})` : "—"}</div>
      <div class="row"><span>Typical lowest</span>${money(c.median_min)}</div>
      <div class="row"><span>Sold price</span>${money(c.median_sold_price)}</div>
      <div class="row"><span>Expected net</span>${c.ranked ? money(c.expected_net) : "—"}</div>`, e);
  };
  heat.addEventListener("mousemove", cellTip);
  heat.addEventListener("mouseleave", hideTip);
}
const dur = (s) => !s ? "—" : s >= 3600 ? `${(s / 3600).toFixed(1)} h` : `${Math.round(s / 60)} min`;

/* status */
async function renderStatus() {
  const view = $("#view");
  const s = await api("/api/status").catch((e) => ({ error: e.message }));
  if (s.error) { view.innerHTML = `<div class="panel">${emptyState("No collector status yet", esc(s.error))}</div>`; return; }
  // Feed keys are "faction/houseId/kind"; show them as "Horde · Commodities".
  const feedName = (k) => {
    const [faction, , kind] = k.split("/");
    return `${factionName(faction)} · ${kind === "commodity" ? "Commodities" : "Gear"}`;
  };
  const resultText = { applied: "New data", identical: "Republished, no changes", not_modified: "No change since last scan", error: "Failed" };
  const feeds = Object.entries(s.feeds || {}).sort(([a], [b]) => feedName(a).localeCompare(feedName(b)));
  view.innerHTML = `
    <section class="panel">
      <h2 class="panel-title">Collector <span class="aside">updated ${ago(Date.parse(s.updated_at) / 1000)}</span></h2>
      <div class="kv"><div><b>Cycles</b><small>successful / total</small></div><div class="val">${count(s.cycles - s.failed_cycles)} / ${count(s.cycles)}</div></div>
      <div class="kv"><div><b>Failed in a row</b><small>alerts after 3</small></div><div class="val ${s.consecutive_failed_cycles ? "error" : ""}">${s.consecutive_failed_cycles}</div></div>
      <div class="kv"><div><b>Nightly rollup and backup</b></div><div class="val">${s.last_maintenance ? ago(Date.parse(s.last_maintenance) / 1000) : "not yet"}</div></div>
      ${s.last_error ? `<p class="fine error">Last error: ${esc(s.last_error)}</p>` : ""}
    </section>
    <div class="status-grid" style="margin-top:16px">${feeds.map(([k, f]) => `
      <section class="panel">
        <h3 class="panel-title">${esc(feedName(k))}</h3>
        <div class="kv"><div><b>Last success</b></div><div class="val">${ago(Date.parse(f.last_success) / 1000)}</div></div>
        <div class="kv"><div><b>Last result</b><small>${count(f.applied_count)} with new data · ${count(f.not_modified_count)} unchanged</small></div><div class="val">${esc(resultText[f.last_result] || f.last_result)}</div></div>
        <div class="kv"><div><b>Auctions</b></div><div class="val">${count(f.n_auctions)}</div></div>
        ${f.publish_lag_seconds ? `<div class="kv"><div><b>Fetched after publish</b><small>how long after Blizzard published the last update</small></div><div class="val">${f.publish_lag_seconds < 120 ? `${f.publish_lag_seconds} s` : `${Math.round(f.publish_lag_seconds / 60)} min`}</div></div>` : ""}
        ${f.last_error ? `<p class="fine error">${esc(f.last_error)}</p>` : ""}
      </section>`).join("")}</div>`;
}

/* advisor: what should I do right now? */
const VERDICT_TONE = {
  "GOOD BUY": "good", "GOOD TO SELL": "good", "SELL NOW": "good", "CANCEL AND RELIST": "good",
  "FAIR": "wait", "SELL IN PARTS": "wait", "HOLD": "wait", "LET IT RIDE": "wait", "ALREADY CHEAPEST": "wait",
  "EXPENSIVE": "bad", "TOO HIGH": "bad",
};
const verdictChip = (v) => `<span class="verdict-chip ${VERDICT_TONE[v] || "neutral"}">${esc(v.toLowerCase())}</span>`;
const confNote = (c) => c === "good" ? "" : `<p class="fine">${c === "early" ? "Early estimate: under 3 days of history." : "Fair estimate: under 10 days of history."}</p>`;
const remember = (k, v) => store.set(`adv:${k}`, v);
const recall = (k, d = "") => store.get(`adv:${k}`, d);

function itemField(id, label) {
  return `<label class="fld grow"><span>${label}</span>
    <span class="ac"><input id="${id}" autocomplete="off" placeholder="Item name" value="${esc(recall(id))}" required>
    <ul class="ac-list" hidden></ul></span></label>`;
}

function renderAdvisor() {
  const h = currentHouse();
  const house = factionName(state.house);
  $("#view").innerHTML = `
    <div class="advisor">
      <section class="panel adv" id="adv-price">
        <h2 class="panel-title">Is this a good price? <span class="aside">${house}</span></h2>
        <form class="adv-form">
          ${itemField("pc-item", "Item")}
          <label class="fld"><span>Price per unit</span><input id="pc-price" placeholder="12g30s" value="${esc(recall("pc-price"))}" required></label>
          <button class="btn primary">Check</button>
        </form>
        <div class="adv-out" aria-live="polite"></div>
      </section>

      <section class="panel adv" id="adv-sell">
        <h2 class="panel-title">Sell or hold? <span class="aside">${house} · ${Math.round((h?.cut ?? 0.05) * 100)}% cut</span></h2>
        <form class="adv-form">
          ${itemField("so-item", "Item you have")}
          <label class="fld small"><span>How many</span><input id="so-qty" inputmode="numeric" value="${esc(recall("so-qty", "1"))}"></label>
          <button class="btn primary">Advise</button>
        </form>
        <div class="adv-out" aria-live="polite"></div>
      </section>

      <section class="panel adv" id="adv-cancel">
        <h2 class="panel-title">Cancel or let it ride? <span class="aside">one of your listings</span></h2>
        <form class="adv-form">
          ${itemField("cr-item", "Item")}
          <label class="fld"><span>Your price per unit</span><input id="cr-price" placeholder="12g30s" value="${esc(recall("cr-price"))}" required></label>
          <label class="fld small"><span>Qty</span><input id="cr-qty" inputmode="numeric" value="${esc(recall("cr-qty", "1"))}"></label>
          <label class="fld"><span>Time left</span><select id="cr-tl">
            ${[["SHORT", "Short (< 30 min)"], ["MEDIUM", "Medium (30 min–2 h)"], ["LONG", "Long (2–12 h)"], ["VERY_LONG", "Very long (12–48 h)"]]
              .map(([v, l]) => `<option value="${v}" ${recall("cr-tl", "LONG") === v ? "selected" : ""}>${l}</option>`).join("")}
          </select></label>
          <button class="btn primary">Decide</button>
        </form>
        <div class="adv-out" aria-live="polite"></div>
      </section>

      <section class="panel adv wide" id="adv-flips">
        <h2 class="panel-title">Worth flipping now <span class="aside">both houses · after cut and deposit</span></h2>
        <div class="adv-out" aria-live="polite"><div class="empty-state">Looking for flips…</div></div>
      </section>
    </div>`;

  bindAutocomplete();
  const q = (o) => new URLSearchParams({ house: state.house, ...o });
  advForm("adv-price", () => ({ item: $("#pc-item").value, price: $("#pc-price").value }), (v) => `/api/advice/price?${q(v)}`, renderPriceCheck);
  advForm("adv-sell", () => ({ item: $("#so-item").value, qty: $("#so-qty").value || "1" }), (v) => `/api/advice/sell?${q(v)}`, renderSellAdvice);
  advForm("adv-cancel", () => ({ item: $("#cr-item").value, price: $("#cr-price").value, qty: $("#cr-qty").value || "1", time_left: $("#cr-tl").value }),
    (v) => `/api/advice/cancel?${q(v)}`, renderCancelAdvice);
  loadFlips();
}

function advForm(id, values, url, render) {
  const sec = $(`#${id}`), out = sec.querySelector(".adv-out");
  sec.querySelector("form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const v = values();
    for (const el of sec.querySelectorAll("input, select")) if (el.id) remember(el.id, el.value);
    sec.querySelectorAll(".ac-list").forEach((l) => { l.hidden = true; });
    out.innerHTML = `<p class="fine">Working it out…</p>`;
    try { out.innerHTML = render(await api(url(v))); }
    catch (err) { out.innerHTML = `<p class="error">${esc(err.message)}</p>`; }
  });
}

function itemLink(it) {
  return `<a class="adv-item" href="#/item/${it.item_id}">${esc(it.name)}</a>`;
}

function reasons(list) {
  return `<ul class="reasons">${list.map((r) => `<li>${esc(r)}</li>`).join("")}</ul>`;
}

function renderPriceCheck(r) {
  const it = r.item;
  return `
    <div class="verdict-row">${verdictChip(r.verdict)}<span class="verdict-text">to buy ${itemLink(it)} at ${money(r.price)}</span></div>
    <div class="verdict-row">${verdictChip(r.sell_verdict)}<span class="verdict-text">to sell at that price</span></div>
    <dl class="facts">
      <div><dt>Market value</dt><dd>${money(it.market_value)}</dd></div>
      <div><dt>Lowest now</dt><dd>${money(it.min)}</dd></div>
      <div><dt>Usually sells for</dt><dd>${money(it.typical_sold_price)}</dd></div>
      <div><dt>Cheaper listed now</dt><dd>${count(r.units_listed_cheaper)}</dd></div>
    </dl>
    ${reasons(r.reasons)}${confNote(it.confidence)}`;
}

function renderSellAdvice(r) {
  const it = r.item;
  const what = r.verdict === "HOLD" ? `hold your ${count(r.qty)} ${itemLink(it)}` :
    `list ${count(r.list_now)}${r.list_now !== r.qty ? ` of ${count(r.qty)}` : ""} ${itemLink(it)}${r.suggested_price ? ` at ${money(r.suggested_price)}` : ""}`;
  return `
    <div class="verdict-row">${verdictChip(r.verdict)}<span class="verdict-text">${what}</span></div>
    ${r.verdict !== "HOLD" && r.net_if_all_sell ? `<dl class="facts">
      <div><dt>You'd get</dt><dd>${money(r.net_if_all_sell)}</dd></div>
      <div><dt>Time to sell</dt><dd>${esc(r.sells_in || "—")}</dd></div>
      <div><dt>Sold per day</dt><dd>${range(it.sold_per_day, it.sold_per_day_lo, it.sold_per_day_hi)}</dd></div>
      <div><dt>Best time to post</dt><dd>${esc(r.best_time || "not known yet")}</dd></div>
    </dl>` : ""}
    ${reasons(r.reasons)}${confNote(it.confidence)}`;
}

function renderCancelAdvice(r) {
  const it = r.item;
  const what = r.verdict === "CANCEL AND RELIST" ? `relist ${itemLink(it)} at ${money(r.relist_price)}` : `keep ${itemLink(it)} at ${money(r.my_price)}`;
  const better = Math.max(r.ride_expected_value, r.relist_expected_value) || 1;
  const bar = (label, v, sells, on) => `<div class="ev ${on ? "on" : ""}"><span>${label}</span>
    <span class="ev-bar"><i style="width:${Math.max(2, (v / better) * 100)}%"></i></span>${money(v)}<small>${sells.toFixed(1)} of ${count(r.qty)} sell</small></div>`;
  return `
    <div class="verdict-row">${verdictChip(r.verdict)}<span class="verdict-text">${what}</span></div>
    ${r.verdict === "ALREADY CHEAPEST" ? "" : `<div class="evs">
      ${bar("Let it ride", r.ride_expected_value, r.ride_expected_units_sold, r.verdict === "LET IT RIDE")}
      ${bar("Cancel & relist", r.relist_expected_value, r.relist_expected_units_sold, r.verdict === "CANCEL AND RELIST")}
    </div>`}
    ${reasons(r.reasons)}${confNote(it.confidence)}`;
}

async function loadFlips() {
  const out = $("#adv-flips .adv-out");
  let r;
  try { r = await api("/api/advice/flips?limit=12"); }
  catch (err) { out.innerHTML = `<p class="error">${esc(err.message)}</p>`; return; }
  if (!r.flips.length) {
    out.innerHTML = emptyState("Nothing worth flipping right now", "No item is listed far enough below what it reliably sells for, after the cut and deposit. That's a normal answer on a busy realm; check again next session.") + confNote(r.confidence);
    return;
  }
  out.innerHTML = `<div class="scroll-x"><table class="mini flips">
    <thead><tr><th>Item</th><th>Buy</th><th>Cost</th><th>Sell at</th><th>Profit</th><th>Sells in</th><th>Alliance</th></tr></thead>
    <tbody>${r.flips.map((f) => `<tr>
      <td><a class="adv-item" href="#/item/${f.item_id}">${esc(f.name)}</a></td>
      <td>${count(f.buy_units)} on ${factionName(f.buy_house)} up to ${money(f.buy_up_to_price)}</td>
      <td>${money(f.buy_cost)}</td>
      <td>${money(f.sell_at)} on ${factionName(f.sell_house)}</td>
      <td class="pos">${money(f.expected_profit)} <span class="sub-num">${pct(f.roi)} return</span></td>
      <td>${f.days_to_sell < 1 ? "under a day" : `${f.days_to_sell.toFixed(1)} days`}</td>
      <td title="${esc(f.alliance_note || "")}">${f.sell_house !== "blackwater" || f.buy_house === "blackwater" ? `<span class="none">—</span>`
        : f.alliance_price ? `${money(f.alliance_price)}${f.alliance_flipper_margin > 0 ? `<span class="sub-num">their flip +${plain(f.alliance_flipper_margin)}</span>` : ""}`
        : `<span class="sub-num">not listed</span>`}</td></tr>`).join("")}</tbody>
  </table></div>
  <p class="fine">Buys only what sold within 3 days at the floor rate, ignoring the single biggest sale; needs at least 5 separate sales seen. Resale is at market value or just under the cheapest competing listing, whichever is lower. Horde → Blackwater flips also need Alliance to pay at least 5% more at home than your Blackwater price.</p>${confNote(r.confidence)}`;
}

function bindAutocomplete() {
  document.querySelectorAll(".ac").forEach((box) => {
    const input = box.querySelector("input"), list = box.querySelector(".ac-list");
    let timer, items = [], active = -1;
    const close = () => { list.hidden = true; active = -1; };
    const pick = (i) => { input.value = items[i].name; close(); };
    const paint = () => {
      list.innerHTML = items.map((it, i) => `<li class="q${it.quality} ${i === active ? "active" : ""}" data-i="${i}">${esc(it.name)}</li>`).join("");
      list.hidden = !items.length;
    };
    input.addEventListener("input", () => {
      clearTimeout(timer);
      timer = setTimeout(async () => {
        if (input.value.trim().length < 2) { items = []; return close(); }
        items = await api(`/api/suggest?house=${state.house}&q=${encodeURIComponent(input.value)}`).catch(() => []);
        active = -1; paint();
      }, 150);
    });
    input.addEventListener("keydown", (e) => {
      if (list.hidden) return;
      if (e.key === "ArrowDown") { active = Math.min(items.length - 1, active + 1); paint(); e.preventDefault(); }
      else if (e.key === "ArrowUp") { active = Math.max(0, active - 1); paint(); e.preventDefault(); }
      else if (e.key === "Enter" && active >= 0) { pick(active); e.preventDefault(); }
      else if (e.key === "Escape") close();
    });
    list.addEventListener("mousedown", (e) => { const li = e.target.closest("[data-i]"); if (li) { pick(+li.dataset.i); e.preventDefault(); } });
    input.addEventListener("blur", () => setTimeout(close, 100));
  });
}

/* router */
function route() {
  hideTip();
  const h = location.hash || "#/";
  const [path, qs] = h.slice(1).split("?");
  const params = new URLSearchParams(qs || "");
  const m = path.match(/^\/item\/(\d+)/);
  const section = path.startsWith("/status") ? "status" : (m || path.startsWith("/browse")) ? "browse" : "advisor";
  document.querySelectorAll("[data-nav]").forEach((a) => a.classList.toggle("active", a.dataset.nav === section));
  $("#hero").classList.toggle("compact", section !== "browse" || !!m);
  $("#layout").classList.toggle("full", section !== "browse");
  if (m) return renderItem(m[1], params.get("variant") || "");
  if (section === "status") return renderStatus();
  if (section === "advisor") return renderAdvisor();
  refreshList();
}

window.addEventListener("hashchange", route);
renderQuality();
loadHouses().then(route).catch((err) => {
  $("#view").innerHTML = `<div class="panel">${emptyState("Can't reach the collector", esc(err.message))}</div>`;
});
setInterval(() => loadHouses().catch(() => {}), 5 * 60 * 1000);
