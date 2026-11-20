const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
const RATE_UNITS = ["bit/s", "kbit/s", "Mbit/s", "Gbit/s", "Tbit/s"];
const BYTE_RATE_UNITS = ["B/s", "kB/s", "MB/s", "GB/s", "TB/s"];

export const KIND_LABELS = { game: "Game", depot: "Depot", product: "Product", app: "App", host: "Host" };

export function fmtBytes(n) {
  let i = 0;
  while (Math.abs(n) >= 1024 && i < BYTE_UNITS.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n.toFixed(0) : n.toFixed(n < 10 ? 2 : n < 100 ? 1 : 0)) + " " + BYTE_UNITS[i];
}

// Rates are shown in bits, as network links are rated.
export function fmtRate(bytesPerSec) {
  let n = bytesPerSec * 8, i = 0;
  while (n >= 1000 && i < RATE_UNITS.length - 1) { n /= 1000; i++; }
  return (n === 0 ? "0" : n.toFixed(n < 10 ? 1 : 0)) + " " + RATE_UNITS[i];
}

// Disk rates are shown in bytes, as disks are rated.
export function fmtByteRate(bytesPerSec) {
  let n = bytesPerSec, i = 0;
  while (n >= 1000 && i < BYTE_RATE_UNITS.length - 1) { n /= 1000; i++; }
  return (n === 0 ? "0" : n.toFixed(n < 10 ? 1 : 0)) + " " + BYTE_RATE_UNITS[i];
}

export function fmtCount(n) { return n.toLocaleString(); }

export function fmtDuration(sec) {
  sec = Math.floor(sec);
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m ${sec % 60}s`;
}

export function fmtTime(ms, withSeconds) {
  return new Date(ms).toLocaleTimeString([], withSeconds
    ? { hour: "2-digit", minute: "2-digit", second: "2-digit" }
    : { hour: "2-digit", minute: "2-digit" });
}

export function fmtAgo(ms, now) {
  if (!ms) return "—";
  const s = Math.max(0, (now - ms) / 1000);
  if (s < 5) return "now";
  return (s < 60 ? Math.floor(s) + "s" : fmtDuration(s)) + " ago";
}

export function appLabel(a) {
  const label = a.kind === "host" ? a.id : `${(KIND_LABELS[a.kind] || a.kind).toLowerCase()} ${a.id}`;
  return a.name ? `${a.name} (${label})` : label;
}
