// Run after build.sh:  node --test cmd/wasm/app/loader_test.mjs
// Boots the built dist/wasm-app/docker-agent.wasm.gz under Node the way
// node/boot.js does, then through loader.js over a local HTTP server.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";
import assert from "node:assert/strict";
import { performance } from "node:perf_hooks";
import { TextDecoder, TextEncoder } from "node:util";
import { webcrypto } from "node:crypto";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, "..", "..", "..");
const dist = process.env.OUT_DIR ? path.resolve(process.env.OUT_DIR) : path.join(root, "dist", "wasm-app");
const gzPath = path.join(dist, "docker-agent.wasm.gz");
const built = fs.existsSync(gzPath);
const skip = built ? false : `${gzPath} not found: run cmd/wasm/app/build.sh first`;

const require = createRequire(import.meta.url);

function installGoShim() {
  if (globalThis.Go) return;
  const goroot = process.env.GOROOT || execFileSync("go", ["env", "GOROOT"], { encoding: "utf8" }).trim();
  // Go's log writes go through fs.write; listAgents is a synchronous JS call,
  // so an asynchronous stderr write (Node's default) would deadlock the runtime.
  const syncFs = Object.assign({}, fs, {
    write(fd, buf, offset, length, position, callback) {
      try {
        callback(null, fs.writeSync(fd, buf, offset, length, position));
      } catch (err) {
        callback(err);
      }
    },
  });
  Object.assign(globalThis, { require, fs: syncFs, path, TextEncoder, TextDecoder });
  globalThis.performance ??= performance;
  globalThis.crypto ??= webcrypto;
  require(path.join(goroot, "lib", "wasm", "wasm_exec.js"));
}

async function waitForAgent() {
  for (let i = 0; i < 1500 && !globalThis.dockerAgent; i++) await new Promise((r) => setTimeout(r, 20));
  assert.ok(globalThis.dockerAgent, "dockerAgent global was never registered");
  return globalThis.dockerAgent;
}

const portableTeam = () => fs.readFileSync(path.join(root, "cmd", "wasm", "examples", "portable-team.yaml"), "utf8");

function serve({ blockGzip = false, stallGzip = false } = {}) {
  const types = { ".wasm": "application/wasm", ".js": "text/javascript", ".json": "application/json" };
  const server = http.createServer((req, res) => {
    const name = path.basename(new URL(req.url, "http://x").pathname);
    const file = path.join(dist, name);
    if (stallGzip && name.endsWith(".gz")) {
      res.writeHead(200, { "Content-Type": "application/gzip" });
      res.write(Buffer.from([0x1f, 0x8b]));
      return;
    }
    if ((blockGzip && name.endsWith(".gz")) || !fs.existsSync(file) || !fs.statSync(file).isFile()) {
      res.writeHead(404).end();
      return;
    }
    res.writeHead(200, { "Content-Type": types[path.extname(name)] ?? "application/octet-stream", "Content-Length": fs.statSync(file).size });
    fs.createReadStream(file).pipe(res);
  });
  return new Promise((resolve) =>
    server.listen(0, "127.0.0.1", () => {
      // close() would wait on the stalled response forever.
      server.close = ((close) => () => (server.closeAllConnections(), close.call(server)))(server.close);
      resolve(server);
    }),
  );
}

async function loadThroughLoader(mode, loaderOptions = {}) {
  delete globalThis.dockerAgent;
  const server = await serve(mode);
  try {
    const { loadDockerAgent } = await import(pathToFileURL(path.join(dist, "loader.js")));
    const base = `http://127.0.0.1:${server.address().port}/`;
    const progress = [];
    const api = await loadDockerAgent({ baseUrl: base, onProgress: (p) => progress.push(p), ...loaderOptions });
    return { api, progress };
  } finally {
    server.close();
  }
}

test("gunzip docker-agent.wasm.gz and list agents", { skip }, async () => {
  installGoShim();
  const compressed = fs.readFileSync(gzPath);
  const stream = new Blob([compressed]).stream().pipeThrough(new DecompressionStream("gzip"));
  const wasm = await new Response(stream).arrayBuffer();
  const go = new globalThis.Go();
  go.argv = ["docker-agent.wasm"];
  const { instance } = await WebAssembly.instantiate(wasm, go.importObject);
  go.run(instance);
  const api = await waitForAgent();
  const agents = api.listAgents(portableTeam());
  assert.ok(agents.length >= 1, "expected at least one agent");
  assert.ok(agents.some((a) => a.name === "root"));
});

test("loader.js boots the gzip build and reports progress", { skip }, async () => {
  installGoShim();
  const { api, progress } = await loadThroughLoader({ blockGzip: false });
  assert.ok(api.listAgents(portableTeam()).length >= 1);
  assert.ok(progress.length > 0 && progress.every((p) => p.source === "gzip"), "progress should come from the gzip download");
  assert.equal(progress.at(-1).received, progress.at(-1).total);
});

test("loader.js falls back to the plain wasm when the gzip is missing", { skip }, async () => {
  installGoShim();
  const { api, progress } = await loadThroughLoader({ blockGzip: true });
  assert.ok(api.listAgents(portableTeam()).length >= 1);
  assert.ok(progress.some((p) => p.source === "plain"));
});

test("loader.js falls back to the plain wasm when the gzip download stalls", { skip }, async () => {
  installGoShim();
  const { api, progress } = await loadThroughLoader({ stallGzip: true }, { stallMs: 1000 });
  assert.ok(api.listAgents(portableTeam()).length >= 1);
  assert.ok(progress.some((p) => p.source === "plain"));
});

if (!built) console.log(`SKIP: ${skip}`);
