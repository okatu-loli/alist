#!/usr/bin/env bash
# tools/123open-plugin/serve.sh
#
# Serves tools/123open-plugin/dist/ (built by pack.sh) as a static local
# plugin source at http://127.0.0.1:8766/ — index.json plus the
# 123open-*.tar.gz package. Point an imported plugin source's index_url at
# http://127.0.0.1:8766/index.json to exercise the install gateway end to
# end against a non-official, signed source. Port 8766 avoids demo-plugin's
# 8765 so both can be served at once.
set -euo pipefail

cd "$(dirname "$0")/dist" && python3 -m http.server 8766
