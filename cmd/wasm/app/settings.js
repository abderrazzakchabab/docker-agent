// Keys & settings panel: OpenRouter sign-in, env rows, tool proxy,
// auto-approve and RAG documents.
//
// Secrets (env values, OpenRouter key) live in memory and in sessionStorage,
// or in localStorage when "remember" is ticked. Everything else is not secret.
import * as auth from "./auth.js";
import { readJSON, remove, writeJSON } from "./store.js";

const MAX_DOC_BYTES = 16 * 1024 * 1024;
const MAX_DOCS = 1000;

const $ = (id) => document.getElementById(id);

function safe(fn) {
  try {
    return fn();
  } catch {
    return null;
  }
}

export function createSettings({ onChange, onNotice }) {
  const prefs = readJSON("local", "settings", {});
  const state = {
    remember: prefs.remember === true,
    toolProxy: typeof prefs.toolProxy === "string" ? prefs.toolProxy : "",
    autoApprove: prefs.autoApprove === true,
    env: [],
    orKey: null,
    documents: new Map(),
    documentsVersion: 0,
  };

  const kind = () => (state.remember ? "local" : "session");
  const other = () => (state.remember ? "session" : "local");

  function persistPrefs() {
    writeJSON("local", "settings", {
      remember: state.remember,
      toolProxy: state.toolProxy,
      autoApprove: state.autoApprove,
    });
  }

  function persistSecrets() {
    writeJSON(kind(), "secrets", { orKey: state.orKey, env: state.env.map(({ name, value }) => ({ name, value })) });
    remove(other(), "secrets");
  }

  function loadSecrets() {
    const saved = readJSON(kind(), "secrets", null);
    if (saved) {
      state.orKey = typeof saved.orKey === "string" ? saved.orKey : null;
      state.env = Array.isArray(saved.env) ? saved.env.map((r) => ({ name: String(r.name ?? ""), value: String(r.value ?? "") })) : [];
    }
    // The key auth.js stores itself is only trusted when the user opted in to persistence.
    if (!state.orKey && state.remember) state.orKey = safe(() => auth.getKey());
  }

  // auth.js always writes to localStorage; honour an unticked "remember".
  function adoptCallbackKey(key) {
    state.orKey = key;
    if (!state.remember) safe(() => auth.signOut());
    persistSecrets();
  }

  function renderOpenRouter() {
    const signedIn = !!state.orKey;
    $("or-signin").hidden = signedIn;
    $("or-signout").hidden = !signedIn;
    $("or-status").textContent = signedIn ? "Signed in" : "Not signed in";
  }

  function renderRememberHint() {
    $("remember-hint").textContent = state.remember
      ? "Keys are stored unencrypted in localStorage and readable by any script on this origin."
      : "Keys stay in this tab (memory and sessionStorage) and are gone when it closes.";
  }

  function renderEnv() {
    const rows = $("env-rows");
    rows.replaceChildren();
    state.env.forEach((row, i) => {
      const wrap = document.createElement("div");
      wrap.className = "env-row";
      const name = document.createElement("input");
      name.type = "text";
      name.className = "env-name";
      name.placeholder = "NAME";
      name.setAttribute("list", "env-names");
      name.setAttribute("aria-label", "Variable name");
      name.autocomplete = "off";
      name.spellcheck = false;
      name.value = row.name;
      const value = document.createElement("input");
      value.type = "password";
      value.placeholder = "value";
      value.setAttribute("aria-label", `Value of ${row.name || "variable"}`);
      value.autocomplete = "off";
      value.value = row.value;
      const reveal = document.createElement("button");
      reveal.type = "button";
      reveal.className = "ghost small";
      reveal.textContent = "Show";
      reveal.addEventListener("click", () => {
        const hidden = value.type === "password";
        value.type = hidden ? "text" : "password";
        reveal.textContent = hidden ? "Hide" : "Show";
      });
      const del = document.createElement("button");
      del.type = "button";
      del.className = "ghost small";
      del.textContent = "Remove";
      del.addEventListener("click", () => {
        state.env.splice(i, 1);
        persistSecrets();
        renderEnv();
        onChange();
      });
      for (const [input, field] of [[name, "name"], [value, "value"]]) {
        input.addEventListener("input", () => {
          row[field] = input.value;
          persistSecrets();
          onChange();
        });
      }
      wrap.append(name, value, reveal, del);
      rows.append(wrap);
    });
  }

  function renderDocs() {
    const list = $("docs-list");
    list.replaceChildren();
    for (const [name, text] of state.documents) {
      const li = document.createElement("li");
      const label = document.createElement("span");
      label.textContent = `${name} (${new Blob([text]).size.toLocaleString()} bytes)`;
      const del = document.createElement("button");
      del.type = "button";
      del.className = "ghost small";
      del.textContent = "Remove";
      del.addEventListener("click", () => {
        state.documents.delete(name);
        state.documentsVersion++;
        renderDocs();
        onChange();
      });
      li.append(label, del);
      list.append(li);
    }
  }

  async function addDocuments(files) {
    let total = [...state.documents.values()].reduce((n, t) => n + new Blob([t]).size, 0);
    for (const file of files) {
      if (state.documents.size >= MAX_DOCS) {
        onNotice(`At most ${MAX_DOCS} documents; skipped ${file.name}.`);
        continue;
      }
      if (total + file.size > MAX_DOC_BYTES) {
        onNotice(`Documents are limited to 16 MiB in total; skipped ${file.name}.`);
        continue;
      }
      try {
        state.documents.set(file.name, await file.text());
        state.documentsVersion++;
        total += file.size;
      } catch (err) {
        onNotice(`Could not read ${file.name}: ${err.message}`);
      }
    }
    renderDocs();
    onChange();
  }

  function bind() {
    loadSecrets();
    $("remember").checked = state.remember;
    $("tool-proxy").value = state.toolProxy;
    $("auto-approve").checked = state.autoApprove;
    renderOpenRouter();
    renderRememberHint();
    renderEnv();
    renderDocs();

    $("or-signin").addEventListener("click", () => {
      auth.signIn().catch((err) => onNotice(`Sign-in failed: ${err.message}`));
    });
    $("or-signout").addEventListener("click", () => {
      safe(() => auth.signOut());
      state.orKey = null;
      persistSecrets();
      renderOpenRouter();
      onChange();
    });
    $("env-add").addEventListener("click", () => {
      state.env.push({ name: "", value: "" });
      renderEnv();
      $("env-rows").lastElementChild?.querySelector("input")?.focus();
    });
    $("remember").addEventListener("change", (e) => {
      state.remember = e.target.checked;
      persistPrefs();
      persistSecrets();
      if (!state.remember) safe(() => auth.signOut());
      else if (state.orKey) safe(() => localStorage.setItem("openrouter_api_key", state.orKey));
      renderRememberHint();
    });
    $("tool-proxy").addEventListener("input", (e) => {
      state.toolProxy = e.target.value.trim();
      persistPrefs();
      onChange();
    });
    $("auto-approve").addEventListener("change", (e) => {
      state.autoApprove = e.target.checked;
      persistPrefs();
      onChange();
    });
    $("docs-input").addEventListener("change", async (e) => {
      await addDocuments([...e.target.files]);
      e.target.value = "";
    });
  }

  return {
    bind,
    // Completes an OpenRouter redirect if the page was opened with ?code=.
    async handleCallback() {
      try {
        const key = await auth.handleCallback();
        if (key) {
          adoptCallbackKey(key);
          renderOpenRouter();
          onChange();
          onNotice("Signed in to OpenRouter.");
        }
      } catch (err) {
        onNotice(`OpenRouter sign-in failed: ${err.message}`);
      }
    },
    documentsVersion: () => state.documentsVersion,
    sessionOptions() {
      const env = {};
      if (state.orKey) env.OPENROUTER_API_KEY = state.orKey;
      for (const { name, value } of state.env) {
        if (name.trim() && value) env[name.trim()] = value;
      }
      const opts = { env, autoApprove: state.autoApprove };
      if (state.toolProxy) opts.toolProxy = state.toolProxy;
      if (state.documents.size) opts.documents = Object.fromEntries(state.documents);
      return opts;
    },
  };
}
