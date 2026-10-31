// Tiny markdown subset (fenced code, inline code, paragraphs) built from DOM
// nodes only: model output never reaches innerHTML.

function inline(text, parent) {
  const re = /`([^`\n]+)`/g;
  let last = 0;
  for (let m = re.exec(text); m; m = re.exec(text)) {
    if (m.index > last) parent.append(text.slice(last, m.index));
    const code = document.createElement("code");
    code.textContent = m[1];
    parent.append(code);
    last = m.index + m[0].length;
  }
  if (last < text.length) parent.append(text.slice(last));
}

function codeBlock(lang, lines) {
  const pre = document.createElement("pre");
  if (lang) {
    const tag = document.createElement("span");
    tag.className = "lang";
    tag.textContent = lang;
    pre.append(tag);
  }
  const code = document.createElement("code");
  const body = lines.join("\n");
  code.textContent = body;
  pre.append(code);
  const copy = document.createElement("button");
  copy.type = "button";
  copy.className = "copy small ghost";
  copy.textContent = "Copy";
  copy.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(body);
      copy.textContent = "Copied";
    } catch {
      copy.textContent = "Failed";
    }
    setTimeout(() => (copy.textContent = "Copy"), 1200);
  });
  pre.append(copy);
  return pre;
}

// An unclosed fence (mid-stream) renders as a code block too.
export function renderMarkdown(text) {
  const frag = document.createDocumentFragment();
  let para = [];
  let fence = null;
  const flush = () => {
    if (!para.length) return;
    const p = document.createElement("p");
    inline(para.join("\n"), p);
    frag.append(p);
    para = [];
  };
  for (const line of text.split("\n")) {
    if (fence) {
      if (/^\s*```\s*$/.test(line)) {
        frag.append(codeBlock(fence.lang, fence.lines));
        fence = null;
      } else {
        fence.lines.push(line);
      }
      continue;
    }
    const open = /^\s*```\s*([\w+#.-]*)\s*$/.exec(line);
    if (open) {
      flush();
      fence = { lang: open[1], lines: [] };
    } else if (line.trim() === "") {
      flush();
    } else {
      para.push(line);
    }
  }
  flush();
  if (fence) frag.append(codeBlock(fence.lang, fence.lines));
  return frag;
}

const ANSI = /\x1b\[[0-9;]*m/g;
const CLI_HELP = "Provide them using any of these sources:";

// Go errors carry terminal colours and, for missing keys, CLI-only advice.
export function cleanError(message) {
  let text = String(message).replace(ANSI, "");
  const cut = text.indexOf(CLI_HELP);
  if (cut >= 0) text = `${text.slice(0, cut).trimEnd()}\nAdd it under Keys & settings (an env row, or Sign in with OpenRouter).`;
  return text;
}
