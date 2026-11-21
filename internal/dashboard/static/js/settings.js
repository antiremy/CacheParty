import { $, cell } from "./dom.js";
import { state } from "./state.js";

function renderSettings(data) {
  state.settings = data;
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

export function initSettings() {
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
}
