import { pollApps, selectService } from "./apps.js";
import { lineChart } from "./chart.js";
import { $, cell, trafficCells } from "./dom.js";
import { fmtByteRate, fmtBytes, fmtCount, fmtDuration, fmtRate, fmtTime } from "./format.js";
import { state } from "./state.js";

const RANGES = { "15m": 900, "24h": 86400 };
let range = "15m";
try { range = localStorage.getItem("cacheparty.range") in RANGES ? localStorage.getItem("cacheparty.range") : range; } catch (_) {}

function colors() {
  const cs = getComputedStyle(document.documentElement);
  const v = (name) => cs.getPropertyValue(name).trim();
  return { s1: v("--series-1"), s2: v("--series-2"), w1: v("--series-1-wash"), w2: v("--series-2-wash") };
}

function sum(a) { return a.reduce((x, y) => x + y, 0); }
function tail(a, k) { return a.slice(Math.max(0, a.length - k)); }
function avg(a) { return a.length ? sum(a) / a.length : 0; }

function render(s) {
  state.last = s;
  $("hostname").textContent = s.hostname;
  $("version").textContent = s.version;
  $("uptime").textContent = fmtDuration(s.uptime);
  // Tiles: "now" is the last 5 s, or the last point in the 24 h view.
  const k = Math.max(1, Math.round(5 / s.step));
  document.title = `CacheParty · ${fmtRate(avg(tail(s.sent, k)))}`;
  $("t-sent").textContent = fmtRate(avg(tail(s.sent, k)));
  $("t-fetched").textContent = fmtRate(avg(tail(s.fetched, k)));
  $("t-active").textContent = fmtCount(s.active);
  const sent = sum(s.sent), fetched = sum(s.fetched);
  // Read-ahead can fetch a little more than was sent; never show < 0%.
  $("t-ratio").textContent = sent > 0 ? Math.max(0, Math.round(100 * (1 - fetched / sent))) + "%" : "—";
  $("t-ratio-note").textContent = sent > 0 ? `${fmtBytes(sent * s.step - fetched * s.step)} saved, this range` : "nothing sent in this range";

  const c = colors();
  lineChart($("chart-main"), {
    series: [
      { name: "To clients", values: s.sent, color: c.s1, wash: c.w1 },
      { name: "From the internet", values: s.fetched, color: c.s2, wash: c.w2 },
    ],
    step: s.step, end: s.end, span: RANGES[range], ticks: 4,
  });
  if (s.sniEnabled) lineChart($("chart-sni"), {
    series: [{ name: "HTTPS passthrough", values: s.sni, color: c.s1, wash: c.w1 }],
    step: s.step, end: s.end, span: RANGES[range], ticks: 2,
  });
  lineChart($("chart-disk"), {
    series: [
      { name: "Read", values: s.diskRead, color: c.s1, wash: c.w1 },
      { name: "Written", values: s.diskWrite, color: c.s2, wash: c.w2 },
    ],
    step: s.step, end: s.end, span: RANGES[range], ticks: 3, bytes: true,
  });
  $("d-read").textContent = fmtByteRate(avg(tail(s.diskRead, k)));
  $("d-write").textContent = fmtByteRate(avg(tail(s.diskWrite, k)));
  $("d-note").textContent = `Chunk data read to serve hits and written while filling the cache. ` +
    `${fmtBytes(s.totals.diskRead)} read, ${fmtBytes(s.totals.diskWritten)} written since start.`;
  for (const id of ["chart-main", "chart-sni", "chart-disk"]) $(id).classList.remove("loading");
  $("sni-card").hidden = !s.sniEnabled;
  $("https-card").hidden = !s.sniEnabled;
  // Per-service, client and upstream totals outlive a restart only when saved.
  for (const el of document.querySelectorAll(".totals-span")) el.textContent = s.saved ? "all time" : "since start";
  $("sni-total").textContent = `${fmtBytes(s.totals.sniSent + s.totals.sniReceived)} since start`;
  renderTable(s);
  renderServices(s.services);
  renderPeers("clients", s.clients);
  renderPeers("upstreams", s.upstreams);
  renderHTTPS(s.https);
  renderStorage(s.cache, s.totals);
  renderAddrs($("n-ipv4"), s.ipv4);
  renderAddrs($("n-ipv6"), s.ipv6);

  const r = s.totals.requests || {};
  $("r-hit").textContent = fmtCount(r.HIT || 0);
  $("r-miss").textContent = fmtCount(r.MISS || 0);
  $("r-bypass").textContent = fmtCount(r.BYPASS || 0);
  $("r-coalesced").textContent = fmtCount(s.totals.coalesced);
  $("r-up").textContent = fmtCount(s.totals.upstreamRequests);
  $("r-uperr").textContent = fmtCount(s.totals.upstreamErrors);
}

function renderTable(s) {
  // The newest points first; the table view is for reading exact values,
  // so it shows the last 60 rather than every second of the range.
  const body = $("table-main").tBodies[0];
  body.textContent = "";
  const n = s.sent.length;
  for (let i = n - 1; i >= Math.max(0, n - 60); i--) {
    const tr = document.createElement("tr");
    cell(tr, fmtTime(s.end - (n - 1 - i) * s.step * 1000, s.step < 60));
    cell(tr, fmtRate(s.sent[i]), "num");
    cell(tr, fmtRate(s.fetched[i]), "num");
    body.appendChild(tr);
  }
}

function renderServices(list) {
  const body = $("services");
  body.textContent = "";
  $("services-empty").hidden = list.length > 0;
  $("services-hint").hidden = list.length === 0;
  for (const sv of list) {
    const tr = document.createElement("tr");
    const btn = document.createElement("button");
    btn.type = "button"; btn.className = "svc-link"; btn.textContent = sv.name;
    btn.title = `Show downloads through ${sv.name}`;
    btn.addEventListener("click", () => selectService(sv.name));
    cell(tr, "").appendChild(btn);
    trafficCells(tr, sv);
    body.appendChild(tr);
  }
}

// renderPeers lists the clients or upstream domains, busiest first.
function renderPeers(id, list) {
  const clients = id === "clients";
  const body = $(id);
  body.textContent = "";
  $(id + "-empty").hidden = list.length > 0;
  for (const p of list) {
    const tr = document.createElement("tr");
    cell(tr, p.name === "other" ? "Others (limit reached)" : p.name, "peer");
    if (clients) { trafficCells(tr, p); body.appendChild(tr); continue; }
    cell(tr, fmtRate(p.rate), "num");
    cell(tr, fmtBytes(p.bytes), "num");
    body.appendChild(tr);
  }
}

// renderHTTPS lists the server names relayed by the HTTPS passthrough,
// busiest first. Their traffic bypasses the cache entirely.
function renderHTTPS(list) {
  const body = $("https");
  body.textContent = "";
  $("https-empty").hidden = list.length > 0;
  for (const d of list) {
    const tr = document.createElement("tr");
    cell(tr, d.name === "other" ? "Others (limit reached)" : d.name, "peer");
    cell(tr, fmtRate(d.rate), "num");
    cell(tr, fmtBytes(d.sent), "num");
    cell(tr, fmtBytes(d.received), "num");
    body.appendChild(tr);
  }
}

function renderStorage(c, totals) {
  const pct = Math.min(100, 100 * c.bytes / c.limit);
  $("s-cache").textContent = fmtBytes(c.bytes);
  $("s-limit").textContent = fmtBytes(c.limit);
  $("s-pct").textContent = pct.toFixed(pct < 10 ? 1 : 0) + "% full";
  $("m-cache").style.width = pct + "%";
  $("cache-meter").setAttribute("aria-label", `Cache ${fmtBytes(c.bytes)} of ${fmtBytes(c.limit)} limit`);
  $("s-chunks").textContent = fmtCount(c.chunks);
  $("s-free").textContent = c.diskTotal ? fmtBytes(c.diskFree) : "—";
  $("s-evicted").textContent = fmtBytes(totals.evictedBytes);
  const known = c.diskTotal > 0;
  $("vol-line").hidden = $("vol-meter").hidden = !known;
  if (known) {
    const used = c.diskTotal - c.diskFree, vpct = Math.min(100, 100 * used / c.diskTotal);
    $("s-vol-used").textContent = fmtBytes(used);
    $("s-vol-total").textContent = fmtBytes(c.diskTotal);
    $("m-vol").style.width = vpct + "%";
    $("vol-meter").setAttribute("aria-label", `Volume ${fmtBytes(used)} of ${fmtBytes(c.diskTotal)} used (${vpct.toFixed(0)}%)`);
  }
}

function renderAddrs(dd, list) {
  dd.textContent = "";
  if (!list.length) { dd.textContent = "none"; dd.className = "muted"; return; }
  dd.className = "";
  for (const a of list) {
    const row = document.createElement("div");
    const ip = document.createElement("span"); ip.className = "ip"; ip.textContent = a.address;
    const ifc = document.createElement("span"); ifc.className = "muted small"; ifc.textContent = a.interface;
    row.append(ip, " ", ifc);
    dd.appendChild(row);
  }
}

let timer = null;
export async function poll() {
  clearTimeout(timer);
  try {
    const res = await fetch(`api/stats?range=${range}`, { cache: "no-store" });
    if (!res.ok) throw new Error(res.status);
    render(await res.json());
    await pollApps();
    $("conn").textContent = "live";
    $("conn").className = "conn ok";
  } catch (e) {
    $("conn").textContent = "disconnected";
    $("conn").className = "conn bad";
  }
  timer = setTimeout(poll, range === "15m" ? 2000 : 15000);
}

export function initStats() {
  document.querySelectorAll(".seg button").forEach((b) => b.addEventListener("click", () => {
    range = b.dataset.range;
    try { localStorage.setItem("cacheparty.range", range); } catch (_) {}
    document.querySelectorAll(".seg button").forEach((o) => o.setAttribute("aria-pressed", o === b));
    for (const id of ["chart-main", "chart-sni", "chart-disk"]) $(id).classList.add("loading");
    poll();
  }));
  document.querySelectorAll(".seg button").forEach((o) => o.setAttribute("aria-pressed", o.dataset.range === range));

  let frame = 0;
  window.addEventListener("resize", () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => state.last && render(state.last));
  });
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => state.last && render(state.last));
}
