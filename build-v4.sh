#!/usr/bin/env bash
# build-v4.sh — reproducible local build for AList v4.
#
# Unlike build.sh (v3), which fetches a prebuilt frontend tarball from the
# alist-org/alist-web / web-dist releases, v4's frontend is the sibling repo
# alist-v4-web — not (yet) published as a dist tarball. This script builds
# that repo from source and embeds its output into public/dist before
# building the Go binary (see public/public.go: `//go:embed all:dist`).
#
# Usage:
#   ./build-v4.sh          # Version = `git describe --tags` if any tag exists, else "dev"
#   ./build-v4.sh dev      # Version = "dev" (skips `git describe`)
#
# Env overrides:
#   ALIST_V4_WEB   path to the alist-v4-web checkout (default: ~/WebstormProjects/alist-v4-web)
#   GIT_AUTHOR     value baked into conf.GitAuthor (default: generic placeholder below)
#
# NOTE on CI / release workflows: none of .github/workflows/*.yml are touched
# by this script. A real v4 release pipeline would need build.yml (and the
# musl/xgo/android/freebsd release workflows) repointed from alist-org/alist-web
# to alist-v4-web's build output — out of scope here; those need CI runners
# and cross toolchains this script does not set up.
set -euo pipefail

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ALIST_V4_WEB="${ALIST_V4_WEB:-$HOME/WebstormProjects/alist-v4-web}"
APP_NAME="alist"

if [ ! -d "$ALIST_V4_WEB" ]; then
  echo "error: frontend repo not found at $ALIST_V4_WEB (set ALIST_V4_WEB to override)" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Version selection
# ---------------------------------------------------------------------------
builtAt="$(date +'%F %T %z')"
gitAuthor="${GIT_AUTHOR:-AList v4 contributors}"
gitCommit=$(git -C "$REPO_ROOT" log --pretty=format:"%h" -1)

if [ "${1:-}" = "dev" ]; then
  version="dev"
else
  version=$(git -C "$REPO_ROOT" describe --abbrev=0 --tags 2>/dev/null || echo "dev")
fi

# WebVersion: the frontend repo's own tag (if any), else its package.json version.
webVersion=$(git -C "$ALIST_V4_WEB" describe --tags --always 2>/dev/null || true)
if [ -z "$webVersion" ]; then
  webVersion=$(node -p "require('$ALIST_V4_WEB/package.json').version" 2>/dev/null || echo "dev")
fi

echo "backend version:  $version"
echo "frontend version: $webVersion"

ldflags="\
-w -s \
-X 'github.com/alist-org/alist/v3/internal/conf.BuiltAt=$builtAt' \
-X 'github.com/alist-org/alist/v3/internal/conf.GitAuthor=$gitAuthor' \
-X 'github.com/alist-org/alist/v3/internal/conf.GitCommit=$gitCommit' \
-X 'github.com/alist-org/alist/v3/internal/conf.Version=$version' \
-X 'github.com/alist-org/alist/v3/internal/conf.WebVersion=$webVersion' \
"

# ---------------------------------------------------------------------------
# Build the frontend
# ---------------------------------------------------------------------------
echo "==> building frontend ($ALIST_V4_WEB)"
(
  cd "$ALIST_V4_WEB"
  if [ ! -d node_modules ]; then
    echo "--> node_modules missing, running pnpm install --frozen-lockfile"
    pnpm install --frozen-lockfile
  fi
  pnpm build
)

echo "==> embedding frontend into public/dist"
rm -rf "$REPO_ROOT/public/dist"
cp -r "$ALIST_V4_WEB/dist" "$REPO_ROOT/public/dist"
# public/dist/* is gitignored except README.md and images/logo.svg (tracked
# placeholders kept so an empty checkout still has a valid embed.FS root).
# The `rm -rf` above deletes those tracked files from the working tree; put
# them back so `git status` stays clean after a local build (harmless if
# alist-v4-web's own dist also ships a README.md/images/logo.svg — the repo
# copies just take precedence).
if git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git -C "$REPO_ROOT" checkout -- public/dist/README.md public/dist/images/logo.svg 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# Build the backend
# ---------------------------------------------------------------------------
echo "==> building backend"
(
  cd "$REPO_ROOT"
  CGO_ENABLED=1 go build -o "$APP_NAME" -ldflags="$ldflags" -tags=jsoniter .
)

BIN_PATH="$REPO_ROOT/$APP_NAME"
echo "==> build complete: $BIN_PATH"
"$BIN_PATH" version
