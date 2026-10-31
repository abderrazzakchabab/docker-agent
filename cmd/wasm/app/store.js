// Browser storage with every access guarded: it can be missing or throw
// (private windows, blocked site data).

const NS = "docker-agent-app:";

function area(kind) {
  try {
    return kind === "session" ? window.sessionStorage : window.localStorage;
  } catch {
    return null;
  }
}

export function read(kind, key) {
  try {
    return area(kind)?.getItem(NS + key) ?? null;
  } catch {
    return null;
  }
}

export function write(kind, key, value) {
  try {
    area(kind)?.setItem(NS + key, value);
    return true;
  } catch {
    return false;
  }
}

export function remove(kind, key) {
  try {
    area(kind)?.removeItem(NS + key);
  } catch {}
}

export function readJSON(kind, key, fallback) {
  try {
    const raw = read(kind, key);
    return raw === null ? fallback : JSON.parse(raw);
  } catch {
    return fallback;
  }
}

export function writeJSON(kind, key, value) {
  return write(kind, key, JSON.stringify(value));
}
