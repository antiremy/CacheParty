"use strict";

const $ = (id) => document.getElementById(id);

const KINDS = {
  pihole: {
    name: "Pi-hole",
    placeholder: "http://pi.hole",
    secret: "Password or app password",
    help: "Pi-hole v6 or later. An app password (Settings › Web interface / API) is best. The names go into misc.dnsmasq_lines as address=/domain/ip lines, which also cover every name under each domain. Pi-hole restarts its resolver after each change, which takes a moment.",
  },
  adguard: {
    name: "AdGuard Home",
    placeholder: "http://192.168.1.2",
    secret: "Password",
    help: "The names are added as DNS rewrites. Use the AdGuard Home web interface's username and password.",
  },
  technitium: {
    name: "Technitium",
    placeholder: "http://192.168.1.2:5380",
    secret: "API token",
    help: "Create an API token under Administration › Sessions. Each domain gets a conditional forwarder zone that forwards to this-server, so other names in it keep resolving as before.",
  },
};

let state = null;
let editing = null; // server being edited, or {} for a new one

function fmtWhen(iso) {
  const d = new Date(iso);
  const sameDay = d.toDateString() === new Date().toDateString();
  return sameDay
    ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function plural(n, one, many) { return `${n.toLocaleString()} ${n === 1 ? one : many}`; }

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function setStatus(id, msg, cls) {
  $(id).textContent = msg;
  $(id).className = "small status " + (cls || "");
}

function password() {
  const pw = $("password").value;
  if (!pw) $("password").focus();
  return pw;
}

// api sends a change and returns the decoded answer, throwing an Error
// with the server's message on failure.
async function api(method, path, body) {
  const pw = password();
  if (!pw) throw new Error("Enter the dashboard password.");
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + pw },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (_) {
    throw new Error("Could not reach the cache.");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Error ${res.status}`);
  try { sessionStorage.setItem("cacheparty.pw", pw); } catch (_) {}
  return data;
}

// --- what to publish -------------------------------------------------------

function renderAddresses(s) {
  const ips = $("ips");
  if (document.activeElement !== ips) ips.value = s.ips.join(" ");
  ips.disabled = !s.editable;

  const det = $("detected");
  det.textContent = "";
  // IPv6 only when there is no IPv4: hosts carry many short-lived IPv6
  // addresses, which make poor DNS answers.
  const found = s.detected.ipv4.length ? s.detected.ipv4 : s.detected.ipv6;
  if (found.length) {
    det.append("This machine has ");
    found.forEach((a, i) => {
      if (i) det.append(", ");
      const b = el("button", "link", a.address);
      b.type = "button";
      b.title = `Add ${a.address} (${a.interface})`;
      b.disabled = !s.editable;
      b.addEventListener("click", () => {
        const cur = ips.value.split(/[\s,]+/).filter(Boolean);
        if (!cur.includes(a.address)) ips.value = [...cur, a.address].join(" ");
        ips.focus();
      });
      det.append(b);
    });
    det.append(". In a container these are the container's own addresses; use the host's address that clients reach instead.");
  }

  const box = $("services");
  box.textContent = "";
  for (const svc of s.services) {
    const label = el("label");
    const cb = el("input");
    cb.type = "checkbox";
    cb.checked = !svc.excluded;
    cb.value = svc.name;
    cb.disabled = !s.editable;
    label.append(cb, " " + svc.name + " ", el("span", "muted", `(${svc.domains})`));
    box.append(label);
  }
  if (!s.services.length) box.append(el("span", "muted", "No cache-domains lists are loaded."));
}

$("addr-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const ips = $("ips").value.split(/[\s,]+/).filter(Boolean);
  const exclude = [...$("services").querySelectorAll("input:not(:checked)")].map((c) => c.value);
  $("addr-save").disabled = true;
  setStatus("addr-status", "Saving…");
  try {
    render(await api("PUT", "api/dns", { ips, exclude }));
    setStatus("addr-status", "Saved. Sync each server to apply.", "ok");
  } catch (err) {
    setStatus("addr-status", err.message, "err");
  } finally {
    $("addr-save").disabled = false;
  }
});

// --- servers ---------------------------------------------------------------

function resultText(r) {
  if (r.error) return r.error;
  const parts = [];
  if (r.dryRun) {
    parts.push(`would add ${r.added.toLocaleString()}`, `remove ${r.removed.toLocaleString()}`);
  } else {
    parts.push(`added ${r.added.toLocaleString()}`, `removed ${r.removed.toLocaleString()}`);
  }
  parts.push(`${r.unchanged.toLocaleString()} already in place`);
  return (r.dryRun ? "Preview: " : "") + parts.join(", ");
}

function renderServers(s) {
  const list = $("servers");
  list.textContent = "";
  $("servers-empty").hidden = s.targets.length > 0;
  for (const t of s.targets) {
    const row = el("div", "server");
    const head = el("div", "server-head");
    const title = el("div", "server-title");
    title.append(el("b", "", KINDS[t.kind].name), " ", el("span", "server-url", t.url));
    const badges = el("div", "server-badges");
    if (t.auto) badges.append(el("span", "badge", "kept in sync"));
    badges.append(el("span", "badge", plural(t.managed, "record added", "records added")));
    head.append(title, badges);
    row.append(head);

    const last = el("p", "small server-last");
    last.dataset.id = t.id;
    if (t.last) {
      last.classList.add(t.last.error ? "err" : "ok");
      last.textContent = `${fmtWhen(t.last.time)}: ${resultText(t.last)}`;
    } else {
      last.classList.add("muted");
      last.textContent = "Not synced yet.";
    }
    row.append(last);

    if (s.editable) {
      const actions = el("div", "save-row");
      const btn = (label, cls, fn) => {
        const b = el("button", cls, label);
        b.type = "button";
        b.disabled = s.running;
        b.addEventListener("click", () => fn(b));
        actions.append(b);
        return b;
      };
      btn("Sync now", "primary", () => sync(t, {}));
      btn("Preview", "btn", () => sync(t, { dryRun: true }));
      btn("Edit", "btn", () => openForm(t));
      // Removing records and forgetting the server need a second click.
      btn("Remove records", "btn", (b) => confirmClick(b, `Remove ${plural(t.managed, "record", "records")}`, () => sync(t, { remove: true })));
      btn("Delete", "btn", (b) => confirmClick(b, t.managed ? "Delete, leaving its records" : "Delete server", () => deleteServer(t)));
      row.append(actions);
    }
    list.append(row);
  }
}

function confirmClick(b, label, fn) {
  if (b.dataset.armed) { fn(); return; }
  const orig = b.textContent;
  b.dataset.armed = "1";
  b.textContent = label + "?";
  b.classList.add("danger");
  setTimeout(() => {
    if (!b.isConnected) return;
    delete b.dataset.armed;
    b.textContent = orig;
    b.classList.remove("danger");
  }, 5000);
}

function lastLine(id) {
  return document.querySelector(`.server-last[data-id="${CSS.escape(id)}"]`);
}

async function sync(t, opt) {
  document.querySelectorAll("#servers button").forEach((b) => (b.disabled = true));
  const line = lastLine(t.id);
  line.className = "small server-last muted";
  line.textContent = opt.remove ? "Removing records…" : opt.dryRun ? "Checking…" : "Syncing…";
  try {
    const data = await api("POST", `api/dns/servers/${encodeURIComponent(t.id)}/sync`, opt);
    render(data.state);
    if (opt.dryRun) {
      // A preview is not saved as the last result, so show it here.
      const l = lastLine(t.id);
      l.className = "small server-last " + (data.result.error ? "err" : "ok");
      l.textContent = resultText(data.result);
    }
  } catch (err) {
    line.className = "small server-last err";
    line.textContent = err.message;
    document.querySelectorAll("#servers button").forEach((b) => (b.disabled = false));
  }
}

async function deleteServer(t) {
  try {
    render(await api("DELETE", `api/dns/servers/${encodeURIComponent(t.id)}`));
  } catch (err) {
    const line = lastLine(t.id);
    line.className = "small server-last err";
    line.textContent = err.message;
  }
}

// --- add / edit form -------------------------------------------------------

function kindChanged() {
  const k = KINDS[$("kind").value];
  $("url").placeholder = k.placeholder;
  $("secret-label").textContent = k.secret;
  $("kind-help").textContent = k.help;
  $("username-row").hidden = $("kind").value !== "adguard";
  const keeps = editing && editing.hasSecret && editing.kind === $("kind").value;
  $("secret").placeholder = keeps ? "unchanged" : "";
}

function openForm(t) {
  editing = t || {};
  $("server-form-title").textContent = t ? `Edit ${KINDS[t.kind].name}` : "Add a server";
  $("kind").value = t ? t.kind : "pihole";
  $("url").value = t ? t.url : "";
  $("username").value = t ? t.username : "";
  $("secret").value = "";
  $("auto").checked = t ? t.auto : true;
  $("insecure").checked = t ? t.insecure : false;
  setStatus("server-status", "");
  kindChanged();
  $("server-form").hidden = false;
  $("url").focus();
}

$("kind").addEventListener("change", kindChanged);
$("add-server").addEventListener("click", () => openForm(null));
$("server-cancel").addEventListener("click", () => { $("server-form").hidden = true; editing = null; });

$("server-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const body = {
    id: editing && editing.id ? editing.id : "",
    kind: $("kind").value,
    url: $("url").value.trim(),
    username: $("username").value.trim(),
    secret: $("secret").value,
    auto: $("auto").checked,
    insecure: $("insecure").checked,
  };
  if (body.kind !== "adguard") body.username = "";
  $("server-save").disabled = true;
  setStatus("server-status", "Saving…");
  try {
    render(await api("POST", "api/dns/servers", body));
    $("server-form").hidden = true;
    editing = null;
  } catch (err) {
    setStatus("server-status", err.message, "err");
  } finally {
    $("server-save").disabled = false;
  }
});

// --- page ------------------------------------------------------------------

function render(s) {
  state = s;
  $("dns-off").hidden = s.enabled;
  $("dns-main").hidden = !s.enabled;
  $("dns-readonly").hidden = !s.enabled || s.editable;
  document.querySelectorAll(".editable").forEach((e) => (e.hidden = !s.editable));
  if (!s.enabled) return;
  renderAddresses(s);
  renderServers(s);
}

try { $("password").value = sessionStorage.getItem("cacheparty.pw") || ""; } catch (_) {}

fetch("api/dns", { cache: "no-store" })
  .then((r) => r.json())
  .then(render)
  .catch(() => {
    const p = $("dns-off");
    p.textContent = "Could not load DNS sync settings.";
    p.hidden = false;
  });
