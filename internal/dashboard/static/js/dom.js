import { fmtBytes, fmtRate } from "./format.js";

export const $ = (id) => document.getElementById(id);

const SVG = "http://www.w3.org/2000/svg";

export function svg(tag, attrs, parent) {
  const el = document.createElementNS(SVG, tag);
  for (const k in attrs) el.setAttribute(k, attrs[k]);
  if (parent) parent.appendChild(el);
  return el;
}

export function cell(tr, text, cls) {
  const td = document.createElement("td");
  td.textContent = text;
  if (cls) td.className = cls;
  tr.appendChild(td);
  return td;
}

// trafficCells adds the Now, Served, Downloaded and From cache columns.
export function trafficCells(tr, x) {
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
