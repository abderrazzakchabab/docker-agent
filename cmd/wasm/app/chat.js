// Renders the session event stream into the chat log. Everything from the
// model, tools or config is written with textContent / DOM nodes.
import { cleanError, renderMarkdown } from "./md.js";

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function button(label, className, onClick) {
  const b = el("button", className, label);
  b.type = "button";
  b.addEventListener("click", onClick);
  return b;
}

function prettyJSON(text) {
  if (!text) return "";
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}

function safeHref(url) {
  try {
    const u = new URL(url);
    return u.protocol === "https:" || u.protocol === "http:" ? u.href : null;
  } catch {
    return null;
  }
}

// Skeleton for an MCP form schema: one empty value per property.
function skeleton(schema) {
  const out = {};
  for (const [name, spec] of Object.entries(schema?.properties ?? {})) {
    const t = spec?.type;
    out[name] = spec?.default ?? (t === "boolean" ? false : t === "number" || t === "integer" ? 0 : t === "array" ? [] : "");
  }
  return out;
}

export function createChat({ log, onUsage }) {
  let turn = null;
  let renderQueued = false;

  const nearBottom = () => log.scrollHeight - log.scrollTop - log.clientHeight < 80;
  const append = (node) => {
    const stick = nearBottom();
    log.querySelector(".empty")?.remove();
    log.append(node);
    if (stick) log.scrollTop = log.scrollHeight;
  };
  const scrollIfNear = (stick) => {
    if (stick) log.scrollTop = log.scrollHeight;
  };

  function notice(text, tone = "") {
    append(el("div", `notice ${tone}`.trim(), text));
  }

  function flushRender() {
    renderQueued = false;
    const cur = turn?.cur;
    if (!cur || cur.kind !== "text" || cur.rendered === cur.text) return;
    const stick = nearBottom();
    cur.el.replaceChildren(renderMarkdown(cur.text));
    cur.rendered = cur.text;
    scrollIfNear(stick);
  }

  function queueRender() {
    if (renderQueued) return;
    renderQueued = true;
    if (typeof requestAnimationFrame === "function") requestAnimationFrame(flushRender);
    else setTimeout(flushRender, 16);
  }

  function appendPart(kind, chunk) {
    if (!turn.cur || turn.cur.kind !== kind) {
      flushRender();
      let node;
      let target;
      if (kind === "reasoning") {
        node = el("details", "reasoning");
        node.append(el("summary", "", "Reasoning"));
        target = el("div", "reasoning-body");
        node.append(target);
      } else {
        node = target = el("div", "md");
      }
      turn.el.append(node);
      turn.cur = { kind, el: target, text: "", rendered: null };
    }
    const cur = turn.cur;
    cur.text += chunk;
    if (kind === "reasoning") {
      const stick = nearBottom();
      cur.el.textContent = cur.text;
      scrollIfNear(stick);
    } else {
      queueRender();
    }
  }

  function toolCard(id, name) {
    let card = turn.tools.get(id);
    if (card) {
      if (name && !card.name) {
        card.name = name;
        card.title.textContent = name;
      }
      return card;
    }
    flushRender();
    turn.cur = null;
    const root = el("div", "card");
    const head = el("div", "card-head");
    const title = el("span", "card-title", name || "tool");
    const status = el("span", "badge", "preparing");
    head.append(title, status);
    const argsPre = el("pre");
    const argsDetails = el("details");
    argsDetails.open = true;
    argsDetails.append(el("summary", "", "Arguments"), argsPre);
    const outPre = el("pre", "out");
    const outDetails = el("details");
    outDetails.open = true;
    outDetails.hidden = true;
    outDetails.append(el("summary", "", "Output"), outPre);
    const actions = el("div", "actions");
    actions.hidden = true;
    root.append(head, argsDetails, outDetails, actions);
    turn.el.append(root);
    card = { id, name, root, title, status, argsPre, rawArgs: "", outPre, outDetails, actions };
    turn.tools.set(id, card);
    turn.lastCard = card;
    return card;
  }

  function setStatus(card, text, tone) {
    card.status.textContent = text;
    if (tone) card.status.dataset.tone = tone;
    else delete card.status.dataset.tone;
  }

  function clearActions(card) {
    card.actions.replaceChildren();
    card.actions.hidden = true;
    card.waiting = false;
    turn?.pending.delete(card);
  }

  function showActions(card, nodes) {
    card.actions.replaceChildren(...nodes);
    card.actions.hidden = false;
    card.waiting = true;
    turn.pending.add(card);
  }

  function confirmation(ev, session) {
    const card = toolCard(ev.id, ev.name);
    card.rawArgs = ev.args ?? card.rawArgs;
    card.argsPre.textContent = prettyJSON(card.rawArgs);
    setStatus(card, "needs approval", "warn");
    const answer = async (decision, label) => {
      for (const b of card.actions.querySelectorAll("button")) b.disabled = true;
      try {
        await session.confirm({ decision, toolName: ev.name });
        clearActions(card);
        setStatus(card, decision === "reject" ? "rejected" : "approved", decision === "reject" ? "bad" : "ok");
      } catch (err) {
        clearActions(card);
        notice(`${label} failed: ${err.message ?? err}`, "warn");
      }
    };
    showActions(card, [
      button("Approve", "primary small", () => answer("approve", "Approve")),
      button("Always approve this tool", "small", () => answer("approve_tool", "Approve tool")),
      button("Reject", "danger small", () => answer("reject", "Reject")),
    ]);
  }

  function elicitation(ev, session) {
    flushRender();
    turn.cur = null;
    const root = el("div", "card");
    const head = el("div", "card-head");
    head.append(el("span", "card-title", "Input requested"), el("span", "badge", ev.mode || "form"));
    root.append(head, el("div", "elicit-msg", ev.message || ""));
    const grid = el("div", "elicit-grid");
    let codeInput;
    let stateInput;
    let contentArea;
    const href = ev.mode === "url" ? safeHref(ev.url) : null;
    if (ev.mode === "url") {
      if (href) {
        const a = el("a", "", href);
        a.href = href;
        a.target = "_blank";
        a.rel = "noopener noreferrer";
        grid.append(a);
      } else if (ev.url) {
        grid.append(el("code", "", String(ev.url)));
      }
      codeInput = el("input");
      codeInput.type = "text";
      codeInput.placeholder = "code (from the redirect URL, optional)";
      stateInput = el("input");
      stateInput.type = "text";
      stateInput.placeholder = "state (optional)";
      grid.append(codeInput, stateInput);
    } else {
      if (ev.schema) {
        const d = el("details");
        d.append(el("summary", "", "Schema"), el("pre", "", JSON.stringify(ev.schema, null, 2)));
        grid.append(d);
      }
      contentArea = el("textarea");
      contentArea.value = JSON.stringify(skeleton(ev.schema), null, 2);
      contentArea.spellcheck = false;
      contentArea.setAttribute("aria-label", "Response JSON");
      grid.append(contentArea);
    }
    const err = el("div", "inline-error");
    err.hidden = true;
    const actions = el("div", "actions");
    const card = { root, actions, status: head.lastChild, waiting: true };
    const respond = async (action) => {
      let content;
      if (action === "accept") {
        if (contentArea) {
          try {
            content = JSON.parse(contentArea.value || "{}");
          } catch (e) {
            err.textContent = `Invalid JSON: ${e.message}`;
            err.hidden = false;
            return;
          }
        } else if (codeInput.value || stateInput.value) {
          content = { code: codeInput.value, state: stateInput.value };
        }
      }
      for (const b of actions.querySelectorAll("button")) b.disabled = true;
      try {
        await session.respondToElicitation({ id: ev.id, action, content });
        card.waiting = false;
        turn?.pending.delete(card);
        actions.remove();
        setStatus(card, action === "accept" ? "accepted" : action === "decline" ? "declined" : "cancelled", action === "accept" ? "ok" : "");
      } catch (e) {
        err.textContent = String(e.message ?? e);
        err.hidden = false;
        for (const b of actions.querySelectorAll("button")) b.disabled = false;
      }
    };
    actions.append(
      button("Accept", "primary small", () => respond("accept")),
      button("Decline", "small", () => respond("decline")),
      button("Cancel", "danger small", () => respond("cancel")),
    );
    root.append(grid, err, actions);
    turn.el.append(root);
    turn.pending.add(card);
    card.actions = actions;
  }

  function usage(ev) {
    onUsage?.(ev);
  }

  return {
    clear() {
      turn = null;
      log.replaceChildren(el("p", "empty", "Configure an agent, add a key, and say hello."));
    },
    notice,
    userMessage(text) {
      append(el("div", "msg user", text));
    },
    beginTurn() {
      const node = el("div", "msg assistant");
      append(node);
      turn = { el: node, cur: null, tools: new Map(), pending: new Set(), lastCard: null };
    },
    handleEvent(ev, session) {
      if (!turn) return;
      const stick = nearBottom();
      switch (ev.type) {
        case "delta":
          if (ev.reasoning) appendPart("reasoning", ev.reasoning);
          if (ev.content) appendPart("text", ev.content);
          break;
        case "tool_call_delta": {
          const card = toolCard(ev.id || turn.lastCard?.id || "pending", ev.name);
          card.rawArgs += ev.arguments ?? "";
          card.argsPre.textContent = card.rawArgs;
          break;
        }
        case "tool_call": {
          const card = toolCard(ev.id, ev.name);
          card.rawArgs = ev.args ?? card.rawArgs;
          card.argsPre.textContent = prettyJSON(card.rawArgs);
          if (!turn.pending.has(card)) setStatus(card, "running", "");
          break;
        }
        case "tool_confirmation":
          confirmation(ev, session);
          break;
        case "tool_output": {
          const card = toolCard(ev.id, ev.name);
          card.outDetails.hidden = false;
          card.outPre.textContent += ev.output;
          break;
        }
        case "tool_result": {
          const card = toolCard(ev.id, ev.name);
          clearActions(card);
          card.outDetails.hidden = false;
          card.outPre.textContent = ev.output ?? "";
          card.root.classList.toggle("error", !!ev.is_error);
          setStatus(card, ev.is_error ? "error" : "done", ev.is_error ? "bad" : "ok");
          break;
        }
        case "tool_blocked": {
          const card = toolCard(ev.id, ev.name);
          clearActions(card);
          setStatus(card, "blocked", "bad");
          notice(`Tool ${ev.name} blocked: ${ev.reason}`, "warn");
          break;
        }
        case "elicitation":
          elicitation(ev, session);
          break;
        case "handoff":
          notice(`Handoff: ${ev.from} -> ${ev.to}`);
          break;
        case "fallback":
          notice(`Model fallback ${ev.from} -> ${ev.to} (attempt ${ev.attempt}): ${ev.reason}`, "warn");
          break;
        case "warning":
          notice(cleanError(ev.message), "warn");
          break;
        case "error":
          notice(cleanError(ev.message), "error");
          break;
        case "usage":
          usage(ev);
          break;
        default:
          break;
      }
      scrollIfNear(stick);
    },
    // Unanswered prompts can no longer be answered once the turn is over.
    endTurn() {
      flushRender();
      if (!turn) return;
      for (const card of [...turn.pending]) {
        if (card.actions) {
          card.actions.replaceChildren();
          card.actions.hidden = true;
        }
        if (card.waiting) setStatus(card, "cancelled", "");
      }
      turn.pending.clear();
      turn = null;
    },
  };
}
