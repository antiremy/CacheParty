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

let lastApps = null; // the newest reply, redrawn when a game is opened
const openGames = new Set(); // Steam games whose depots are shown

function renderApps(data) {
  if (data.service !== state.service) return; // a reply for an earlier selection
  lastApps = data;
  const body = $("apps");
  body.textContent = "";
  body.classList.toggle("pictures", !!data.pictures);
  $("apps-empty").hidden = data.apps.length > 0;
  // With every row named the column is "Game". Otherwise one kind gets the
  // column header, and a mix is labelled row by row.
  const named = data.apps.some((a) => a.name) && data.apps.every((a) => a.name || a.kind === "other");
  const kinds = new Set(data.apps.map((a) => a.kind).filter((k) => k !== "other"));
  const kind = named ? "game" : kinds.size === 1 ? [...kinds][0] : null;
  $("apps-kind").textContent = KIND_LABELS[kind] || "Download";
  const now = state.last ? state.last.end : Date.now();
  // Removing a game needs the password; "other" rows are not one game. A
  // grouped game removes all of its depots at once.
  const purgeable = (a) => state.settings && state.settings.editable && a.kind !== "other" && data.service !== "other";
  $("apps-actions").hidden = !data.apps.some((a) => purgeable(a) || (a.depots || []).some(purgeable));
  const row = (a, sub) => {
    const tr = document.createElement("tr");
    if (sub) tr.className = "sub";
    const td = cell(tr, "");
    if (data.pictures && !sub) picture(td, a);
    appName(td, a, sub);
    if (!kind && a.kind !== "other") {
      const badge = document.createElement("span");
      badge.className = "badge"; badge.textContent = (KIND_LABELS[a.kind] || a.kind).toLowerCase();
      td.appendChild(badge);
    }
    if (a.depots) depotsToggle(td, a);
    trafficCells(tr, a);
    cell(tr, fmtAgo(a.lastActive, now), "num");
    if (!$("apps-actions").hidden) {
      const td = cell(tr, "", "num");
      if (purgeable(a)) {
        const btn = document.createElement("button");
        btn.type = "button"; btn.className = "link"; btn.textContent = "Purge";
        btn.title = a.depots ? `Remove everything cached for ${appLabel(a)}, all ${a.depots.length} depots` : `Remove everything cached for ${appLabel(a)}`;
        btn.addEventListener("click", () => openPurge(a));
        td.appendChild(btn);
      }
    }
    body.appendChild(tr);
  };
  for (const a of data.apps) {
    row(a, false);
    if (a.depots && openGames.has(a.id)) a.depots.forEach((d) => row(d, true));
  }
}

// appName fills td with a's name, or its ID where the name is not known.
// Steam games and depots link to SteamDB. A depot listed under its game
// shows its ID, since the game row already names it.
function appName(td, a, sub) {
  const text = a.kind === "other" ? "Others (limit reached)" : sub ? `Depot ${a.id}` : a.name || a.id;
  if (a.kind !== "depot" && a.kind !== "game") {
    td.textContent = text;
    if (a.name) td.title = `${KIND_LABELS[a.kind] || a.kind} ${a.id}`;
    return;
  }
  const link = document.createElement("a");
  link.href = `https://steamdb.info/${a.kind === "game" ? "app" : "depot"}/${encodeURIComponent(a.id)}/`;
  link.target = "_blank"; link.rel = "noopener noreferrer";
  link.title = a.kind === "game" ? `Look up this game on SteamDB (app ${a.id})` : `Look up depot ${a.id} on SteamDB`;
  link.textContent = text;
  td.appendChild(link);
}

// picture adds Steam's picture of a's game (GAME_PICTURES), or an empty
// space of the same size so names line up. The viewer's browser loads it
// from Steam.
function picture(td, a) {
  const id = a.kind === "game" ? a.id : a.game;
  if (!id) {
    const space = document.createElement("span");
    space.className = "cover";
    td.appendChild(space);
    return;
  }
  const img = document.createElement("img");
  img.className = "cover"; img.alt = ""; img.loading = "lazy";
  img.src = `https://shared.akamai.steamstatic.com/store_item_assets/steam/apps/${encodeURIComponent(id)}/header.jpg`;
  img.addEventListener("error", () => { img.style.visibility = "hidden"; });
  td.appendChild(img);
}

// depotsToggle adds a button that shows or hides a game's depots.
function depotsToggle(td, a) {
  const btn = document.createElement("button");
  btn.type = "button"; btn.className = "link small depots";
  btn.textContent = a.depots.length === 1 ? "1 depot" : `${a.depots.length} depots`;
  btn.setAttribute("aria-expanded", openGames.has(a.id));
  btn.addEventListener("click", () => {
    if (!openGames.delete(a.id)) openGames.add(a.id);
    renderApps(lastApps);
    document.querySelector(`#apps button.depots[data-game="${CSS.escape(a.id)}"]`)?.focus();
  });
  btn.dataset.game = a.id;
  td.appendChild(btn);
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
