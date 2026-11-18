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
  // Icons with an item id show the in-game tooltip on hover, focus or tap (itemTip below).
  const tipAttrs = row.id ? ` data-tip-item="${esc(row.id)}"${row.variant ? ` data-tip-variant="${esc(row.variant)}"` : ""} tabindex="0" role="img" aria-label="${esc(row.name || "Item")}: show details"` : ` aria-hidden="true"`;
  return `<span class="icon ${big ? "big" : ""} q${row.quality ?? 1}"${style}${tipAttrs}></span>`;
}
// Faction crests are the game's own icons (honor currency), from Blizzard's icon server like the
// item icons; Blackwater (neutral) has none, so it keeps a drawn coin.
const CREST_ICON = { horde: "pvpcurrency-honor-horde", alliance: "pvpcurrency-honor-alliance" };
function crest(faction) {
  if (CREST_ICON[faction]) return `<img class="crest crest-img" src="https://render.worldofwarcraft.com/icons/56/${CREST_ICON[faction]}.jpg" alt="" aria-hidden="true">`;
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
const hideTip = () => { tip.hidden = true; tip.classList.remove("item-tip"); };

/* market hint: hold or chase (odds from a backtest, see internal/report/hint.go) */
function hintCopy(h) {
  if (!h) return null;
  const x = `${h.ratio.toFixed(h.ratio >= 10 ? 0 : 1)}×`;
  switch (h.kind) {
    case "sell": return { label: "Sell now", short: `${x} its usual price`,
      body: `Priced ${x} its usual level${h.piling ? ", and listings are piling up" : ""}. ${pct(h.odds)} of the time a price this stretched fell 10% or more within 12 hours. List at the cheapest price now, or undercut quickly.` };
    case "chase": return { label: "Chase it", short: "falling, listings piling up",
      body: `The price is dropping while listings pile up. ${pct(h.odds)} of the time it fell another 10% or more within 12 hours. Undercut quickly, or sell now.` };
    case "hold": return { label: "Hold", short: "steady at its usual price",
      body: `Steady at its usual price. ${pct(h.odds)} of the time it stayed within 10% over the next 12 hours. Post once at the cheapest price and check back later; reposting buys little.` };
    case "slow": return { label: "Slow", short: h.clear_days ? `~${num(h.clear_days)} days to sell what's listed` : "no recent sales",
      body: `${h.clear_days ? `What's listed would take about ${num(h.clear_days)} days to sell at the recent rate.` : "No certain sales recently, with stock listed."} Each cancel loses a deposit: post once, or skip it.` };
  }
  return null;
}

/* item tooltips: Blizzard's preview_item rendered like the in-game tooltip */
const QUALITY_TYPES = ["POOR", "COMMON", "UNCOMMON", "RARE", "EPIC", "LEGENDARY"];
const qualityOf = (q) => Math.max(0, QUALITY_TYPES.indexOf(q?.type));
const ds = (o) => o?.display_string || o?.display?.display_string || "";
const rgb = (c) => c ? `rgb(${c.r},${c.g},${c.b})` : "";
function previewLines(p) {
  const out = [];
  const line = (text, cls = "", style = "") => text && out.push(`<div class="tl ${cls}"${style ? ` style="color:${style}"` : ""}>${esc(text)}</div>`);
  line(p.level?.display_string, "tl-ilvl");
  line(p.binding?.name);
  line(p.unique_equipped);
  const slot = p.inventory_type?.type !== "NON_EQUIP" ? p.inventory_type?.name : "";
  const sub = p.is_subclass_hidden ? "" : p.item_subclass?.name;
  if (slot || sub) out.push(`<div class="tl tl-split"><span>${esc(slot)}</span><span>${esc(sub)}</span></div>`);
  if (p.weapon) {
    out.push(`<div class="tl tl-split"><span>${esc(ds(p.weapon.damage))}</span><span>${esc(ds(p.weapon.attack_speed))}</span></div>`);
    line(ds(p.weapon.dps));
  }
  line(ds(p.armor));
  for (const st of p.stats || []) line(ds(st), "", rgb(st.display?.color));
  line(ds(p.durability));
  for (const r of Object.values(p.requirements || {})) line(ds(r));
  for (const sp of p.spells || []) line(sp.description, "tl-use");
  if (p.description) line(p.description, p.recipe ? "tl-use" : "tl-flavor");
  return out.join("");
}
function coins(sp) {
  return sp?.value ? `<div class="tl tl-sell">Sell Price: ${money(sp.value)}</div>` : "";
}
const STAT_FULL = { STRENGTH: "Strength", AGILITY: "Agility", STAMINA: "Stamina", INTELLECT: "Intellect", SPIRIT: "Spirit", ATTACK_POWER: "Attack Power" };
function renderItemTip(t) {
  const p = t.preview || {};
  const q = t.preview ? qualityOf(p.quality) : t.quality;
  let html = `<div class="tip-head">${icon({ icon: t.icon, quality: q })}<span class="tip-name q${q}">${esc(t.suffix_name || p.name || t.name)}</span></div>`;
  html += previewLines(p);
  if (t.suffix_stats?.length || t.suffix_other) {
    // In game the suffix stats sit with the base stats; here they follow, marked when not exact.
    html += (t.suffix_stats || []).map((l) => `<div class="tl tl-suffix">+${l.min === l.max ? l.value : `${l.min}–${l.max}`} ${STAT_FULL[l.stat] || l.stat}</div>`).join("");
    if (t.suffix_other) html += `<div class="tl tl-flavor">+ ${t.suffix_other === 1 ? "another suffix stat" : `${t.suffix_other} more suffix stats`} not shown</div>`;
    if (!t.suffix_exact && t.suffix_stats?.length) html += `<div class="tl tl-flavor">Suffix range: this slot isn't checked against real gear yet.</div>`;
  }
  if (p.recipe?.item) {
    const r = p.recipe.item;
    html += `<div class="tip-sep"></div><div class="tl q${qualityOf(r.quality)}">${esc(r.name)}</div>${previewLines(r)}`;
    if (p.recipe.reagents_display_string) html += `<div class="tl">Reagents: ${esc(p.recipe.reagents_display_string)}</div>`;
  }
  html += coins(p.sell_price);
  if (!t.preview) html += `<div class="tl tl-flavor">Details appear after the collector looks this item up.</div>`;
  const hint = hintCopy(t.hint);
  html += `<div class="tip-foot"><div class="tip-foot-title">In the ${esc(factionName(t.house))} auction house</div>
    <div class="row"><span>Buyout</span>${money(t.min)}</div>
    <div class="row"><span>Market value</span>${money(t.fair)}</div>
    <div class="tip-avail">${t.available ? `${count(t.available)} available` : "None listed now"}</div>${
      hint ? `<div class="tip-hint hint-${esc(t.hint.kind)}"><b>${hint.label}</b> · ${esc(hint.short)}</div>` : ""}</div>`;
  return html;
}
const itemTips = new Map(); // key → { at, p }: prices change every scan, so entries expire
const TIP_TTL = 60000;
let tipFor = null, tipShown = null; // requested vs. actually showing
const touchOnly = () => matchMedia("(hover: none)").matches;
async function itemTip(el, ev) {
  const id = el.dataset.tipItem, variant = el.dataset.tipVariant || "";
  tipFor = el;
  const key = `${id}/${variant}/${state.house}`;
  const cached = itemTips.get(key);
  if (!cached || Date.now() - cached.at > TIP_TTL) {
    itemTips.set(key, { at: Date.now(), p: api(`/api/item/${id}/tooltip?house=${encodeURIComponent(state.house)}${variant ? `&variant=${encodeURIComponent(variant)}` : ""}`).catch((err) => { itemTips.delete(key); throw err; }) });
  }
  let t;
  try { t = await itemTips.get(key).p; } catch { return; }
  if (tipFor !== el) return; // the pointer moved on while loading
  const r = el.getBoundingClientRect();
  tip.classList.add("item-tip"); // before showTip, so it is measured at its final width
  showTip(renderItemTip(t), ev?.clientX != null ? ev : { clientX: r.right, clientY: r.top });
  tipShown = el;
}
function endItemTip() { tipFor = tipShown = null; hideTip(); }
// Touch screens fire compat mouseover/focus before the click; only the click counts there.
document.addEventListener("mouseover", (e) => { const el = e.target.closest?.("[data-tip-item]"); if (el && !touchOnly()) itemTip(el, e); });
document.addEventListener("mouseout", (e) => { const el = e.target.closest?.("[data-tip-item]"); if (el && !touchOnly() && !el.contains(e.relatedTarget)) endItemTip(); });
document.addEventListener("focusin", (e) => { const el = e.target.closest?.("[data-tip-item]"); if (el && !touchOnly()) itemTip(el); });
document.addEventListener("focusout", (e) => { if (e.target.closest?.("[data-tip-item]") && !touchOnly()) endItemTip(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && tipFor) endItemTip(); });
// Touch: a tap on an icon toggles its tooltip instead of following the row's link; a tap
// anywhere else closes it.
document.addEventListener("click", (e) => {
  if (!touchOnly()) return;
  const el = e.target.closest?.("[data-tip-item]");
  if (!el) { if (tipShown) endItemTip(); return; }
  e.preventDefault(); e.stopPropagation();
  if (tipShown === el || tipFor === el) endItemTip(); else itemTip(el, e);
}, true);

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
// A link that belongs to one house (Supply's, priced at Horde) selects it before the hash changes,
// so the item page opens on the prices the link was shown with.
document.addEventListener("click", (e) => {
  const a = e.target.closest?.("a[data-house-link]");
  const f = a?.dataset.houseLink;
  if (!f || f === state.house || !state.houses.some((h) => h.faction === f && !h.view_only)) return;
  state.house = f; store.set("house", f); renderHouses();
});
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
  const sub = [r.ilvl ? `Item level <b>${r.ilvl}</b>` : "", r.req_level > 1 ? `Requires level <b>${r.req_level}</b>` : "", r.variant && !r.suffix ? esc(r.variant) : ""].filter(Boolean).join(" · ");
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
// suffixList links every suffix of an item (listed now first); current marks the one shown.
function suffixList(id, variants, current) {
  return `<div class="suffix-list">${variants.map((v) => `
    <a class="suffix ${v.variant === current ? "here" : ""}" href="#/item/${id}?variant=${encodeURIComponent(v.variant)}">
      <span>${esc(v.suffix || v.variant)}</span>
      <span class="right">${v.available ? `${money(v.min)}<small>${count(v.available)} listed</small>` : `<small>not listed now</small>`}</span>
    </a>`).join("")}</div>`;
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
  const it = item.item, h = item.house, hint = hintCopy(it.hint);
  if (state.classes) renderBrowse();
  else api(`/api/items?house=${state.house}&limit=1`).then((d) => renderBrowse(d.classes, d.all_count)).catch(() => {});
  const variants = item.variants || [];
  // Random-suffix gear is priced per suffix; the plain item has nothing of its own to show.
  if (!variant && variants.length && !item.houses.some((x) => x.row)) {
    view.innerHTML = `
      <a class="back" href="#/browse">← Back to the auction house</a>
      <section class="item-head qh${it.quality}">
        ${icon(it, true)}
        <div><h2 class="q${it.quality}">${esc(it.name)}</h2><div class="meta">Comes with a random suffix · each one has its own prices and history</div></div>
      </section>
      <section class="panel"><h3 class="panel-title">Pick a suffix <span class="aside">${factionName(h.faction)} auction house</span></h3>${suffixList(id, variants, "")}</section>`;
    return;
  }
  const sub = [`<span class="q${it.quality}">${QUALITY[it.quality] || ""}</span>`, it.ilvl ? `Item level <b>${it.ilvl}</b>` : "",
    it.req_level > 1 ? `Requires level <b>${it.req_level}</b>` : "", it.subclass ? esc(it.subclass) : "", `Item ID <b>${it.id}</b>`].filter(Boolean);
  view.innerHTML = `
    <a class="back" href="#/browse">← Back to the auction house</a>
    <section class="item-head qh${it.quality}">
      ${icon(it, true)}
      <div><h2 class="q${it.quality}">${esc(it.name)}${it.variant && !it.suffix ? ` <small class="item-sub">${esc(it.variant)}</small>` : ""}</h2><div class="meta">${sub.join("<span aria-hidden='true'>◆</span>")}</div></div>
      <div class="best"><small>Lowest buyout · ${factionName(h.faction)}</small>${money(it.min, { zero: "Not listed" })}</div>
    </section>
    ${hint ? `<div class="hint-bar hint-${esc(it.hint.kind)}" role="note"><b>${hint.label}</b><span>${esc(hint.body)}</span></div>` : ""}
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
        <section class="panel">
          <h3 class="panel-title">Price distribution <span class="aside">listed now · bought in the last 48 h</span></h3>
          <div class="chart book-plot" id="book"><div class="empty-state">Loading the order book…</div></div>
          <div class="legend"><span class="l-listed">Units listed now</span><span class="l-sold">Units bought, last 48 h</span><span class="l-fair">Market value</span><span class="l-paid">Median paid, 24 h</span></div>
          <p class="fine" id="book-note">Hover a price to see what buying everything up to it costs.</p>
        </section>
        <section class="panel when" id="when"></section>
      </div>
      <div class="stack">
        ${variants.length ? `<section class="panel"><h3 class="panel-title">Suffixes <span class="aside">this house</span></h3>${suffixList(id, variants, variant)}</section>` : ""}
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
  // The page may have moved on (another item or house) by the time the book arrives.
  const bookFor = `${id}|${variant || ""}|${state.house}`;
  $("#book").dataset.for = bookFor;
  const bookHere = () => $("#book")?.dataset.for === bookFor;
  api(`/api/item/${id}/book?house=${state.house}${vq}`).then((b) => { if (bookHere()) drawBook(b, it.fair); })
    .catch((err) => { if (bookHere()) $("#book").innerHTML = emptyState("No order book", esc(err.message)); });
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
/* price distribution: units listed at each price above the axis, units bought below it, on one
   unit scale. Prices are grouped in log steps so a dump at a tenth of the price and the wall
   at market value share one chart. */
function drawBook(b, fair) {
  const el = $("#book");
  if (!el) return;
  const listed = b.listed || [], sold = b.sold || [];
  if (!listed.length && !sold.length) {
    // Price-only houses keep no individual auctions, so there's no book to draw.
    el.innerHTML = b.tracked ? emptyState("Nothing listed", "No open listings and no recent purchases on this house.")
      : emptyState("No order book", "Auctions aren't tracked on this house (price only), so its listings by price aren't known.");
    $("#book-note").textContent = "";
    return;
  }
  // Range: cheapest price seen, up to the dearest sale or the 95th-percentile listed unit
  // (and market value), so a single absurd listing doesn't squash the rest.
  const total = listed.reduce((a, l) => a + l.q, 0);
  let acc = 0, p95 = listed.length ? listed[listed.length - 1].p : 0;
  for (const l of listed) { acc += l.q; if (acc >= total * 0.95) { p95 = l.p; break; } }
  const prices = [...listed.map((l) => l.p), ...sold.map((l) => l.p)];
  const lo = Math.min(...prices);
  const hi = Math.max(lo * 1.2, p95, sold.length ? sold[sold.length - 1].p : 0, fair || 0, b.recent_price || 0) * 1.02;
  const W = el.clientWidth, H = el.clientHeight, padL = 44, padR = 12, padT = 18, padB = 22;
  const nBins = Math.max(12, Math.min(48, Math.floor((W - padL - padR) / 9)));
  const lr = Math.log(hi / lo) || 1;
  const binOf = (p) => Math.min(nBins - 1, Math.max(0, Math.floor((Math.log(p / lo) / lr) * nBins)));
  const bins = Array.from({ length: nBins }, (_, i) => ({ from: lo * Math.exp((lr * i) / nBins), to: lo * Math.exp((lr * (i + 1)) / nBins), q: 0, n: 0, sq: 0, sn: 0 }));
  let above = 0;
  for (const l of listed) { if (l.p > hi) { above += l.q; continue; } const x = bins[binOf(l.p)]; x.q += l.q; x.n += l.n; }
  for (const l of sold) { if (l.p > hi) continue; const x = bins[binOf(l.p)]; x.sq += l.q; x.sn += l.n; }
  const maxQ = Math.max(1, ...bins.map((x) => Math.max(x.q, x.sq)));
  const plotW = W - padL - padR, bw = plotW / nBins;
  const showSold = b.tracked;
  const mid = showSold ? padT + (H - padT - padB) * 0.6 : H - padB;
  const upH = mid - padT, downH = H - padB - mid - 4;
  const xOf = (p) => padL + (Math.log(p / lo) / lr) * plotW;
  const up = (q) => (q / maxQ) * upH, down = (q) => (q / maxQ) * downH;
  const bars = bins.map((x, i) => {
    const x0 = padL + i * bw + 1, w = Math.max(1, bw - 2);
    const u = x.q ? Math.max(2, up(x.q)) : 0, d = x.sq ? Math.max(2, down(x.sq)) : 0;
    return (u ? `<rect class="bk-listed" x="${x0.toFixed(1)}" y="${(mid - u).toFixed(1)}" width="${w.toFixed(1)}" height="${u.toFixed(1)}" rx="2"/>` : "") +
      (showSold && d ? `<rect class="bk-sold" x="${x0.toFixed(1)}" y="${(mid + 4).toFixed(1)}" width="${w.toFixed(1)}" height="${d.toFixed(1)}" rx="2"/>` : "");
  }).join("");
  const ref = (p, cls, label, anchorRight) => p && p >= lo && p <= hi ? `<line class="${cls}" x1="${xOf(p)}" x2="${xOf(p)}" y1="${padT - 4}" y2="${H - padB}"/>
    <text class="bk-ref-lbl" x="${xOf(p) + (anchorRight ? -4 : 4)}" y="${padT + 6}" text-anchor="${anchorRight ? "end" : "start"}">${label} ${shortGold(p)}</text>` : "";
  // Keep the two reference labels apart: the lower price's label points left.
  const paid = b.recent_price, lowerIsFair = fair && paid ? fair < paid : true;
  const ticks = [];
  for (let i = 0; i <= 4; i++) {
    const p = lo * Math.exp((lr * i) / 4);
    ticks.push(`<text x="${padL + (plotW * i) / 4}" y="${H - 6}" text-anchor="${i === 0 ? "start" : i === 4 ? "end" : "middle"}">${shortGold(p)}</text>`);
  }
  const yl = `<text x="${padL - 8}" y="${padT + 4}" text-anchor="end">${count(maxQ)}</text>${showSold ? `<text x="${padL - 8}" y="${H - padB}" text-anchor="end">${count(maxQ)}</text>` : ""}<text x="${padL - 8}" y="${mid + 4}" text-anchor="end">0</text>`;
  el.innerHTML = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Units listed and bought by price">
    <g class="axis">${ticks.join("")}${yl}</g>
    <line class="bk-axis" x1="${padL}" x2="${W - padR}" y1="${mid}" y2="${mid}"/>
    ${bars}
    ${ref(fair, "bk-fair", "market", lowerIsFair)}${ref(paid, "bk-paid", "paid", !lowerIsFair)}
    <rect class="bk-hover" id="bk-hover" y="${padT - 4}" height="${H - padB - padT + 4}" width="${bw}" visibility="hidden"/>
  </svg>`;
  $("#book-note").textContent = `${above ? `${count(above)} more listed above ${shortGold(hi)}, off the chart. ` : ""}${
    showSold ? "Purchases are what the scans can see: gear counts only if no cheaper copy outlasted it." : "Sales aren't tracked on this house (price only)."} Hover a price to see what buying everything up to it costs.`;
  const svg = el.querySelector("svg"), hov = $("#bk-hover");
  const pick = (e) => {
    const r = svg.getBoundingClientRect();
    const mx = ((e.clientX - r.left) / r.width) * W;
    const i = Math.floor((mx - padL) / bw);
    if (i < 0 || i >= nBins) { hov.setAttribute("visibility", "hidden"); hideTip(); return; }
    const x = bins[i];
    // Buying every listing up to the end of this step, cheapest first.
    let units = 0, cost = 0, next = 0;
    for (const l of listed) {
      if (l.p < x.to) { units += l.q; cost += l.q * l.p; } else { next = l.p; break; }
    }
    hov.setAttribute("x", padL + i * bw); hov.setAttribute("visibility", "visible");
    showTip(`<b>${plain(Math.round(x.from))} – ${plain(Math.round(x.to))}</b>
      <div class="row"><span>Listed here</span>${count(x.q)} in ${count(x.n)} auction${x.n === 1 ? "" : "s"}</div>
      ${showSold ? `<div class="row"><span>Bought, 48 h</span>${count(x.sq)} in ${count(x.sn)} sale${x.sn === 1 ? "" : "s"}</div>` : ""}
      <div class="row"><span>Buy all up to here</span>${count(units)} for ${plain(cost)}</div>
      <div class="row"><span>Next listing then</span>${next ? plain(next) : "none"}</div>`, e);
  };
  svg.addEventListener("mousemove", pick);
  svg.addEventListener("mouseleave", () => { hov.setAttribute("visibility", "hidden"); hideTip(); });
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
      <div class="kv"><div><b>Version</b><small>build running the collector</small></div><div class="val">${esc(s.version || "unknown")}</div></div>
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

/* flips: small trades for the gold you have and how often you trade */
const KIND = {
  relist: ["Relist", "Buy listings dumped under the next price and relist just under it, in the same house."],
  cross: ["Cross", "Buy on one house, sell on the other."],
  alliance: ["Alliance", "Buy on Horde, sell on Blackwater for less than Alliance players pay at home."],
};
// Slider positions map to gold on a log scale: 5g … 2000g.
const GOLD_MIN = 5, GOLD_MAX = 2000;
const toGold = (pos) => Math.round(GOLD_MIN * Math.pow(GOLD_MAX / GOLD_MIN, pos / 1000));
const toPos = (gold) => Math.round(1000 * Math.log(gold / GOLD_MIN) / Math.log(GOLD_MAX / GOLD_MIN));
// Mirrors styleProfiles in internal/report/flips.go.
const STYLE = {
  active: { label: "Active", share: 0.5, note: "About hourly · hold, check in, camp" },
  casual: { label: "Casual", share: 0.4, note: "4–5 times a day · hold, check in" },
  away: { label: "Away", share: 0.3, note: "Once or twice a day · hold" },
};
// Older names, kept in the browser, map to the nearest style.
const OLD_STYLE = { high: "active", medium: "casual", low: "away", regular: "casual" };
// Mirrors playProfiles: what to do with the listing once it's up.
const PLAY = {
  hold: ["Hold", "Leave it up, even when undercut."],
  check_in: ["Check in", "Look every few hours; relist under the cheapest when more than this many are listed cheaper than yours."],
  camp: ["Camp", "Check hourly; relist under the cheapest whenever undercut."],
};
const houseTag = (h) => `<span class="house-tag house-${esc(h)}">${h === "blackwater" ? "Neutral" : esc(h)}</span>`;
const cancelIf = (f) => f.play === "hold" ? "—" : f.play === "check_in" && f.relist_queue ? count(f.relist_queue) : "undercut";

function renderFlips() {
  const gold = Math.min(GOLD_MAX, Math.max(GOLD_MIN, store.get("gold", 40)));
  let style = store.get("style", "") || OLD_STYLE[store.get("risk", "")] || "casual";
  style = OLD_STYLE[style] || style;
  if (!Object.hasOwn(STYLE, style)) style = "casual";
  $("#view").innerHTML = `
    <section class="panel flips-panel">
      <div class="flips-head">
        <div>
          <h2 class="panel-title">Flips</h2>
          <p class="flips-lede">Trades for your gold and how often you play. Updates after every scan.</p>
        </div>
        <div class="gold-control">
          <label for="gold-num">Your gold</label>
          <div class="gold-row">
            <input type="range" id="gold-range" min="0" max="1000" value="${toPos(gold)}" aria-label="Your gold">
            <span class="gold-input"><input id="gold-num" inputmode="decimal" value="${gold}"><span>g</span></span>
          </div>
          <small id="gold-note"></small>
        </div>
        <div class="risk-control" role="radiogroup" aria-label="How you trade">
          <span class="risk-label">How you trade</span>
          <div class="risk-seg">${Object.entries(STYLE).map(([k, r]) => `<button type="button" role="radio" data-style="${k}" aria-checked="${k === style}">${r.label}</button>`).join("")}</div>
          <small id="risk-note"></small>
        </div>
      </div>
      <div id="flips-out" aria-live="polite"><div class="empty-state">Finding flips…</div></div>
      <details class="rules">
        <summary>How flips are chosen</summary>
        <ul class="reasons">
          <li><b>Plays.</b> Hold: post 24h and leave it, even when undercut; commodities sell the cheaper listings first, then yours. Check in (casual, active): look every ~5h and relist under the cheapest when more units are listed cheaper than yours than the <b>Cancel if position</b> number, which is what buyers take before your listing runs out. Camp (active): check hourly and relist whenever undercut.</li>
          <li>Each item is planned with every play your style allows and the most profitable one is shown, so active sees everything casual does, and casual everything away does.</li>
          <li><b>Priced from what buyers pay.</b> The resale is capped at the median price paid over the last 24 hours, and needs at least 2 purchases at or above it in the last 12 hours. What you buy, plus everything listed within 10% of your price, must sell within a day (12 hours for camping).</li>
          <li><b>Sliding or swinging prices</b> (asking price down over 10% a day, or a resale more than 15% under market value) are left to camping.</li>
          <li><b>Profit</b> is expected profit: weighed by how often this item's cheapest listings sold, and were undercut, within one round of the play. A relist costs the deposit and about 3%; stock left unsold is valued at what buyers pay then. Hover it for the chance it all sells.</li>
          <li>Ranked by profit. One flip uses at most 50%, 40% or 30% of your gold (active, casual, away) and needs a 20%, 20% or 25% return after the cut (5% Horde, 15% Neutral). Away sells on Horde only.</li>
          <li>Neutral resale is never above the dearer of the Horde and Alliance prices. Horde → Neutral flips sell to Alliance players, only for items they buy often.</li>
          <li><b>Going fast:</b> about half of underpriced listings are gone within an hour of a scan. Buy soon after a scan or skip it.</li>
          <li>Gear only counts as sold if no cheaper copy outlasted it (it may have been cancelled). Anything listed and bought within the hour is never seen.</li>
        </ul>
      </details>
    </section>`;
  const range = $("#gold-range"), num = $("#gold-num");
  let timer, cur = gold;
  const refresh = () => {
    const r = STYLE[style], most = cur * r.share;
    $("#gold-note").textContent = `One flip uses at most ${most >= 1 ? Math.floor(most) + "g" : Math.round(r.share * 100) + "%"}.`;
    $("#risk-note").textContent = r.note;
    document.querySelectorAll("[data-style]").forEach((b) => b.setAttribute("aria-checked", String(b.dataset.style === style)));
    clearTimeout(timer);
    timer = setTimeout(() => loadFlips(cur, style), 120);
  };
  const set = (g, from) => {
    g = Math.min(GOLD_MAX, Math.max(GOLD_MIN, Math.round(g)));
    if (from !== "num") num.value = g;
    if (from !== "range") range.value = toPos(g);
    store.set("gold", g);
    cur = g;
    refresh();
  };
  range.addEventListener("input", () => set(toGold(+range.value), "range"));
  num.addEventListener("input", () => { const v = parseFloat(num.value); if (v > 0) set(v, "num"); });
  document.querySelectorAll("[data-style]").forEach((b) => b.addEventListener("click", () => {
    style = b.dataset.style;
    store.set("style", style);
    refresh();
  }));
  set(gold);
}

// Live updates: the page polls a cheap version endpoint and reloads the flips after each scan,
// marking flips that weren't there before and showing a toast (for a second monitor).
// gold/style are what the table shows; want is what the controls last asked for, which the
// poll uses so a background refresh never reverts a change that is still loading.
const live = { gold: 0, style: "", want: null, computed: 0, keys: null, newCount: 0, timer: 0, tick: 0 };
const flipKey = (f) => `${f.item_id}:${f.variant || ""}:${f.buy_house}>${f.sell_house}`;

function startLive() {
  stopLive();
  live.timer = setInterval(async () => {
    try {
      const v = await api("/api/flips/version");
      if (v.computed_at > live.computed && live.computed) loadFlips(live.want.gold, live.want.style, true);
    } catch { /* collector briefly unreachable: try again next tick */ }
  }, 20000);
  live.tick = setInterval(updateAge, 30000);
}
function stopLive() {
  clearInterval(live.timer); clearInterval(live.tick);
  live.timer = live.tick = 0;
  clearNewCount();
}
function clearNewCount() {
  live.newCount = 0;
  document.title = document.title.replace(/^\(\d+\) /, "");
}
["pointerdown", "keydown"].forEach((e) => window.addEventListener(e, clearNewCount));

function updateAge() {
  const el = $("#scan-age");
  if (el && live.computed) el.textContent = ago(live.computed);
}

function toast(html) {
  let el = $("#toast");
  if (!el) {
    el = document.createElement("div");
    el.id = "toast"; el.className = "toast"; el.setAttribute("role", "status");
    document.body.append(el);
    el.addEventListener("click", () => el.classList.remove("show"));
  }
  el.innerHTML = html;
  el.classList.add("show");
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.remove("show"), 12000);
}

let flipsSeq = 0;
async function loadFlips(gold, style, fromScan = false) {
  const seq = ++flipsSeq;
  if (!fromScan) live.want = { gold, style };
  let r;
  try { r = await api(`/api/flips?gold=${gold}&style=${style}&limit=30`); }
  catch (err) {
    const out = $("#flips-out");
    if (out && seq === flipsSeq) out.innerHTML = emptyState("No flips yet", esc(err.message));
    return;
  }
  // A slower response for an earlier gold amount or style must not overwrite a newer one.
  const out = $("#flips-out");
  if (!out || seq !== flipsSeq) return;
  // New flips: only meaningful when the scan changed, not when gold or style did.
  const keys = new Set(r.flips.map(flipKey));
  const sameView = live.gold === gold && live.style === style;
  const prevKeys = live.keys; // live.keys is replaced below, before the rows are drawn
  const isNew = (f) => fromScan && sameView && prevKeys && !prevKeys.has(flipKey(f));
  const fresh = r.flips.filter(isNew).length;
  if (fromScan && sameView && r.computed_at > live.computed) {
    const at = new Date(r.computed_at * 1000).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
    toast(`<b>New scan at ${at}</b><span>${fresh ? `${fresh} new flip${fresh > 1 ? "s" : ""}` : "No new flips"} · ${r.flips.length} in total</span>`);
    if (fresh) {
      live.newCount += fresh;
      document.title = `(${live.newCount}) ${document.title.replace(/^\(\d+\) /, "")}`;
    }
  }
  Object.assign(live, { gold, style, computed: r.computed_at, keys });
  if (!live.timer) startLive();
  const updated = `<p class="fine">Listings as of the scan <span id="scan-age">${ago(r.computed_at)}</span>; this page updates itself after each scan. About half of underpriced listings are gone within an hour of a scan.${
    r.confidence === "good" ? "" : r.confidence === "early" ? " Early estimate: under 3 days of sales history." : " Fair estimate: under 10 days of history."}${
    r.alliance === "tracked" ? "" : " Alliance sales aren't tracked (price_only in config), so Alliance flips are off."}</p>`;
  if (!r.flips.length) {
    out.innerHTML = emptyState(`Nothing qualifies with ${gold}g for ${STYLE[style].label.toLowerCase()} trading right now`, "That's a normal answer: nothing cheap enough sells fast enough, at a price buyers are paying. Checking more often (active) adds camping, for busier, faster-moving markets. Here's what ruled things out:") + funnelTable(r.funnel) + updated;
    return;
  }
  out.innerHTML = `<div class="scroll-x"><table class="mini flips">
    <thead><tr><th>Item</th><th>Buy from</th><th>Sell at</th><th>Buy at</th><th>List at</th><th>Profit</th><th>Play</th><th title="Units listed cheaper than yours">Cancel if position &gt;</th></tr></thead>
    <tbody>${r.flips.map((f) => `<tr class="${isNew(f) ? "is-new" : ""}">
      <td><div class="item-cell">${icon({ id: f.item_id, name: f.name, icon: f.icon, quality: f.quality })}<div>
        <a class="adv-item q${f.quality ?? 1}" href="#/item/${f.item_id}${f.variant ? `?variant=${encodeURIComponent(f.variant)}` : ""}" title="${esc(f.why)}">${esc(f.name)}</a>${
        isNew(f) || f.gone_soon >= 0.5 ? `<div class="flip-tags">${isNew(f) ? `<span class="tag tag-new">New</span>` : ""}${
        f.gone_soon >= 0.5 ? `<span class="tag tag-hot" title="About ${pct(f.gone_soon)} of listings like this are gone within an hour of a scan">Going fast</span>` : ""}</div>` : ""}</div></div></td>
      <td data-label="Buy from">${houseTag(f.buy_house)}</td>
      <td data-label="Sell at">${houseTag(f.sell_house)}</td>
      <td data-label="Buy at">${count(f.buy_units)} × ≤ ${money(f.buy_up_to_price)}</td>
      <td data-label="List at">${money(f.sell_at)} <span class="dur">${f.list_hours}h</span></td>
      <td data-label="Profit" class="pos" title="${pct(f.sold_out_chance)} chance it all sells · ${plain(f.profit_if_sold)} if it does">${money(f.expected_profit)}</td>
      <td data-label="Play"><span class="play-name play-${esc(f.play)}" title="${esc(PLAY[f.play]?.[1] || "")}">${PLAY[f.play]?.[0] || esc(f.play)}</span></td>
      <td data-label="Cancel if position >" class="num">${cancelIf(f)}</td></tr>`).join("")}</tbody>
  </table></div>${updated}`;
}

function funnelTable(funnel) {
  const routes = Object.entries(funnel || {}).filter(([, f]) => f.candidates > 0).sort();
  if (!routes.length) return "";
  return `<div class="scroll-x"><table class="mini funnel">
    <thead><tr><th>Route</th><th>Items checked</th><th>Sells too slowly</th><th>Nobody paid it lately</th><th>Price falling</th><th>Deposit too high</th><th>No price gap</th><th>Needs more gold</th><th>Not listed to buy</th></tr></thead>
    <tbody>${routes.map(([route, f]) => `<tr><td>${esc(route.replace(/(^|→ )(\w)/g, (m) => m.toUpperCase()))}</td><td>${count(f.candidates)}</td>
      <td>${count(f.sells_too_slowly)}</td><td>${count(f.unproven || 0)}</td><td>${count(f.price_falling || 0)}</td><td>${count(f.deposit_too_high || 0)}</td><td>${count(f.no_profitable_gap)}</td><td>${count(f.needs_more_gold)}</td><td>${count(f.not_listed_to_buy)}</td></tr>`).join("")}</tbody>
  </table></div>`;
}

/* supply: Durotar Supply and Logistics crates (Favor) and writs (reputation) */
// Gold per rep: whole copper, "<1c" below that; negative means the writ pays you.
const perRep = (v) => {
  if (!v) return "free";
  const c = Math.round(Math.abs(v)), amount = c ? plain(c) : "<1c";
  return v < 0 ? `earns ${amount}` : amount;
};
// Supply is priced at Horde, so its item links open the item page on Horde (see data-house-link).
// A fill never seen on the AH has no item id (plan.go leaves it 0): plain text, no item page.
function supplyLink(id, name, cls = "") {
  if (!id) return cls ? `<span class="${cls}">${esc(name)}</span>` : esc(name);
  return `<a class="item-link ${cls}" href="#/item/${id}" data-house-link="horde">${esc(name)}</a>`;
}
function supplyItem(items, id, extra = "") {
  const it = items[id] || { name: `Item #${id}`, quality: 1 };
  return `<div class="item-cell">${icon({ id, ...it })}<div>${supplyLink(id, it.name, `item-name q${it.quality}`)}${extra}</div></div>`;
}
function inScope(w, scope) {
  if (scope === "world") return true;
  if (scope.startsWith("c:")) return w.continent === scope.slice(2);
  return w.customer.zone === scope.slice(2);
}
/* upgrades: an armory-style paper doll with the best upgrade under each slot */
const DOLL_LEFT = ["HEAD", "NECK", "SHOULDER", "BACK", "CHEST", "SHIRT", "TABARD", "WRIST"];
const DOLL_RIGHT = ["HANDS", "WAIST", "LEGS", "FEET", "FINGER_1", "FINGER_2", "TRINKET_1", "TRINKET_2"];
const DOLL_BOTTOM = ["MAIN_HAND", "OFF_HAND", "RANGED"];
const DOLL_NAME = { HEAD: "Head", NECK: "Neck", SHOULDER: "Shoulder", BACK: "Back", CHEST: "Chest", SHIRT: "Shirt", TABARD: "Tabard",
  WRIST: "Wrist", HANDS: "Hands", WAIST: "Waist", LEGS: "Legs", FEET: "Feet", FINGER_1: "Ring", FINGER_2: "Ring", TRINKET_1: "Trinket",
  TRINKET_2: "Trinket", MAIN_HAND: "Main hand", OFF_HAND: "Off hand", RANGED: "Idol / ranged" };
// upgrade rows use grouped slots; map a doll slot to its group
const dollGroup = (k) => k.startsWith("FINGER") ? "FINGER" : k.startsWith("TRINKET") ? "TRINKET" : k === "HANDS" ? "HAND" : k;
const WEIGHT_ORDER = ["DPS", "STRENGTH", "AGILITY", "ATTACK_POWER", "CRIT_RATING", "HIT_RATING", "STAMINA", "INTELLECT", "SPIRIT"];
const STAT_SHORT = { DPS: "DPS", STRENGTH: "Str", AGILITY: "Agi", STAMINA: "Sta", INTELLECT: "Int", SPIRIT: "Spi", ATTACK_POWER: "AP", CRIT_RATING: "Crit", HIT_RATING: "Hit" };
function statLine(it) {
  const parts = Object.entries(it.stats || {}).filter(([, v]) => v).map(([k, v]) => `+${v} ${STAT_SHORT[k] || k.toLowerCase().replace(/_/g, " ")}`);
  if (it.dps) parts.unshift(`${it.dps.toFixed(1)} dps`);
  const line = parts.join(" · ");
  return it.approx && line ? `<span class="approx" title="Suffix stats worked out from the game data; this slot isn't checked against a real character yet, so they may be off by a point or two.">≈ ${line}</span>` : line;
}
function signed(v) { return !v ? `<span class="none">—</span>` : `<span class="${v > 0 ? "up" : "down"}">${v > 0 ? "+" : ""}${Math.round(v)}</span>`; }
const itemHref = (it) => `#/item/${it.id}${it.variant ? `?variant=${encodeURIComponent(it.variant)}` : ""}`;
function dollIcon(it) {
  const q = it ? it.quality ?? 1 : -1;
  return `<span class="doll-icon q-border${q}">${it && it.icon ? `<img src="${esc(it.icon)}" alt="" loading="lazy">` : ""}</span>`;
}
// For a ring/trinket the upgrade belongs to the one it replaces (or the empty one).
function upgradesFor(key, doll, rows) {
  const g = dollGroup(key);
  return rows.filter((r) => {
    if (r.slot !== g) return false;
    if (g !== "FINGER" && g !== "TRINKET") return true;
    const mine = doll[key];
    const rep = (r.replaces || [])[0];
    // id and suffix: two copies of a ring can carry different suffixes
    return rep ? mine && rep.id === mine.id && (rep.variant || "") === (mine.variant || "") : !mine;
  });
}
function dollSlot(key, side, doll, rows, open) {
  const it = doll[key], ups = upgradesFor(key, doll, rows), best = ups[0];
  return `<button class="doll-slot ${side} ${open === key ? "open" : ""}" data-slot="${key}" type="button">
    ${dollIcon(it)}
    <span class="doll-text">
      ${it ? `<span class="doll-name q${it.quality}">${esc(it.name)}</span>
        <span class="doll-sub">${it.ilvl || ""}${it.enchant ? ` <span class="doll-ench">${esc(it.enchant)}</span>` : ""}</span>`
        : `<span class="doll-name empty">${DOLL_NAME[key]}</span>`}
      ${best ? `<span class="doll-up">▲ +${best.gain} · ${plain(best.item.price)}${ups.length > 1 ? ` <em>${ups.length} options</em>` : ""}</span>` : ""}
    </span>
  </button>`;
}
let upSortNote = "best gain per gold first";
function upgradeList(filter, doll, rows) {
  const list = filter ? upgradesFor(filter, doll, rows) : rows;
  const it = filter && doll[filter];
  const head = filter
    ? `<div class="up-filter"><span>${DOLL_NAME[filter]}${it ? ` · wearing <span class="q${it.quality}">${esc(it.name)}</span>` : " · empty"}</span>
        <button type="button" class="up-clear" data-clear title="Show every slot">✕</button></div>`
    : `<div class="up-filter all"><span>All slots · ${upSortNote}</span></div>`;
  if (!list.length) return head + emptyState("Nothing better listed", "No item clears the minimum gain with these filters.");
  return head + `<ol class="up-list">${list.slice(0, 60).map((r) => { const x = r.item; return `<li>
    ${icon({ id: x.id, variant: x.variant, name: x.name, icon: x.icon, quality: x.quality })}
    <div class="up-main">
      <a class="adv-item q${x.quality}" href="${itemHref(x)}">${esc(x.name)}</a>
      <div class="item-sub">${filter ? "" : `${SLOT_NAME[r.slot] || r.slot} · `}${x.req_level > 1 ? `lvl ${x.req_level} · ` : ""}${statLine(x)}${r.ahead ? ` · <span class="tag tag-hot">level ${x.req_level}</span>` : ""}</div>
      <div class="up-deltas">${r.health ? `<span>${signed(r.health)} health</span>` : ""}${r.mana ? `<span>${signed(r.mana)} mana</span>` : ""}${r.spirit ? `<span>${signed(r.spirit)} spirit</span>` : ""}${(x.flags || []).map((f) => `<span class="vol">${esc(f)}</span>`).join("")}</div>
    </div>
    <div class="up-num"><div class="line"><b>+${r.gain}</b><small>${r.per_gold}/g</small></div><div class="line">${money(x.price)}<small>${factionName(x.house)}</small></div></div>
  </li>`; }).join("")}</ol>${list.length > 60 ? `<p class="fine">Top 60 of ${list.length}. Raise the minimum gain or set a max price to narrow it.</p>` : ""}`;
}
const SLOT_NAME = { HEAD: "Head", NECK: "Neck", SHOULDER: "Shoulder", BACK: "Back", CHEST: "Chest", WRIST: "Wrist", HAND: "Hands",
  WAIST: "Waist", LEGS: "Legs", FEET: "Feet", FINGER: "Ring", TRINKET: "Trinket", MAIN_HAND: "Main hand", OFF_HAND: "Off hand", RANGED: "Idol / ranged" };
const DOLL_STATS = [["health", "Health", "hp"], ["power", "Mana", "mana"], ["strength", "Strength", "str"], ["agility", "Agility", "agi"],
  ["stamina", "Stamina", "sta"], ["intellect", "Intellect", "int"], ["spirit", "Spirit", "spi"], ["attack_power", "Attack power", "ap"], ["melee_crit", "Crit", "crit"]];
const INV_NAME = { HEAD: "head", NECK: "neck", SHOULDER: "shoulder", CLOAK: "back", CHEST: "chest", ROBE: "robe", WRIST: "wrist",
  HAND: "hands", WAIST: "waist", LEGS: "legs", FEET: "feet", FINGER: "rings", TRINKET: "trinkets", TWOHWEAPON: "two-handers",
  WEAPON: "one-handers", WEAPONMAINHAND: "main-hand weapons", HOLDABLE: "off-hand items", SHIELD: "shields", RANGED: "ranged", RELIC: "idols" };
function checkedLine() {
  return `<p class="fine up-checked" title="Solved from 663 fixed-stat items: one budget column reproduces Blizzard's stats for 100% of each slot's items.">Suffix stats are exact for every slot except trinkets, idols, thrown and off-hand weapons (≈).</p>`;
}
let upGen = 0; // bumped by every Upgrades request and page render
async function renderUpgrades() {
  upGen++;
  const f = { buyOnce: store.get("upBuyOnce2", false), character: store.get("upChar", "Sine Wave"), ahead: store.get("upAhead", false), minGain: store.get("upMinGain", 3), maxPrice: store.get("upMaxPrice", "") };
  if (f.character === "sine-wave") f.character = "Sine Wave";
  let open = "";
  $("#view").innerHTML = `
    <form class="up-form" id="up-form">
      <label>Character <input name="character" value="${esc(f.character)}" size="16" placeholder="First Last"></label>
      <label title="Biggest upgrade first instead of the most per gold, and skip items more than 10 levels below you: buy once, not every few levels."><input type="checkbox" name="buyOnce" ${f.buyOnce ? "checked" : ""}> Buy once</label>
      <label><input type="checkbox" name="ahead" ${f.ahead ? "checked" : ""}> include the next 3 levels</label>
      <label>Min gain <input name="minGain" type="number" step="1" value="${esc(String(f.minGain))}" style="width:4em"></label>
      <label>Max price <input name="maxPrice" type="number" step="0.1" placeholder="gold" value="${esc(String(f.maxPrice))}" style="width:5em"></label>
      <button class="btn">Load</button>
      <span class="up-help" tabindex="0" aria-label="How to use this">?<span class="up-tip" role="tooltip">
        <b>Name</b> as on the armory, e.g. Sine Wave.<br>
        <b>Gain</b> = stat points over what you wear. <b>Buy once</b>: biggest upgrade first, nothing more than 10 levels below you; off: most gain per gold.<br>
        <b>Click a slot</b> to filter, ✕ to clear.<br>
        <b>≈</b> suffix stats not yet checked on real gear.</span></span>
      <span class="tag tag-new">preview</span>
    </form>
    <div id="up-out" aria-live="polite"></div>`;
  const form = $("#up-form");
  let d;
  const draw = () => {
    const out = $("#up-out"), c = d.character, doll = c.doll || {}, rows = d.rows || [];
    upSortNote = f.buyOnce ? `biggest upgrade first · level ${Math.max(1, c.level - 10)}+` : "best gain per gold first";
    out.innerHTML = `<div class="up-layout">
      <section class="doll">
        ${c.render ? `<img class="doll-render" src="${esc(c.render)}" alt="">` : ""}
        <header class="doll-head">${crest(c.faction)}
          <div><h2>${esc(c.name)}${c.surname ? ` ${esc(c.surname)}` : ""}</h2>
          <div class="doll-meta"><b>${c.equipped_item_level} ilvl</b> · ${c.level} ${esc(c.race)} ${esc(c.class)}</div></div>
        </header>
        <div class="doll-grid">
          <div class="doll-col">${DOLL_LEFT.map((k) => dollSlot(k, "left", doll, rows, open)).join("")}</div>
          <div class="doll-mid"></div>
          <div class="doll-col">${DOLL_RIGHT.map((k) => dollSlot(k, "right", doll, rows, open)).join("")}</div>
        </div>
        <div class="doll-bottom">${DOLL_BOTTOM.map((k, i) => dollSlot(k, i === 0 ? "right" : "left", doll, rows, open)).join("")}</div>
        <div class="doll-stats">${DOLL_STATS.filter(([k]) => c.stats[k] != null).map(([k, label, cls]) => `<div class="doll-stat ${cls}"><b>${k === "melee_crit" ? `${c.stats[k].toFixed(1)}%` : count(c.stats[k])}</b><span>${label}</span></div>`).join("")}</div>
      </section>
      <aside class="panel up-side">
        <h3 class="panel-title">Upgrades <span class="aside">${count(d.candidates)} usable items listed</span></h3>
        <form class="up-weights" id="up-weights" title="Points per stat point (weapon: per DPS). Hover a stat for where its weight comes from; edits are kept in this browser.">
          <span class="up-weights-name">${esc(d.preset.name)}</span>
          ${WEIGHT_ORDER.filter((k) => k in d.weights).map((k) => `<label title="${esc(d.preset.sources[k] || "")}">${STAT_SHORT[k] || k}<input name="${k}" type="number" min="0" max="100" step="0.1" value="${+d.weights[k].toFixed(2)}"></label>`).join("")}
          <button type="button" class="up-reset" title="Back to the preset's weights">reset</button>
        </form>
        ${checkedLine()}
        <div id="up-list">${upgradeList(open, doll, rows)}</div>
      </aside></div>`;
    out.querySelectorAll(".doll-slot").forEach((b) => b.addEventListener("click", () => { open = open === b.dataset.slot ? "" : b.dataset.slot; draw(); }));
    out.querySelector("[data-clear]")?.addEventListener("click", () => { open = ""; draw(); });
    const wf = $("#up-weights");
    wf.addEventListener("change", () => {
      const edited = {};
      for (const el of wf.querySelectorAll("input")) if (el.value !== "" && +el.value !== +d.preset.weights[el.name].toFixed(2)) edited[el.name] = +el.value; // inputs show 2 decimals
      store.set("upWeights", edited);
      load();
    });
    wf.querySelector(".up-reset").addEventListener("click", () => { store.set("upWeights", {}); load(); });
  };
  const load = async (fresh = false) => {
    f.character = form.character.value.trim(); f.buyOnce = form.buyOnce.checked; store.set("upBuyOnce2", f.buyOnce); f.ahead = form.ahead.checked; f.minGain = form.minGain.value; f.maxPrice = form.maxPrice.value;
    store.set("upChar", f.character); store.set("upAhead", f.ahead); store.set("upMinGain", f.minGain); store.set("upMaxPrice", f.maxPrice);
    const out = $("#up-out");
    out.innerHTML = `<div class="panel"><div class="empty-state">Loading ${esc(f.character)} from the armory and scoring the auction house…</div></div>`;
    // Filters can change while a request is out: only the latest one may draw, and none once the
    // page has been left or re-rendered (renderUpgrades bumps upGen too).
    const gen = ++upGen;
    let res;
    try {
      const wq = Object.entries(store.get("upWeights", {})).map(([k, v]) => `&w_${k}=${encodeURIComponent(v)}`).join("");
      res = await api(`/api/upgrades?character=${encodeURIComponent(f.character)}${fresh ? "&fresh=1" : ""}&buy_once=${f.buyOnce ? 1 : 0}&ahead=${f.ahead ? 1 : 0}&min_gain=${encodeURIComponent(f.minGain)}&max_price=${encodeURIComponent(f.maxPrice)}${wq}`);
    } catch (err) {
      if (gen === upGen && out.isConnected) out.innerHTML = `<div class="panel">${emptyState("Couldn't load that character", esc(err.message))}</div>`;
      return;
    }
    if (gen !== upGen || !out.isConnected) return; // superseded
    d = res;
    draw();
  };
  form.addEventListener("submit", (e) => { e.preventDefault(); load(true); }); // Load: always from the armory
  form.addEventListener("change", (e) => { if (e.target.type === "checkbox") load(); });
  load();
}
async function renderSupply() {
  $("#view").innerHTML = `
    <section class="panel flips-panel">
      <div class="flips-head"><div>
        <h2 class="panel-title">Supply</h2>
        <p class="flips-lede">Durotar Supply and Logistics, priced from the Horde auction house. Crates earn Merchant's Favor for the vendor; writs earn reputation, ideally for no gold.</p>
      </div></div>
      <div id="supply-out" aria-live="polite"><div class="empty-state">Pricing crates and writs…</div></div>
    </section>`;
  let d;
  try { d = await api("/api/supply"); } catch (err) {
    $("#supply-out").innerHTML = emptyState("Can't price supplies yet", esc(err.message));
    return;
  }
  const out = $("#supply-out");
  if (!out) return;
  const items = d.items || {};
  const crates = d.crates || [], writs = d.writs || [];
  const zones = [...new Set(writs.map((w) => w.customer.zone).filter(Boolean))].sort();
  let scope = store.get("supplyScope", "world");
  if (scope !== "world" && !(scope.startsWith("c:") || zones.includes(scope.slice(2)))) scope = "world";

  const crateRows = crates.map((c) => {
    const ranked = c.per_favor > 0;
    const alts = (c.options || []).filter((o) => !(c.best && o.item_id === c.best.item_id && o.qty === c.best.qty));
    return `<tr class="${ranked ? "" : "muted-row"}">
      <td>${supplyItem(items, c.id, c.note ? `<span class="sub-num">${esc(c.note)}</span>` : "")}</td>
      <td data-label="Favor">${c.favor || "?"}</td>
      <td data-label="Cheapest fill">${c.best ? `${count(c.best.qty)} × ${supplyLink(c.best.item_id, items[c.best.item_id]?.name || c.best.name)}<span class="sub-num">${money(c.best.cost)}</span>` : "—"}${
        alts.length ? `<details class="alts"><summary>${alts.length} other fill${alts.length > 1 ? "s" : ""}</summary>${alts.map((o) =>
          `<div>${count(o.qty)} × ${supplyLink(o.item_id, o.name)}:${o.ok ? money(o.cost) : `<span class="none">not enough listed</span>`}</div>`).join("")}</details>` : ""}</td>
      <td data-label="Empty crate">${money(c.crate_cost)}</td>
      <td data-label="Total">${money(c.total)}</td>
      <td data-label="Per Favor" class="${ranked ? "pos" : ""}">${ranked ? money(c.per_favor) : "—"}</td></tr>`;
  }).join("");

  out.innerHTML = `
    <div id="supply-charts"></div>
    <h3 class="supply-h">Crates for Merchant's Favor</h3>
    <p class="fine supply-note">Cheapest first: an empty crate plus its cheapest fill, divided by the Favor the sealed crate pays (raw materials 5/10/15, crafted double; Artisan not known yet).</p>
    <div class="scroll-x"><table class="mini flips supply">
      <thead><tr><th>Crate</th><th>Favor</th><th>Cheapest fill</th><th>Empty crate</th><th>Total</th><th>Gold per Favor</th></tr></thead>
      <tbody>${crateRows}</tbody></table></div>
    <h3 class="supply-h">Writs for reputation</h3>
    <div class="scope-row">
      <div class="risk-seg" role="radiogroup" aria-label="Where">${[["world", "World"], ["c:Kalimdor", "Kalimdor"], ["c:Eastern Kingdoms", "Eastern Kingdoms"]].map(([k, l]) =>
        `<button type="button" role="radio" data-scope="${esc(k)}" aria-checked="${scope === k}">${l}</button>`).join("")}</div>
      <label class="zone-pick">Zone <select id="zone-pick"><option value="">Any</option>${zones.map((z) => `<option value="z:${esc(z)}" ${scope === "z:" + z ? "selected" : ""}>${esc(z)}</option>`).join("")}</select></label>
    </div>
    <div id="zone-sum"></div>
    <div id="writ-table"></div>
    <p class="fine">Each named writ can be handed in once a day. Net = coin pouch (a guide estimate: ${plain(d.pouch.Journeyman)} / ${plain(d.pouch.Expert)} / ${plain(d.pouch.Artisan)} for Journeyman / Expert / Artisan) − the writ − the item it asks for, at Horde prices. Customers come from community guides; some aren't known yet.</p>`;

  const draw = () => {
    store.set("supplyScope", scope);
    out.querySelectorAll("[data-scope]").forEach((b) => b.setAttribute("aria-checked", String(b.dataset.scope === scope)));
    $("#zone-pick").value = scope.startsWith("z:") ? scope : "";
    const shown = writs.filter((w) => inScope(w, scope));
    const byZone = new Map();
    for (const w of shown) {
      if (!w.ok || !w.customer.zone) continue;
      const z = byZone.get(w.customer.zone) || { zone: w.customer.zone, n: 0, rep: 0, net: 0 };
      z.n++; z.rep += w.rep; z.net += w.net;
      byZone.set(z.zone, z);
    }
    const sums = [...byZone.values()].sort((a, b) => (-a.net / (a.rep || 1)) - (-b.net / (b.rep || 1)));
    $("#zone-sum").innerHTML = scope.startsWith("z:") || !sums.length ? "" : `
      <div class="scroll-x"><table class="mini zone-sum"><caption>Doing every buyable writ in a zone, cheapest rep first</caption>
        <thead><tr><th>Zone</th><th>Writs</th><th>Rep</th><th>Net gold</th><th>Gold per rep</th></tr></thead>
        <tbody>${sums.map((z) => `<tr><td><button type="button" class="linkish" data-zone="${esc(z.zone)}">${esc(z.zone)}</button></td>
          <td>${z.n}</td><td>${count(z.rep)}</td><td class="${z.net >= 0 ? "pos" : ""}">${money(z.net, { zero: "0" })}</td><td>${perRep(Math.round(-z.net / z.rep))}</td></tr>`).join("")}</tbody></table></div>`;
    const bench = [];
    if (d.receipt?.ok) bench.push(`<tr class="bench"><td><span class="kind kind-relist">Benchmark</span> Forged Delivery Receipt<span class="sub-num">15 Favor at the best crate's price, plus the cheapest empty crate, for 50 rep. Needs Friendly.</span></td>
      <td data-label="Asks for">—</td><td data-label="Writ">—</td><td data-label="Coin pouch">—</td><td data-label="Net">—</td><td data-label="Rep">50</td><td data-label="Gold per rep">${perRep(Math.round(d.receipt.per_rep))}</td><td data-label="Hand in">The Crossroads camp</td></tr>`);
    for (const w of d.sealed || []) bench.push(`<tr class="bench"><td>${supplyItem(items, w.id, `<span class="sub-num">Opens into a ${esc(w.item)}: valued at the average order</span>`)}</td>
      <td data-label="Asks for">${w.ok ? money(w.item_cost) : "—"}<span class="sub-num">on average</span></td><td data-label="Writ">${money(w.writ_cost)}</td><td data-label="Coin pouch">${money(w.pouch)}</td>
      <td data-label="Net" class="${w.net >= 0 ? "pos" : ""}">${w.ok ? money(w.net, { zero: "0" }) : "—"}</td><td data-label="Rep">${w.rep || "?"}</td><td data-label="Gold per rep">${w.ok ? perRep(w.per_rep) : "—"}</td><td data-label="Hand in">Depends on the order</td></tr>`);
    $("#writ-table").innerHTML = !shown.length ? emptyState("No known writs here", "No writ customers are known in this area yet. Try World.") : `
      <div class="scroll-x"><table class="mini flips supply writs">
        <thead><tr><th>Writ</th><th>Asks for</th><th>Writ</th><th>Coin pouch</th><th>Net</th><th>Rep</th><th>Gold per rep</th><th>Hand in to</th></tr></thead>
        <tbody>${shown.map((w) => {
          const c = w.customer;
          return `<tr class="${w.ok ? "" : "muted-row"}">
            <td>${w.ok && w.net >= 0 ? `<span class="tag tag-new">Free rep</span>` : ""}${supplyItem(items, w.id, `<span class="sub-num">${esc(w.tier || "Tier unknown")}</span>`)}</td>
            <td data-label="Asks for">${w.item_id ? supplyItem(items, w.item_id, `<span class="sub-num">${w.qty > 1 ? `${count(w.qty)} × ` : ""}${w.item_ok ? money(w.item_cost) : "not enough on the AH"}</span>`) : `${esc(w.item)}<span class="sub-num">not seen on the AH</span>`}</td>
            <td data-label="Writ">${w.writ_cost ? money(w.writ_cost) : `<span class="none">not listed</span>`}</td>
            <td data-label="Coin pouch">${money(w.pouch)}</td>
            <td data-label="Net" class="${w.ok && w.net >= 0 ? "pos" : ""}">${w.ok ? money(w.net, { zero: "0" }) : "—"}</td>
            <td data-label="Rep">${w.rep || "?"}</td>
            <td data-label="Gold per rep">${w.ok && w.rep ? perRep(w.per_rep) : "—"}</td>
            <td data-label="Hand in to">${c.zone ? `${esc(c.npc || "Customer not known")}<span class="sub-num">${esc(c.zone)}${c.coords ? ` · ${esc(c.coords)}` : ""}</span>` : `<span class="none">Not known yet</span>`}</td></tr>`;
        }).join("")}${bench.join("")}</tbody></table></div>`;
  };
  out.addEventListener("click", (e) => {
    const b = e.target.closest("[data-scope], [data-zone]");
    if (!b) return;
    scope = b.dataset.scope || `z:${b.dataset.zone}`;
    draw();
  });
  $("#zone-pick").addEventListener("change", (e) => { scope = e.target.value || "world"; draw(); });
  draw();
  supplyCharts(d.history || []);
}

/* supply charts: is now a cheap time for Favor and rep, against the recent history? */
const SUPPLY_RANGES = [["2d", 2], ["7d", 7], ["30d", 30]];
function supplyCharts(history) {
  const el = $("#supply-charts");
  if (!el) return;
  let range = store.get("supplyRange", "7d");
  if (!SUPPLY_RANGES.some(([k]) => k === range)) range = "7d";
  const series = [
    { key: "favor", title: "Gold per Favor", ok: "favor_ok", val: "per_favor", unit: "per Favor",
      what: "The cheapest crate to fill, per Favor" },
    { key: "rep", title: "Gold per rep", ok: "rep_ok", val: "per_rep", unit: "per rep", bench: "receipt_rep",
      what: "The cheapest writ you can buy, per rep. Below zero, its coin pouch pays you" },
  ];
  const draw = () => {
    store.set("supplyRange", range);
    const days = SUPPLY_RANGES.find(([k]) => k === range)[1];
    const since = Date.now() / 1000 - days * 86400;
    const pts = history.filter((p) => p.t >= since);
    el.innerHTML = `
      <div class="supply-charts-head">
        <h3 class="supply-h">Is now a good time?</h3>
        <div class="risk-seg" role="radiogroup" aria-label="Chart range">${SUPPLY_RANGES.map(([k]) =>
          `<button type="button" role="radio" data-range="${k}" aria-checked="${k === range}">${k}</button>`).join("")}</div>
      </div>
      <div class="supply-charts">${series.map((s) => `
        <div class="sc-card">
          <div class="sc-top"><div><div class="sc-title">${s.title}</div><div class="fine">${s.what}.</div></div><div class="sc-now" id="sc-now-${s.key}"></div></div>
          <div class="sc-chart" id="sc-${s.key}"><div class="chart sc-plot"></div><div class="sc-legend"></div></div>
        </div>`).join("")}</div>`;
    for (const s of series) supplyChart(s, pts, range);
  };
  el.addEventListener("click", (e) => {
    const b = e.target.closest("[data-range]");
    if (!b) return;
    range = b.dataset.range;
    draw();
  });
  draw();
}
// Signed short money for axes: "1.2g", "−35c".
const shortSigned = (c) => (c < 0 ? "−" : "") + shortGold(Math.abs(c));
// Where the latest value sits among the range. Scans at the same price count as neither cheaper
// nor dearer: good when 75%+ of the period was dearer, pricey when 75%+ was cheaper.
function supplyVerdict(vals, now) {
  if (vals.length < 6) return { cls: "", text: "Not enough history yet to judge" };
  const dearer = Math.round((vals.filter((v) => v > now).length / vals.length) * 100);
  const cheaper = Math.round((vals.filter((v) => v < now).length / vals.length) * 100);
  if (dearer >= 75) return { cls: "good", text: `Good time: cheaper than ${dearer}% of the period` };
  if (cheaper >= 75) return { cls: "bad", text: `Pricey: dearer than ${cheaper}% of the period` };
  if (dearer === 0 && cheaper === 0) return { cls: "", text: "Typical: the same all period" };
  return { cls: "", text: `Typical: cheaper than ${dearer}%, dearer than ${cheaper}% of the period` };
}
function supplyChart(s, all, range) {
  const el = $(`#sc-${s.key}`), nowEl = $(`#sc-now-${s.key}`);
  if (!el) return;
  const pts = all.filter((p) => p[s.ok]);
  const latest = all[all.length - 1];
  if (latest && latest[s.ok]) {
    const vals = pts.map((p) => p[s.val]);
    const v = supplyVerdict(vals, latest[s.val]);
    nowEl.innerHTML = `<div class="sc-val">${s.key === "rep" ? perRep(latest[s.val]) : money(latest[s.val])}<span class="fine"> ${s.unit} now</span></div>
      <div class="sc-verdict ${v.cls}">${v.text}</div>`;
  } else {
    nowEl.innerHTML = `<div class="sc-verdict">Nothing buyable at the last scan</div>`;
  }
  if (pts.length < 2) { el.innerHTML = emptyState("Not enough history yet", "A point is added after every Horde scan."); return; }

  const plot = el.querySelector(".sc-plot");
  const W = plot.clientWidth, H = plot.clientHeight, padL = 54, padR = 12, padT = 10, padB = 26;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const vals = pts.map((p) => p[s.val]);
  const sorted = [...vals].sort((a, b) => a - b);
  const median = sorted[Math.floor(sorted.length / 2)];
  const benchVals = s.bench ? pts.map((p) => p[s.bench]).filter((v) => v) : [];
  // Scale to the main series' spread; the benchmark is clipped if it's far off, and zero is
  // drawn when the series crosses it.
  let lo = Math.min(...sorted.slice(0, Math.max(1, Math.ceil(sorted.length * 0.02))));
  let hi = sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * 0.98))];
  // The benchmark joins the scale only when it's within one spread of the series; a far-off one
  // would flatten the line that matters, so it's named in the legend instead.
  const benchMed = benchVals.length ? [...benchVals].sort((a, b) => a - b)[Math.floor(benchVals.length / 2)] : 0;
  const mainSpan = Math.max(1, hi - lo);
  const benchOn = benchVals.length > 0 && benchMed <= hi + mainSpan && benchMed >= lo - mainSpan;
  if (benchOn) { lo = Math.min(lo, ...benchVals); hi = Math.max(hi, ...benchVals); }
  if (s.key === "rep" && lo < 0 && hi < 0) hi = 0;
  const span = Math.max(1, hi - lo);
  lo -= span * 0.06; hi += span * 0.08;
  const x = (t) => padL + ((t - t0) / Math.max(1, t1 - t0)) * (W - padL - padR);
  const y = (v) => padT + (1 - (Math.min(hi, Math.max(lo, v)) - lo) / (hi - lo)) * (H - padT - padB);
  const step = (key, ok) => {
    let d = "", pen = false;
    all.forEach((p, i) => {
      if (p.t < t0 || p.t > t1) return;
      if (!(ok ? p[ok] : p[key])) { pen = false; return; }
      d += pen ? ` H${x(p.t).toFixed(1)} V${y(p[key]).toFixed(1)}` : ` M${x(p.t).toFixed(1)} ${y(p[key]).toFixed(1)}`;
      pen = true;
    });
    return d;
  };
  const grid = [], labels = [];
  for (let i = 0; i <= 4; i++) {
    const v = lo + ((hi - lo) * i) / 4;
    grid.push(`<line x1="${padL}" x2="${W - padR}" y1="${y(v)}" y2="${y(v)}"/>`);
    labels.push(`<text x="${padL - 8}" y="${y(v) + 4}" text-anchor="end">${shortSigned(v)}</text>`);
  }
  const tl = [], nT = Math.min(5, pts.length);
  for (let i = 0; i < nT; i++) {
    const t = t0 + ((t1 - t0) * i) / (nT - 1), d = new Date(t * 1000);
    const lbl = range === "2d" ? d.toLocaleTimeString("en-GB", { weekday: "short", hour: "2-digit", minute: "2-digit", timeZone: "Europe/Berlin" })
      : d.toLocaleDateString("en-GB", { day: "numeric", month: "short", timeZone: "Europe/Berlin" });
    tl.push(`<text x="${x(t)}" y="${H - 6}" text-anchor="${i === 0 ? "start" : i === nT - 1 ? "end" : "middle"}">${lbl}</text>`);
  }
  const hasZero = lo < 0 && hi > 0;
  const zero = hasZero ? `<line class="zero" x1="${padL}" x2="${W - padR}" y1="${y(0)}" y2="${y(0)}"/>` : "";
  const zeroLbl = hasZero ? `<text class="zero-lbl" x="${W - padR - 4}" y="${y(0) + 13}" text-anchor="end">below 0: writs pay you</text>` : "";
  const line = step(s.val, s.ok);
  const last = pts[pts.length - 1];
  plot.innerHTML = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(s.title)} history">
    <defs><clipPath id="clip-${s.key}"><rect x="${padL}" y="0" width="${W - padL - padR}" height="${H}"/></clipPath></defs>
    <g class="grid">${grid.join("")}</g><g class="axis">${labels.join("")}${tl.join("")}</g>
    <g clip-path="url(#clip-${s.key})">
      ${zero}
      <line class="median" x1="${padL}" x2="${W - padR}" y1="${y(median)}" y2="${y(median)}"/>
      ${benchOn ? `<path class="bench" d="${step(s.bench)}"/>` : ""}
      <path class="p20" d="${line}"/>
      <circle class="now-dot" cx="${x(last.t)}" cy="${y(last[s.val])}" r="4"/>
    </g>
    ${zeroLbl}
    <text class="median-lbl" x="${padL + 6}" y="${y(median) - 5}">median ${s.key === "rep" ? perRep(median) : plain(Math.round(median))}</text>
    <line class="cross" id="cross-${s.key}" y1="${padT}" y2="${H - padB}" visibility="hidden"/>
  </svg>`;
  el.querySelector(".sc-legend").innerHTML = `${s.bench ? `<span><i class="lg-main"></i>Cheapest writ</span><span><i class="lg-bench"></i>Forged Delivery Receipt${benchOn ? "" : benchMed ? `: ${perRep(benchMed)} per rep, above the chart` : ": not priced"}</span><span><i class="lg-median"></i>Median</span>`
    : `<span><i class="lg-main"></i>Cheapest crate</span><span><i class="lg-median"></i>Median</span>`}`;
  const svg = plot.querySelector("svg"), cross = $(`#cross-${s.key}`);
  const fmt = (v) => s.key === "rep" ? perRep(v) : money(v);
  svg.addEventListener("mousemove", (e) => {
    const r = svg.getBoundingClientRect();
    const mx = ((e.clientX - r.left) / r.width) * W;
    let best = pts[0];
    for (const p of pts) if (Math.abs(x(p.t) - mx) < Math.abs(x(best.t) - mx)) best = p;
    cross.setAttribute("x1", x(best.t)); cross.setAttribute("x2", x(best.t)); cross.setAttribute("visibility", "visible");
    showTip(`<b>${berlin.format(new Date(best.t * 1000))}</b>
      <div class="row"><span>${s.key === "rep" ? "Cheapest writ" : "Cheapest crate"}</span>${fmt(best[s.val])}</div>
      ${s.bench && best[s.bench] ? `<div class="row"><span>Delivery Receipt</span>${fmt(best[s.bench])}</div>` : ""}
      ${s.key === "rep" ? `<div class="row"><span>Free-rep writs</span>${count(best.free_writs)}</div>` : ""}`, e);
  });
  svg.addEventListener("mouseleave", () => { cross.setAttribute("visibility", "hidden"); hideTip(); });
}

/* router */
function route() {
  hideTip();
  const h = location.hash || "#/";
  const [path, qs] = h.slice(1).split("?");
  const params = new URLSearchParams(qs || "");
  const m = path.match(/^\/item\/(\d+)/);
  const section = path.startsWith("/upgrades") ? "upgrades" : path.startsWith("/status") ? "status" : path.startsWith("/supply") ? "supply" : (m || path.startsWith("/browse")) ? "browse" : "advisor";
  document.querySelectorAll("[data-nav]").forEach((a) => a.classList.toggle("active", a.dataset.nav === section));
  $("#hero").classList.toggle("compact", section !== "browse" || !!m);
  $("#layout").classList.toggle("full", section !== "browse");
  if (section !== "advisor") stopLive();
  if (m) return renderItem(m[1], params.get("variant") || "");
  if (section === "status") return renderStatus();
  if (section === "supply") return renderSupply();
  if (section === "upgrades") return renderUpgrades();
  if (section === "advisor") return renderFlips();
  refreshList();
}

window.addEventListener("hashchange", route);
renderQuality();
loadHouses().then(route).catch((err) => {
  $("#view").innerHTML = `<div class="panel">${emptyState("Can't reach the collector", esc(err.message))}</div>`;
});
setInterval(() => loadHouses().catch(() => {}), 5 * 60 * 1000);
