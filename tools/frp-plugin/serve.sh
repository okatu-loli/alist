#!/usr/bin/env bash
# tools/frp-plugin/serve.sh
#
# Serves tools/frp-plugin/dist/ (built by pack.sh) as a static local plugin
# source at http://127.0.0.1:8767/ — index.json plus the frp-*.tar.gz
# packages. Point an imported plugin source's index_url at
# http://127.0.0.1:8767/index.json (pubkey = dist/pubkey.hex) to exercise the
# install gateway end to end against a non-official, signed 🟡 source. The
# port matches the url baked into dist/index.json by pack.sh.
set -euo pipefail

cd "$(dirname "$0")/dist" && python3 -m http.server 8767
