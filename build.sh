#!/usr/bin/env bash
# build.sh — compile the vpsmgr Go binary (CLI name `vps`) into ./bin. Requires any Go that
# supports toolchain auto-switch (>= 1.21); go.mod pins the go/toolchain
# version (currently go1.26.5), so building always uses that exact release.
# Usage: ./build.sh [VERSION]          # VERSION defaults to the source default
#        ./build.sh v0.1.0             # strip leading 'v' and inject version
#        GOOS=linux GOARCH=arm64 ./build.sh  # cross-compile (CGO_ENABLED=0)
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$PWD"

log(){ echo "[build] $*"; }

if ! command -v go >/dev/null 2>&1; then
  log "error: go not found"
  log "run ./install.sh (installs golang-go) or: apt-get install -y golang-go"
  exit 1
fi

GOOS="${GOOS:-$(go env GOOS)}"
GOARCH="${GOARCH:-$(go env GOARCH)}"
VERSION="${1:-}"
if [[ -n "$VERSION" ]]; then
  VERSION="${VERSION#v}"   # strip leading 'v' (v0.1.0 -> 0.1.0)
else
  # Local/dev build: identify the exact commit so bug reports are reproducible.
  # Format = nearest tag + short sha + UTC timestamp, e.g.
  #   0.2.4-a8934ba9-2608120952
  #   0.2.4-a8934ba9-dirty-2608120952   (uncommitted changes present)
  # Building exactly on a tag uses the tag (matches a release build). The
  # timestamp sorts builds by time, which future upgrade logic can rely on.
  TS=$(date -u +%y%m%d%H%M)
  SHA="nosha"
  DIRTY=""
  NEAREST="0.0.0"
  if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    SHA=$(git -C "$ROOT" rev-parse --short=8 HEAD 2>/dev/null || echo nosha)
    git -C "$ROOT" diff --quiet 2>/dev/null || DIRTY="-dirty"
    if TAG=$(git -C "$ROOT" describe --tags --exact-match HEAD 2>/dev/null); then
      VERSION="${TAG#v}${DIRTY}"
    else
      NEAREST=$(git -C "$ROOT" describe --tags --abbrev=0 HEAD 2>/dev/null || echo "0.0.0")
      VERSION="${NEAREST#v}-${SHA}${DIRTY}-${TS}"
    fi
  else
    VERSION="${NEAREST}-${SHA}-${TS}"
  fi
fi

log "go $( (cd src && go version) | awk '{print $3}') os=${GOOS} arch=${GOARCH} version=${VERSION:-<source default>}"
mkdir -p bin

LDFLAGS="-s -w"
if [[ -n "$VERSION" ]]; then
  LDFLAGS="$LDFLAGS -X vpsmgr/internal/ver.Version=$VERSION"
fi

OUT="$ROOT/bin/vps"
[[ "$GOOS" == "windows" ]] && OUT="$OUT.exe"
(cd src && CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build \
  -trimpath -buildvcs=false \
  -ldflags="$LDFLAGS" \
  -o "$OUT" .)
log "built bin/vps"

if [[ "$GOOS" == "$(go env GOOS)" && "$GOARCH" == "$(go env GOARCH)" ]]; then
  log "version: $("$OUT" version 2>/dev/null || true)"
fi
