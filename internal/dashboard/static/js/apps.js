import { $, cell, trafficCells } from "./dom.js";
import { appLabel, fmtAgo, KIND_LABELS } from "./format.js";
import { closePurge, openPurge } from "./purge.js";
import { state } from "./state.js";

export function selectService(name, focus = true) {
  state.service = name;
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

function renderApps(data) {
  if (data.service !== state.service) return; // a reply for an earlier selection
  const body = $("apps");
  body.textContent = "";
  $("apps-empty").hidden = data.apps.length > 0;
  // One kind gets the column header; a mix is labelled row by row.
  const kinds = new Set(data.apps.map((a) => a.kind).filter((k) => k !== "other"));
  const kind = kinds.size === 1 ? [...kinds][0] : null;
  $("apps-kind").textContent = KIND_LABELS[kind] || "Download";
  const now = state.last ? state.last.end : Date.now();
  // Removing a game needs the password; "other" rows are not one game.
  const purgeable = (a) => state.settings && state.settings.editable && a.kind !== "other" && data.service !== "other";
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

export async function pollApps() {
  if (!state.service) return;
  try {
    const res = await fetch(`api/services/${encodeURIComponent(state.service)}`, { cache: "no-store" });
    if (res.ok) renderApps(await res.json());
  } catch (_) {}
}

export function initApps() {
  try { state.service = new URLSearchParams(location.hash.slice(1)).get("service"); } catch (_) {}
  $("apps-back").addEventListener("click", () => selectService(null));
}
