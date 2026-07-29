#!/usr/bin/env bash
# tools/demo-plugin/pack.sh
#
# Builds the demo-plugin binary, then packages+signs it at two versions
# (0.1.0, 0.2.0) into tools/demo-plugin/dist/, and writes a dist/index.json
# a local plugin source can serve (see serve.sh). Exercises the same hidden
# CLI (`go run . plugin genkey|sign|pack`) an official/community package
# builder would use.
#
# Output (all gitignored):
#   dist/plugin           - built demo-plugin gRPC binary
#   dist/mysource.ed25519 - throwaway ed25519 private key (hex), local only
#   dist/pubkey.hex       - the matching public key (hex), for importing as
#                           a plugin source's trust root
#   dist/pkg-<version>/   - staged package dirs (plugin.json + binary +
#                           checksums.txt + signature)
#   dist/demo-<version>.tar.gz - packaged, signed plugin archives
#   dist/index.json       - registry index listing both versions
set -euo pipefail

cd "$(dirname "$0")/../.."   # repo root
PLUGDIR="tools/demo-plugin"
DIST="$PLUGDIR/dist"

export GOFLAGS=-mod=mod

echo "==> building demo-plugin binary"
mkdir -p "$DIST"
go build -o "$DIST/plugin" "./$PLUGDIR"

echo "==> generating signing keypair"
GENOUT="$(go run . plugin genkey --out "$DIST/mysource.ed25519")"
echo "$GENOUT"
PUBKEY="$(printf '%s\n' "$GENOUT" | sed -n 's/^public key (embed in trustroot.go): //p')"
if [ -z "$PUBKEY" ]; then
  echo "pack.sh: could not parse public key from genkey output" >&2
  exit 1
fi
printf '%s' "$PUBKEY" > "$DIST/pubkey.hex"
echo "==> public key: $PUBKEY"

INDEX_INPUT="$DIST/.index-input"
: > "$INDEX_INPUT"

for V in 0.1.0 0.2.0; do
  PKGDIR="$DIST/pkg-$V"
  rm -rf "$PKGDIR"
  mkdir -p "$PKGDIR"
  cp "$DIST/plugin" "$PKGDIR/plugin"
  if [ "$V" = "0.1.0" ]; then
    cp "$PLUGDIR/plugin.json" "$PKGDIR/plugin.json"
  else
    cp "$PLUGDIR/plugin.v2.json" "$PKGDIR/plugin.json"
  fi

  echo "==> signing $V"
  go run . plugin sign --key "$DIST/mysource.ed25519" --dir "$PKGDIR"

  echo "==> packing $V"
  go run . plugin pack --dir "$PKGDIR" --out "$DIST/demo-$V.tar.gz"

  SHA="$(shasum -a 256 "$DIST/demo-$V.tar.gz" | awk '{print $1}')"
  SIG="$(tr -d '\n' < "$PKGDIR/signature")"
  echo "$V|$SHA|$SIG" >> "$INDEX_INPUT"
done

echo "==> writing index.json"
python3 - "$DIST" <<'PYEOF'
import json
import os
import sys

dist = sys.argv[1]
with open(os.path.join(dist, ".index-input")) as f:
    lines = [l.strip() for l in f if l.strip()]

entries = []
for line in lines:
    version, sha, sig = line.split("|", 2)
    entries.append({
        "id": "com.example.demoplugin",
        "name": "Demo Plugin",
        "version": version,
        "kind": "StorageDriver",
        "core_api": "^0.1.0",
        "sha256": sha,
        "url": "http://127.0.0.1:8765/demo-%s.tar.gz" % version,
        "sig": sig,
        "license": "MIT",
        "source_url": "https://github.com/example/demo-plugin",
        "pricing": "free",
    })

out_path = os.path.join(dist, "index.json")
with open(out_path, "w") as f:
    json.dump(entries, f, indent=2)
    f.write("\n")

print("wrote %d entries to %s" % (len(entries), out_path))
PYEOF

rm -f "$INDEX_INPUT"

echo "==> done"
ls -la "$DIST"
