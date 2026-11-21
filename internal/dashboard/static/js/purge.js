import { $ } from "./dom.js";
import { appLabel, fmtBytes, fmtCount } from "./format.js";
import { state } from "./state.js";

// Purging asks the cache what it holds for the game first, and removes it
// only once that has been shown and confirmed.
let purging = null; // {service, kind, id} being purged, or null

function purgeStatus(msg, cls) {
  $("purge-status").textContent = msg;
  $("purge-status").className = "small " + (cls || "");
}

export function openPurge(a) {
  purging = { service: state.service, kind: a.kind, id: a.id };
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

export function closePurge() {
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

export function initPurge() {
  $("purge").addEventListener("submit", (e) => { e.preventDefault(); purgeRequest(false); });
  $("purge-go").addEventListener("click", () => purgeRequest(true));
  $("purge-cancel").addEventListener("click", closePurge);
}
