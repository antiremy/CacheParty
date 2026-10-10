"use strict";

// --- formatting ----------------------------------------------------------

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
const RATE_UNITS = ["bit/s", "kbit/s", "Mbit/s", "Gbit/s", "Tbit/s"];

function fmtBytes(n) {
  let i = 0;
  while (Math.abs(n) >= 1024 && i < BYTE_UNITS.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n.toFixed(0) : n.toFixed(n < 10 ? 2 : n < 100 ? 1 : 0)) + " " + BYTE_UNITS[i];
}

// Rates are shown in bits, as network links are rated.
function fmtRate(bytesPerSec) {
  let n = bytesPerSec * 8, i = 0;
  while (n >= 1000 && i < RATE_UNITS.length - 1) { n /= 1000; i++; }
  return (n === 0 ? "0" : n.toFixed(n < 10 ? 1 : 0)) + " " + RATE_UNITS[i];
}

const BYTE_RATE_UNITS = ["B/s", "kB/s", "MB/s", "GB/s", "TB/s"];

// Disk rates are shown in bytes, as disks are rated.
function fmtByteRate(bytesPerSec) {
  let n = bytesPerSec, i = 0;
  while (n >= 1000 && i < BYTE_RATE_UNITS.length - 1) { n /= 1000; i++; }
  return (n === 0 ? "0" : n.toFixed(n < 10 ? 1 : 0)) + " " + BYTE_RATE_UNITS[i];
}

function fmtCount(n) { return n.toLocaleString(); }

function fmtDuration(sec) {
  sec = Math.floor(sec);
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m ${sec % 60}s`;
}

function fmtTime(ms, withSeconds) {
  return new Date(ms).toLocaleTimeString([], withSeconds
    ? { hour: "2-digit", minute: "2-digit", second: "2-digit" }
    : { hour: "2-digit", minute: "2-digit" });
}

const $ = (id) => document.getElementById(id);
const SVG = "http://www.w3.org/2000/svg";
function svg(tag, attrs, parent) {
  const el = document.createElementNS(SVG, tag);
  for (const k in attrs) el.setAttribute(k, attrs[k]);
  if (parent) parent.appendChild(el);
  return el;
}
function cell(tr, text, cls) {
  const td = document.createElement("td");
  td.textContent = text;
  if (cls) td.className = cls;
  tr.appendChild(td);
  return td;
}

// --- line chart ----------------------------------------------------------

// A "nice" upper bound and tick step for rates, so the axis reads in round
// values of the displayed unit: perByte is 8 for bit/s, 1 for B/s.
function niceScale(maxBytesPerSec, ticks, perByte) {
  const max = Math.max(maxBytesPerSec, 1000) * perByte;
  const raw = max / ticks;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw);
  return { top: Math.ceil(max / step) * step / perByte, step: step / perByte };
}

// lineChart draws series (each {name, values, color}) whose points are step
// seconds apart and end at end (ms), over a window of span seconds. Rates
// are in bits unless bytes is set.
function lineChart(el, { series, step, end, span, ticks, bytes }) {
  const fmt = bytes ? fmtByteRate : fmtRate;
  const w = el.clientWidth, h = el.clientHeight;
  if (!w || !h) return;
  const pad = { l: 64, r: 12, t: 8, b: 22 };
  const pw = w - pad.l - pad.r, ph = h - pad.t - pad.b;
  const start = end - span * 1000;
  const n = Math.max(...series.map((s) => s.values.length));
  const tAt = (i) => end - (n - 1 - i) * step * 1000;
  const x = (t) => pad.l + (t - start) / (span * 1000) * pw;
  const peak = Math.max(0, ...series.flatMap((s) => s.values));
  const scale = niceScale(peak, ticks, bytes ? 1 : 8);
  const y = (v) => pad.t + ph - v / scale.top * ph;

  el.textContent = "";
  const root = svg("svg", { role: "img", "aria-label": series.map((s) => s.name).join(" and ") + " over time" }, el);
  const grid = svg("g", { class: "grid" }, root);
  for (let v = 0; v <= scale.top + 1e-9; v += scale.step) {
    if (v > 0) svg("line", { x1: pad.l, x2: w - pad.r, y1: y(v), y2: y(v) }, grid);
    const label = svg("text", { x: pad.l - 8, y: y(v) + 4, "text-anchor": "end" }, root);
    label.textContent = fmt(v);
  }
  svg("line", { class: "baseline", x1: pad.l, x2: w - pad.r, y1: pad.t + ph, y2: pad.t + ph }, root);

  // Time ticks on round minutes (or hours for long spans).
  const tickEvery = (span <= 900 ? 180 : span <= 3600 ? 600 : 4 * 3600) * 1000;
  const tz = new Date().getTimezoneOffset() * 60000;
  for (let t = Math.ceil((start - tz) / tickEvery) * tickEvery + tz; t <= end; t += tickEvery) {
    if (x(t) < pad.l + 20 || x(t) > w - pad.r - 20) continue;
    const label = svg("text", { x: x(t), y: h - 4, "text-anchor": "middle" }, root);
    label.textContent = fmtTime(t, false);
  }

  for (const s of series) {
    if (!s.values.length) continue;
    let d = "";
    s.values.forEach((v, i) => { d += (i ? "L" : "M") + x(tAt(i)).toFixed(1) + "," + y(v).toFixed(1); });
    const first = x(tAt(0)).toFixed(1), last = x(tAt(s.values.length - 1)).toFixed(1);
    svg("path", { d: d + `L${last},${pad.t + ph}L${first},${pad.t + ph}Z`, fill: s.wash, stroke: "none" }, root);
    svg("path", { class: "line", d, stroke: s.color }, root);
  }

  // Hover layer: a crosshair snapped to the nearest point, one tooltip
  // listing every series.
  const cross = svg("line", { class: "cross", y1: pad.t, y2: pad.t + ph, visibility: "hidden" }, root);
  const dots = series.map((s) => svg("circle", { class: "dot", r: 4, fill: s.color, visibility: "hidden" }, root));
  const hit = svg("rect", { x: pad.l, y: pad.t, width: pw, height: ph, fill: "transparent", tabindex: 0 }, root);
  const tip = $("tooltip");
  const show = (i, clientX, clientY) => {
    if (i < 0 || i >= n) return hide();
    const t = tAt(i), cx = x(t);
    cross.setAttribute("x1", cx); cross.setAttribute("x2", cx); cross.setAttribute("visibility", "visible");
    tip.textContent = "";
    const time = document.createElement("div");
    time.className = "tt-time";
    time.textContent = fmtTime(t, step < 60);
    tip.appendChild(time);
    series.forEach((s, k) => {
      const v = s.values[i - (n - s.values.length)];
      dots[k].setAttribute("visibility", v === undefined ? "hidden" : "visible");
      if (v === undefined) return;
      dots[k].setAttribute("cx", cx); dots[k].setAttribute("cy", y(v));
      const row = document.createElement("div");
      row.className = "tt-row";
      const key = document.createElement("span");
      key.className = "key"; key.style.background = s.color;
      const val = document.createElement("b"); val.textContent = fmt(v);
      const name = document.createElement("span"); name.textContent = s.name;
      row.append(key, val, name);
      tip.appendChild(row);
    });
    tip.hidden = false;
    const r = tip.getBoundingClientRect();
    tip.style.left = Math.min(clientX + 14, window.innerWidth - r.width - 8) + "px";
    tip.style.top = Math.max(8, clientY - r.height - 12) + "px";
    el.dataset.hover = i;
  };
  const hide = () => {
    cross.setAttribute("visibility", "hidden");
    dots.forEach((d) => d.setAttribute("visibility", "hidden"));
    tip.hidden = true;
    delete el.dataset.hover;
  };
  const indexAt = (clientX) => {
    const box = root.getBoundingClientRect();
    const t = start + (clientX - box.left - pad.l) / pw * span * 1000;
    return Math.round(n - 1 - (end - t) / (step * 1000));
  };
  hit.addEventListener("pointermove", (e) => show(indexAt(e.clientX), e.clientX, e.clientY));
  hit.addEventListener("pointerleave", hide);
  hit.addEventListener("blur", hide);
  hit.addEventListener("focus", () => {
    const b = hit.getBoundingClientRect();
    show(n - 1, b.right - 40, b.top + 20);
  });
}

// --- state and polling ----------------------------------------------------

const RANGES = { "15m": 900, "24h": 86400 };
let range = "15m";
try { range = localStorage.getItem("cacheparty.range") in RANGES ? localStorage.getItem("cacheparty.range") : range; } catch (_) {}
let last = null;

function colors() {
  const cs = getComputedStyle(document.documentElement);
  const v = (name) => cs.getPropertyValue(name).trim();
  return { s1: v("--series-1"), s2: v("--series-2"), w1: v("--series-1-wash"), w2: v("--series-2-wash") };
}

function sum(a) { return a.reduce((x, y) => x + y, 0); }
function tail(a, k) { return a.slice(Math.max(0, a.length - k)); }
function avg(a) { return a.length ? sum(a) / a.length : 0; }

function render(s) {
  last = s;
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

// trafficCells adds the Now, Served, Downloaded and From cache columns.
function trafficCells(tr, x) {
  cell(tr, fmtRate(x.sentRate), "num");
  cell(tr, fmtBytes(x.sent), "num");
  cell(tr, fmtBytes(x.fetched), "num");
  const pct = x.sent > 0 ? Math.max(0, Math.min(100, Math.round(100 * (1 - x.fetched / x.sent)))) : null;
  const td = cell(tr, "");
  const wrap = document.createElement("div"); wrap.className = "ratio";
  const bar = document.createElement("div"); bar.className = "bar";
  const fill = document.createElement("span"); fill.style.width = (pct ?? 0) + "%";
  bar.appendChild(fill);
  const label = document.createElement("span"); label.className = "pct";
  label.textContent = pct === null ? "—" : pct + "%";
  wrap.append(bar, label);
  td.appendChild(wrap);
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

// --- per-service downloads -------------------------------------------------

const KIND_LABELS = { depot: "Depot", product: "Product", app: "App", host: "Host" };
let service = null; // the service whose downloads are shown, or null
try { service = new URLSearchParams(location.hash.slice(1)).get("service"); } catch (_) {}

function selectService(name, focus = true) {
  service = name;
  history.replaceState(null, "", name ? "#service=" + encodeURIComponent(name) : location.pathname + location.search);
  $("services-view").hidden = !!name;
  $("apps-view").hidden = !name;
  $("apps-title").hidden = !name;
  $("apps-back").disabled = !name;
  $("apps-service").textContent = name || "";
  closePurge();
  if (name) {
    $("apps").textContent = "";
    $("apps-empty").hidden = true;
    pollApps();
    if (focus) $("apps-back").focus();
  }
}

function fmtAgo(ms, now) {
  if (!ms) return "—";
  const s = Math.max(0, (now - ms) / 1000);
  if (s < 5) return "now";
  return (s < 60 ? Math.floor(s) + "s" : fmtDuration(s)) + " ago";
}

function renderApps(data) {
  if (data.service !== service) return; // a reply for an earlier selection
  const body = $("apps");
  body.textContent = "";
  $("apps-empty").hidden = data.apps.length > 0;
  // One kind gets the column header; a mix is labelled row by row.
  const kinds = new Set(data.apps.map((a) => a.kind).filter((k) => k !== "other"));
  const kind = kinds.size === 1 ? [...kinds][0] : null;
  $("apps-kind").textContent = KIND_LABELS[kind] || "Download";
  const now = last ? last.end : Date.now();
  // Removing a game needs the password; "other" rows are not one game.
  const purgeable = (a) => settings && settings.editable && a.kind !== "other" && data.service !== "other";
  $("apps-actions").hidden = !data.apps.some(purgeable);
  for (const a of data.apps) {
    const tr = document.createElement("tr");
    const td = cell(tr, "");
    if (a.kind === "depot") {
      // SteamDB names the game a depot belongs to.
      const link = document.createElement("a");
      link.href = `https://steamdb.info/depot/${encodeURIComponent(a.id)}/`;
      link.target = "_blank"; link.rel = "noopener noreferrer";
      link.title = "Look up this depot on SteamDB";
      link.textContent = a.id;
      td.appendChild(link);
    } else {
      td.textContent = a.kind === "other" ? "Others (limit reached)" : a.id;
    }
    if (!kind && a.kind !== "other") {
      const badge = document.createElement("span");
      badge.className = "badge"; badge.textContent = (KIND_LABELS[a.kind] || a.kind).toLowerCase();
      td.appendChild(badge);
    }
    trafficCells(tr, a);
    cell(tr, fmtAgo(a.lastActive, now), "num");
    if (!$("apps-actions").hidden) {
      const td = cell(tr, "", "num");
      if (purgeable(a)) {
        const btn = document.createElement("button");
        btn.type = "button"; btn.className = "link"; btn.textContent = "Purge";
        btn.title = `Remove everything cached for ${appLabel(a)}`;
        btn.addEventListener("click", () => openPurge(a));
        td.appendChild(btn);
      }
    }
    body.appendChild(tr);
  }
}

function appLabel(a) {
  return a.kind === "host" ? a.id : `${(KIND_LABELS[a.kind] || a.kind).toLowerCase()} ${a.id}`;
}

// --- purge ------------------------------------------------------------------

// Purging asks the cache what it holds for the game first, and removes it
// only once that has been shown and confirmed.
let purging = null; // {service, kind, id} being purged, or null

function purgeStatus(msg, cls) {
  $("purge-status").textContent = msg;
  $("purge-status").className = "small " + (cls || "");
}

function openPurge(a) {
  purging = { service, kind: a.kind, id: a.id };
  $("purge").hidden = false;
  $("purge-title").textContent = `Remove ${appLabel(a)} from the cache`;
  $("purge-cancel").textContent = "Cancel";
  $("purge-go").hidden = true;
  try { if (!$("purge-password").value) $("purge-password").value = sessionStorage.getItem("cacheparty.pw") || ""; } catch (_) {}
  if ($("purge-password").value) {
    purgeRequest(false);
  } else {
    purgeStatus("Enter the dashboard password to see what is cached.");
    $("purge-password").focus();
  }
}

function closePurge() {
  purging = null;
  $("purge").hidden = true;
}

async function purgeRequest(confirm) {
  const pw = $("purge-password").value;
  if (!pw) { purgeStatus("Enter the dashboard password.", "err"); $("purge-password").focus(); return; }
  const target = purging;
  $("purge-check").disabled = $("purge-go").disabled = true;
  purgeStatus(confirm ? "Removing…" : "Counting cached chunks…");
  try {
    const res = await fetch("api/cache/purge", {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + pw },
      body: JSON.stringify({ ...target, confirm }),
    });
    const data = await res.json();
    if (purging !== target) return; // closed or switched meanwhile
    if (!res.ok) { purgeStatus(data.error || `Error ${res.status}`, "err"); $("purge-go").hidden = true; return; }
    try { sessionStorage.setItem("cacheparty.pw", pw); } catch (_) {}
    $("purge-go").hidden = true;
    if (confirm) {
      purgeStatus(`Removed ${fmtCount(data.chunks)} chunks and freed ${fmtBytes(data.bytes)}.`, "ok");
      $("purge-cancel").textContent = "Close";
    } else if (!data.chunks) {
      purgeStatus("Nothing of it is cached.");
    } else {
      purgeStatus(`${fmtCount(data.chunks)} chunks (${fmtBytes(data.bytes)}) in ${fmtCount(data.objects)} files will be removed. ` +
        "Anyone downloading it now gets the rest from the internet.");
      $("purge-go").textContent = `Remove ${fmtBytes(data.bytes)}`;
      $("purge-go").hidden = false;
      $("purge-go").focus();
    }
  } catch (_) {
    purgeStatus("Could not reach the cache.", "err");
  } finally {
    $("purge-check").disabled = $("purge-go").disabled = false;
  }
}

$("purge").addEventListener("submit", (e) => { e.preventDefault(); purgeRequest(false); });
$("purge-go").addEventListener("click", () => purgeRequest(true));
$("purge-cancel").addEventListener("click", closePurge);

async function pollApps() {
  if (!service) return;
  try {
    const res = await fetch(`api/services/${encodeURIComponent(service)}`, { cache: "no-store" });
    if (res.ok) renderApps(await res.json());
  } catch (_) {}
}

$("apps-back").addEventListener("click", () => selectService(null));

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
async function poll() {
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

document.querySelectorAll(".seg button").forEach((b) => b.addEventListener("click", () => {
  range = b.dataset.range;
  try { localStorage.setItem("cacheparty.range", range); } catch (_) {}
  document.querySelectorAll(".seg button").forEach((o) => o.setAttribute("aria-pressed", o === b));
  for (const id of ["chart-main", "chart-sni", "chart-disk"]) $(id).classList.add("loading");
  poll();
}));
document.querySelectorAll(".seg button").forEach((o) => o.setAttribute("aria-pressed", o.dataset.range === range));

let resizeFrame = 0;
window.addEventListener("resize", () => {
  cancelAnimationFrame(resizeFrame);
  resizeFrame = requestAnimationFrame(() => last && render(last));
});
window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => last && render(last));

// --- card layout ----------------------------------------------------------

// The smaller cards are packed into as many columns as fit, each card going to
// the shortest column so far. A card stays in its column unless moving it
// evens things out by more than CARD_STICKY, so cards don't hop as tables grow.
const CARD_MIN_WIDTH = 380;
const CARD_STICKY = 48;
const cardBox = $("cards");
const cards = [...cardBox.children];
let cardCols = [];

function layoutCards() {
  const gap = parseFloat(getComputedStyle(cardBox).rowGap) || 0;
  const n = Math.max(1, Math.floor((cardBox.clientWidth + gap) / (CARD_MIN_WIDTH + gap)));
  if (cardCols.length !== n) {
    cardCols = Array.from({ length: n }, () => Object.assign(document.createElement("div"), { className: "masonry-col" }));
    cardBox.style.gridTemplateColumns = `repeat(${n}, minmax(0, 1fr))`;
  }
  const heights = cardCols.map(() => 0);
  const want = cardCols.map(() => []);
  for (const card of cards) {
    let col = heights.indexOf(Math.min(...heights));
    const cur = cardCols.indexOf(card.parentNode);
    if (cur >= 0 && heights[cur] - heights[col] <= CARD_STICKY) col = cur;
    want[col].push(card);
    if (!card.hidden) heights[col] += card.offsetHeight + gap;
  }
  // Moving a card blurs whatever was focused inside it, such as a link.
  const focused = document.activeElement;
  cardCols.forEach((col, i) => {
    if (want[i].length !== col.children.length || want[i].some((c, j) => col.children[j] !== c)) col.replaceChildren(...want[i]);
  });
  if (cardCols.some((c) => c.parentNode !== cardBox)) cardBox.replaceChildren(...cardCols);
  if (focused && focused !== document.activeElement) focused.focus({ preventScroll: true });
}

let cardFrame = 0;
const cardObserver = new ResizeObserver(() => {
  cancelAnimationFrame(cardFrame);
  cardFrame = requestAnimationFrame(layoutCards);
});
cardObserver.observe(cardBox);
cards.forEach((c) => cardObserver.observe(c));
layoutCards();

// --- settings -------------------------------------------------------------

let settings = null;

function renderSettings(data) {
  settings = data;
  const live = $("live-settings");
  live.textContent = "";
  for (const s of data.settings.filter((s) => s.live)) {
    const wrap = document.createElement("div");
    wrap.className = "setting";
    const label = document.createElement("label");
    label.htmlFor = "set-" + s.key;
    label.textContent = s.label;
    const v = document.createElement("div");
    v.className = "var";
    v.textContent = s.key;
    const row = document.createElement("div");
    row.className = "row";
    const input = document.createElement("input");
    input.id = "set-" + s.key;
    input.value = s.value;
    input.dataset.key = s.key;
    input.dataset.orig = s.value;
    input.disabled = !data.editable;
    input.addEventListener("input", () => input.classList.toggle("changed", input.value.trim() !== input.dataset.orig));
    const badge = document.createElement("span");
    badge.className = "badge";
    badge.textContent = { dashboard: "set here", env: "from env", default: "default" }[s.source];
    row.append(input, badge);
    if (s.source === "dashboard" && data.editable) {
      const reset = document.createElement("button");
      reset.type = "button";
      reset.className = "link";
      reset.textContent = "Reset";
      reset.title = "Remove the value set here and go back to the environment or default";
      reset.addEventListener("click", () => save({ [s.key]: null }));
      row.appendChild(reset);
    }
    const help = document.createElement("div");
    help.className = "help";
    help.textContent = s.help;
    wrap.append(label, v, row, help);
    live.appendChild(wrap);
  }
  $("save-row").hidden = !data.editable;
  $("settings-note").textContent = data.editable
    ? "Changes apply immediately and are saved, taking precedence over environment variables. Eviction picks them up within a minute."
    : "Read-only. Set the DASHBOARD_PASSWORD environment variable to change settings from here.";

  const body = $("fixed-settings").tBodies[0];
  body.textContent = "";
  for (const s of data.settings.filter((s) => !s.live)) {
    const tr = document.createElement("tr");
    cell(tr, s.label);
    cell(tr, s.key).className = "var";
    cell(tr, s.value === "" ? "(off)" : s.value);
    cell(tr, s.source === "env" ? "env" : "default");
    body.appendChild(tr);
  }
}

function status(msg, cls) {
  $("save-status").textContent = msg;
  $("save-status").className = "small " + (cls || "");
}

async function save(values) {
  const pw = $("password").value;
  if (!pw) { status("Enter the dashboard password.", "err"); $("password").focus(); return; }
  $("save").disabled = true;
  status("Saving…");
  try {
    const res = await fetch("api/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + pw },
      body: JSON.stringify({ values }),
    });
    const data = await res.json();
    if (!res.ok) { status(data.error || `Error ${res.status}`, "err"); return; }
    try { sessionStorage.setItem("cacheparty.pw", pw); } catch (_) {}
    renderSettings(data);
    status("Saved.", "ok");
  } catch (e) {
    status("Could not reach the cache.", "err");
  } finally {
    $("save").disabled = false;
  }
}

$("settings-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const values = {};
  document.querySelectorAll("#live-settings input").forEach((i) => {
    if (i.value.trim() !== i.dataset.orig) values[i.dataset.key] = i.value.trim();
  });
  if (!Object.keys(values).length) { status("Nothing changed."); return; }
  save(values);
});

try { $("password").value = sessionStorage.getItem("cacheparty.pw") || ""; } catch (_) {}

fetch("api/settings", { cache: "no-store" })
  .then((r) => r.json())
  .then(renderSettings)
  .catch(() => { $("settings-note").textContent = "Could not load settings."; });

selectService(service, false);
poll();
