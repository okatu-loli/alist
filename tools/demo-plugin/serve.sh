#!/usr/bin/env bash
# tools/demo-plugin/serve.sh
#
# Serves tools/demo-plugin/dist/ (built by pack.sh) as a static local plugin
# source at http://127.0.0.1:8765/ — index.json plus the demo-*.tar.gz
# packages. Point an imported plugin source's index_url at
# http://127.0.0.1:8765/index.json to exercise the install gateway end to
# end against a non-official, signed source.
set -euo pipefail

cd "$(dirname "$0")/dist" && python3 -m http.server 8765
