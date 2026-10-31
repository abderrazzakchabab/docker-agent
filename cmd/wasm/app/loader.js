// Boots docker-agent.wasm and resolves with globalThis.dockerAgent.
// Needs wasm_exec.js (classic script) to have defined the global `Go`.

const GZIP_MAGIC = [0x1f, 0x8b];
const WASM_MAGIC = [0x00, 0x61, 0x73, 0x6d];

const startsWith = (bytes, magic) => magic.every((b, i) => bytes[i] === b);

// Aborts the download once no bytes arrive for stallMs, so a stalled host
// fails over (or errors) instead of hanging the page forever.
function watchdog(url, stallMs) {
  const ctrl = new AbortController();
  let timer;
  const kick = () => {
    clearTimeout(timer);
    timer = setTimeout(() => ctrl.abort(new Error(`${url}: no data for ${stallMs / 1000}s`)), stallMs);
  };
  kick();
  return { signal: ctrl.signal, kick, stop: () => clearTimeout(timer) };
}

async function fetchChecked(url, dog) {
  const res = await fetch(url, { signal: dog.signal });
  if (!res.ok) throw new Error(`${url}: HTTP ${res.status}`);
  return res;
}

// Reads the body while reporting progress. The total is only trusted when
// the body was not content-decoded on the way (Content-Length would not match).
async function readWithProgress(res, dog, onProgress) {
  const encoded = res.headers.has("Content-Encoding");
  const length = Number(res.headers.get("Content-Length"));
  const total = !encoded && length > 0 ? length : 0;
  const chunks = [];
  let received = 0;
  const reader = res.body.getReader();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    dog.kick();
    chunks.push(value);
    received += value.length;
    onProgress?.({ received, total });
  }
  const bytes = new Uint8Array(received);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.length;
  }
  return bytes;
}

async function gunzip(bytes) {
  const stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("gzip"));
  return new Response(stream).arrayBuffer();
}

// Some hosts add Content-Encoding: gzip to *.gz files, so fetch has already
// decoded the body; the magic bytes tell the two cases apart.
async function instantiateGzip(url, importObject, stallMs, onProgress) {
  const dog = watchdog(url, stallMs);
  let bytes;
  try {
    bytes = await readWithProgress(await fetchChecked(url, dog), dog, onProgress);
  } finally {
    dog.stop();
  }
  let wasm = bytes;
  if (startsWith(bytes, GZIP_MAGIC)) wasm = new Uint8Array(await gunzip(bytes));
  else if (!startsWith(bytes, WASM_MAGIC)) throw new Error(`${url}: not gzip or wasm data`);
  return (await WebAssembly.instantiate(wasm, importObject)).instance;
}

async function instantiatePlain(url, importObject, stallMs, onProgress) {
  if (typeof WebAssembly.instantiateStreaming === "function") {
    const dog = watchdog(url, stallMs);
    try {
      const res = await fetchChecked(url, dog);
      const total = Number(res.headers.get("Content-Length")) || 0;
      let received = 0;
      const counted = res.body.pipeThrough(
        new TransformStream({
          transform(chunk, controller) {
            dog.kick();
            received += chunk.length;
            onProgress?.({ received, total: res.headers.has("Content-Encoding") ? 0 : total });
            controller.enqueue(chunk);
          },
        }),
      );
      // Re-wrapping forces the MIME type instantiateStreaming insists on.
      const wrapped = new Response(counted, { headers: { "Content-Type": "application/wasm" } });
      return (await WebAssembly.instantiateStreaming(wrapped, importObject)).instance;
    } catch (err) {
      if (dog.signal.aborted) throw dog.signal.reason;
      console.warn("instantiateStreaming failed, retrying with instantiate:", err);
    } finally {
      dog.stop();
    }
  }
  const dog = watchdog(url, stallMs);
  try {
    const bytes = await readWithProgress(await fetchChecked(url, dog), dog, onProgress);
    return (await WebAssembly.instantiate(bytes, importObject)).instance;
  } finally {
    dog.stop();
  }
}

async function buildSuffix(base) {
  try {
    const res = await fetch(new URL("build.json", base), { cache: "no-cache" });
    if (!res.ok) return "";
    const { wasm } = await res.json();
    return wasm ? `?v=${encodeURIComponent(wasm)}` : "";
  } catch {
    return "";
  }
}

function waitForGlobal(timeoutMs, exited) {
  return new Promise((resolve, reject) => {
    const deadline = Date.now() + timeoutMs;
    const poll = () => {
      if (globalThis.dockerAgent) return resolve(globalThis.dockerAgent);
      if (exited.error) return reject(exited.error);
      if (Date.now() > deadline) {
        return reject(new Error(`docker-agent did not register globalThis.dockerAgent within ${timeoutMs / 1000}s`));
      }
      setTimeout(poll, 20);
    };
    poll();
  });
}

// options: {baseUrl?, timeoutMs?, stallMs?, onProgress?({received, total, source}), onStatus?(text)}
export async function loadDockerAgent({
  baseUrl = import.meta.url,
  timeoutMs = 30000,
  stallMs = 30000,
  onProgress,
  onStatus,
} = {}) {
  if (globalThis.dockerAgent) return globalThis.dockerAgent;
  if (typeof globalThis.Go !== "function") {
    throw new Error("wasm_exec.js is not loaded: the Go runtime shim (globalThis.Go) is missing");
  }
  const suffix = await buildSuffix(baseUrl);
  const go = new globalThis.Go();
  go.argv = ["docker-agent.wasm"];

  let instance;
  const gzURL = new URL(`docker-agent.wasm.gz${suffix}`, baseUrl);
  const plainURL = new URL(`docker-agent.wasm${suffix}`, baseUrl);
  try {
    onStatus?.("Downloading docker-agent.wasm.gz");
    instance = await instantiateGzip(gzURL, go.importObject, stallMs, (p) => onProgress?.({ ...p, source: "gzip" }));
  } catch (err) {
    console.warn("gzip load failed, falling back to the plain binary:", err);
    onStatus?.("Downloading docker-agent.wasm");
    instance = await instantiatePlain(plainURL, go.importObject, stallMs, (p) => onProgress?.({ ...p, source: "plain" }))
      .catch((plainErr) => {
        throw new Error(`could not load docker-agent.wasm (${plainErr.message}); gzip attempt: ${err.message}`);
      });
  }

  onStatus?.("Starting runtime");
  // main() blocks forever, so run() only settles if the Go program exits.
  const exited = {};
  go.run(instance).then(
    () => (exited.error = new Error("the Go program exited before registering dockerAgent")),
    (err) => (exited.error = err),
  );
  return waitForGlobal(timeoutMs, exited);
}
