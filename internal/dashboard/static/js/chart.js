import { $, svg } from "./dom.js";
import { fmtByteRate, fmtRate, fmtTime } from "./format.js";

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
export function lineChart(el, { series, step, end, span, ticks, bytes }) {
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
