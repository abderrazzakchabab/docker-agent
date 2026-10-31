#!/usr/bin/env bash
# Builds the standalone browser app: the wasm binary (plain, gzip, optional brotli),
# the static page and the bundled examples, ready for any static host.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
out="${OUT_DIR:-$root/dist/wasm-app}"
case "$out" in /*) ;; *) out="$PWD/$out" ;; esac

cd "$root"
rm -rf "$out"
mkdir -p "$out/examples"

echo "==> go build (js/wasm)"
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" ${WASM_TAGS:+-tags "$WASM_TAGS"} \
  -o "$out/docker-agent.wasm" ./cmd/wasm

# wasm_exec.js is tied to the compiler version and must match the binary.
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$out/wasm_exec.js"
cp "$here/index.html" "$here/style.css" "$here/app.js" "$here/loader.js" "$here/md.js" "$here/store.js" "$here/chat.js" "$here/settings.js" "$out/"
cp "$root/cmd/wasm/web/auth.js" "$out/auth.js"
cp "$root"/cmd/wasm/examples/*.yaml "$out/examples/"

# The page lists the examples from this file, so a host needs no directory listing.
{
  printf '['
  sep=''
  for f in "$out"/examples/*.yaml; do
    printf '%s"%s"' "$sep" "$(basename "$f" .yaml)"
    sep=','
  done
  printf ']\n'
} >"$out/examples/index.json"

# Fingerprint the binary so the loader can cache it as immutable and still
# pick up a redeploy.
if command -v sha256sum >/dev/null; then
  sum="$(sha256sum "$out/docker-agent.wasm" | cut -c1-16)"
else
  sum="$(shasum -a 256 "$out/docker-agent.wasm" | cut -c1-16)"
fi
printf '{"wasm":"%s"}\n' "$sum" >"$out/build.json"

echo "==> compress"
gzip -9 -k -f "$out/docker-agent.wasm"
# The loader never fetches the .br; it only helps hosts that negotiate
# Content-Encoding, and quality 11 takes minutes, so it is opt-in.
if [ -n "${BROTLI:-}" ]; then
  if command -v brotli >/dev/null; then
    brotli -q "${BROTLI_QUALITY:-11}" -k -f "$out/docker-agent.wasm"
  else
    echo "brotli not installed: skipping docker-agent.wasm.br"
  fi
fi

# Content-Encoding is deliberately left unset: the loader decompresses the
# .gz itself and falls back to the plain .wasm, so no host negotiation is needed.
cat >"$out/_headers" <<'HEADERS'
/docker-agent.wasm
  Content-Type: application/wasm
  Cache-Control: public, max-age=31536000, immutable
/docker-agent.wasm.gz
  Content-Type: application/gzip
  Cache-Control: public, max-age=31536000, immutable
/docker-agent.wasm.br
  Content-Type: application/octet-stream
  Cache-Control: public, max-age=31536000, immutable
/build.json
  Cache-Control: no-cache
HEADERS

echo "==> $out"
(cd "$out" && ls -l docker-agent.wasm* | awk '{printf "%12d  %s\n", $5, $9}')
echo "total: $(du -sh "$out" | cut -f1)"
