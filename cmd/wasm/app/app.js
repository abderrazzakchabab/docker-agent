import { createChat } from "./chat.js";
import { loadDockerAgent } from "./loader.js";
import { cleanError } from "./md.js";
import { createSettings } from "./settings.js";
import { read, readJSON, write, writeJSON } from "./store.js";

const $ = (id) => document.getElementById(id);

const DEFAULT_YAML = `providers:
  openrouter:
    provider: openai
    base_url: https://openrouter.ai/api/v1
    token_key: OPENROUTER_API_KEY

agents:
  root:
    model: openrouter/meta-llama/llama-3.3-70b-instruct:free
    description: A helpful assistant.
    instruction: |
      You are a helpful assistant. Be concise.
`;

const FALLBACK_EXAMPLES = ["portable-team", "handbook-rag", "cloud-bedrock", "cloud-vertex"];

const app = {
  api: null,
  session: null,
  signature: null,
  generation: 0,
  running: false,
  starting: null,
  usage: { input: 0, output: 0, cost: 0, context: 0, limit: 0 },
};

function setBadge(node, text, tone = "") {
  node.textContent = text;
  if (tone) node.dataset.tone = tone;
  else delete node.dataset.tone;
}

// ---- Theme ----
const THEMES = ["auto", "light", "dark"];
function initTheme() {
  let theme = read("local", "theme");
  if (!THEMES.includes(theme)) theme = "auto";
  const apply = () => {
    if (theme === "auto") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
    $("theme-toggle").textContent = `Theme: ${theme}`;
  };
  apply();
  $("theme-toggle").addEventListener("click", () => {
    theme = THEMES[(THEMES.indexOf(theme) + 1) % THEMES.length];
    write("local", "theme", theme);
    apply();
  });
}

// ---- Chat + settings ----
const chat = createChat({
  log: $("log"),
  onUsage(ev) {
    app.usage.input += ev.input_tokens || 0;
    app.usage.output += ev.output_tokens || 0;
    app.usage.cost = ev.cost || app.usage.cost;
    app.usage.context = ev.context_length || 0;
    app.usage.limit = ev.context_limit || 0;
    renderUsage();
  },
});

const settings = createSettings({
  onChange: () => refreshSessionState(),
  onNotice: (text) => chat.notice(text, "warn"),
});

function renderUsage() {
  const u = app.usage;
  const badge = $("usage");
  if (!u.input && !u.output) {
    badge.hidden = true;
    return;
  }
  const parts = [`in ${u.input.toLocaleString()}`, `out ${u.output.toLocaleString()}`];
  if (u.limit) parts.push(`ctx ${Math.round((u.context / u.limit) * 100)}%`);
  if (u.cost) parts.push(`$${u.cost.toFixed(u.cost < 0.01 ? 5 : 3)}`);
  badge.textContent = parts.join(" · ");
  badge.hidden = false;
}

function resetUsage() {
  app.usage = { input: 0, output: 0, cost: 0, context: 0, limit: 0 };
  renderUsage();
}

// ---- Config editor ----
const yamlBox = $("yaml");

function saveDraft() {
  writeJSON("local", "draft", { yaml: yamlBox.value, agent: $("agent-picker").value });
}

function setValidation(text, ok) {
  const node = $("validation");
  node.textContent = text;
  node.className = `status-line ${ok === true ? "ok" : ok === false ? "bad" : ""}`;
}

function validate() {
  if (!app.api) return setValidation("Runtime is still loading.", null);
  try {
    const cfg = app.api.parseConfig(yamlBox.value);
    const agents = app.api.listAgents(yamlBox.value);
    populateAgents(agents);
    const models = Object.keys(cfg.models ?? {}).length;
    setValidation(`OK: ${agents.length} agent${agents.length === 1 ? "" : "s"}, ${models} model${models === 1 ? "" : "s"}.`, true);
    return true;
  } catch (err) {
    setValidation(cleanError(err.message ?? err), false);
    return false;
  }
}

function populateAgents(agents) {
  const picker = $("agent-picker");
  const wanted = picker.value;
  picker.replaceChildren(new Option("(default)", ""));
  for (const a of agents) {
    picker.add(new Option(a.name + (a.model ? ` (${a.model})` : ""), a.name));
  }
  picker.value = agents.some((a) => a.name === wanted) ? wanted : "";
}

let validateTimer;
function onYamlEdited() {
  saveDraft();
  refreshSessionState();
  clearTimeout(validateTimer);
  validateTimer = setTimeout(validate, 500);
}

function initEditor() {
  const draft = readJSON("local", "draft", null);
  yamlBox.value = typeof draft?.yaml === "string" && draft.yaml.trim() ? draft.yaml : DEFAULT_YAML;
  yamlBox.addEventListener("input", onYamlEdited);
  yamlBox.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      yamlBox.dataset.free = "1";
      return;
    }
    if (e.key === "Tab" && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey && yamlBox.dataset.free !== "1") {
      e.preventDefault();
      yamlBox.setRangeText("  ", yamlBox.selectionStart, yamlBox.selectionEnd, "end");
      onYamlEdited();
      return;
    }
    if (e.key !== "Tab") delete yamlBox.dataset.free;
  });
  $("validate").addEventListener("click", validate);
  $("agent-picker").addEventListener("change", () => {
    saveDraft();
    refreshSessionState();
  });
  initExamples(typeof draft?.agent === "string" ? draft.agent : "");
}

async function initExamples(savedAgent) {
  const picker = $("example-picker");
  picker.add(new Option("Load example…", ""));
  picker.add(new Option("Default (OpenRouter)", "__default"));
  let names = FALLBACK_EXAMPLES;
  try {
    const res = await fetch("examples/index.json");
    if (res.ok) names = await res.json();
  } catch {}
  for (const name of names) picker.add(new Option(name, name));
  picker.addEventListener("change", async () => {
    const name = picker.value;
    picker.value = "";
    if (!name) return;
    try {
      if (name === "__default") {
        yamlBox.value = DEFAULT_YAML;
      } else {
        const res = await fetch(`examples/${encodeURIComponent(name)}.yaml`);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        yamlBox.value = await res.text();
      }
      $("agent-picker").value = "";
      onYamlEdited();
      validate();
    } catch (err) {
      setValidation(`Could not load example ${name}: ${err.message}`, false);
    }
  });
  app.pendingAgent = savedAgent;
}

// ---- Session lifecycle ----
function currentOptions() {
  return {
    yaml: yamlBox.value,
    agentName: $("agent-picker").value || undefined,
    ...settings.sessionOptions(),
  };
}

// Documents can total 16 MiB; compare their version, not their text, on every keystroke.
const signatureOf = (opts) => JSON.stringify({ ...opts, documents: settings.documentsVersion() });

function refreshSessionState() {
  const status = $("session-status");
  const hasKey = !!app.api;
  $("send").disabled = !hasKey || app.running;
  $("stop").disabled = !app.running;
  $("start-session").disabled = !hasKey || app.running;
  if (!hasKey) return setBadge(status, "Runtime loading");
  if (app.running) return setBadge(status, "Running", "ok");
  if (!app.session) return setBadge(status, "No session");
  if (app.signature !== signatureOf(currentOptions())) return setBadge(status, "Stale: restarts on next send", "warn");
  setBadge(status, "Session ready", "ok");
}

async function closeSession() {
  const old = app.session;
  app.session = null;
  app.signature = null;
  app.generation++;
  if (old) {
    try {
      await old.close();
    } catch (err) {
      console.warn("close failed:", err);
    }
  }
}

function eventHandler(generation) {
  return (ev) => {
    if (generation !== app.generation) return;
    if (ev.type === "error") app.errorShown = true;
    chat.handleEvent(ev, app.session);
  };
}

async function ensureSession() {
  if (app.starting) return app.starting;
  const opts = currentOptions();
  const sig = signatureOf(opts);
  if (app.session && app.signature === sig) return app.session;

  app.starting = (async () => {
    const hadSession = !!app.session;
    await closeSession();
    const generation = app.generation;
    setBadge($("session-status"), "Starting…");
    const session = await app.api.createSession(opts, eventHandler(generation));
    if (generation !== app.generation) {
      session.close().catch(() => {});
      throw new Error("session was replaced while starting");
    }
    app.session = session;
    app.signature = sig;
    if (hadSession) {
      chat.notice("Config or settings changed: started a new session. Earlier messages are no longer in the model's context.");
      resetUsage();
    }
    return session;
  })();
  try {
    return await app.starting;
  } finally {
    app.starting = null;
  }
}

async function startSession() {
  if (!app.api) return;
  try {
    await ensureSession();
    chat.notice("Session ready.");
  } catch (err) {
    chat.notice(`Could not start session: ${cleanError(err.message ?? err)}`, "error");
  }
  refreshSessionState();
}

async function sendPrompt(text) {
  if (!app.api || app.running || !text.trim()) return;
  app.running = true;
  refreshSessionState();
  let session;
  try {
    session = await ensureSession();
  } catch (err) {
    chat.notice(`Could not start session: ${cleanError(err.message ?? err)}`, "error");
    app.running = false;
    refreshSessionState();
    return;
  }
  $("prompt").value = "";
  chat.userMessage(text);
  chat.beginTurn();
  refreshSessionState();
  const generation = app.generation;
  app.errorShown = false;
  try {
    await session.send(text);
  } catch (err) {
    const message = String(err.message ?? err);
    if (generation === app.generation) {
      if (/aborted/i.test(message)) chat.notice("Stopped.");
      else if (!app.errorShown) chat.notice(cleanError(message), "error");
    }
  } finally {
    if (generation === app.generation) chat.endTurn();
    app.running = false;
    refreshSessionState();
    $("prompt").focus();
  }
}

async function newConversation() {
  chat.clear();
  resetUsage();
  if (!app.session) return;
  const session = app.session;
  const stale = app.signature !== signatureOf(currentOptions());
  try {
    if (stale) await closeSession();
    else await session.restart();
  } catch (err) {
    chat.notice(`Could not restart: ${err.message ?? err}`, "error");
  }
  refreshSessionState();
}

function initComposer() {
  const prompt = $("prompt");
  $("composer").addEventListener("submit", (e) => {
    e.preventDefault();
    sendPrompt(prompt.value);
  });
  prompt.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendPrompt(prompt.value);
    }
  });
  $("stop").addEventListener("click", () => app.session?.abort());
  $("start-session").addEventListener("click", startSession);
  $("new-convo").addEventListener("click", newConversation);
  window.addEventListener("pagehide", () => {
    try {
      app.session?.close();
    } catch {}
  });
}

// ---- Boot ----
function formatBytes(n) {
  return n >= 1048576 ? `${(n / 1048576).toFixed(1)} MB` : `${Math.round(n / 1024)} KB`;
}

async function boot() {
  const banner = $("load-banner");
  const progress = $("load-progress");
  try {
    app.api = await loadDockerAgent({
      onStatus: (text) => ($("load-label").textContent = text),
      onProgress: ({ received, total }) => {
        if (total) {
          progress.max = total;
          progress.value = received;
          $("load-detail").textContent = `${formatBytes(received)} / ${formatBytes(total)}`;
        } else {
          progress.removeAttribute("value");
          $("load-detail").textContent = formatBytes(received);
        }
      },
    });
    banner.hidden = true;
    setBadge($("runtime-status"), "Runtime ready", "ok");
    if (validate() && app.pendingAgent) {
      $("agent-picker").value = app.pendingAgent;
      if ($("agent-picker").value !== app.pendingAgent) $("agent-picker").value = "";
    }
  } catch (err) {
    console.error(err);
    banner.classList.add("failed");
    progress.hidden = true;
    $("load-label").textContent = "Could not load the runtime";
    $("load-detail").textContent = String(err.message ?? err);
    setBadge($("runtime-status"), "Runtime failed", "bad");
  }
  refreshSessionState();
}

initTheme();
initEditor();
settings.bind();
initComposer();
refreshSessionState();
settings.handleCallback();
boot();
