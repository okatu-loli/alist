#!/usr/bin/env bash
# tools/123open-plugin/pack.sh
#
# Builds the 123open-plugin binary, then packages+signs it into
# tools/123open-plugin/dist/, and writes a dist/index.json a local plugin
# source can serve (see serve.sh). Exercises the same hidden CLI
# (`go run . plugin genkey|sign|pack`) an official/community package builder
# would use.
#
# Output (all gitignored):
#   dist/plugin            - built 123open-plugin gRPC binary
#   dist/mysource.ed25519  - throwaway ed25519 private key (hex), local only
#   dist/pubkey.hex        - the matching public key (hex), for importing as
#                            a plugin source's trust root
#   dist/pkg-<version>/    - staged package dir (plugin.json + binary +
#                            i18n/ locale files + checksums.txt + signature)
#   dist/123open-<version>.tar.gz - packaged, signed plugin archive
#   dist/index.json        - registry index listing the version
set -euo pipefail

cd "$(dirname "$0")/../.."   # repo root
PLUGDIR="tools/123open-plugin"
DIST="$PLUGDIR/dist"

export GOFLAGS=-mod=mod

echo "==> building 123open-plugin binary"
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

for V in 0.1.0; do
  PKGDIR="$DIST/pkg-$V"
  rm -rf "$PKGDIR"
  mkdir -p "$PKGDIR"
  cp "$DIST/plugin" "$PKGDIR/plugin"
  cp "$PLUGDIR/plugin.json" "$PKGDIR/plugin.json"
  cp -R "$PLUGDIR/i18n" "$PKGDIR/i18n"

  echo "==> signing $V"
  go run . plugin sign --key "$DIST/mysource.ed25519" --dir "$PKGDIR"

  echo "==> packing $V"
  go run . plugin pack --dir "$PKGDIR" --out "$DIST/123open-$V.tar.gz"

  SHA="$(shasum -a 256 "$DIST/123open-$V.tar.gz" | awk '{print $1}')"
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
        "id": "cn.alist.123open",
        "name": "123 Open",
        "version": version,
        "kind": "StorageDriver",
        "core_api": "^0.1.0",
        "sha256": sha,
        "url": "http://127.0.0.1:8766/123open-%s.tar.gz" % version,
        "sig": sig,
        "license": "AGPL-3.0",
        "source_url": "https://github.com/alist-org/alist",
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
