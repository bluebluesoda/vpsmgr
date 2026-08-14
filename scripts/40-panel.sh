#!/usr/bin/env bash
# 40-panel.sh — install vpsmgr binary and initialize panel (config/cert/db/systemd).
# Binary source: prebuilt GitHub release by default, local Go build as fallback
# (or when VPSMGR_BUILD_MODE=local, e.g. via ./install.sh --local-build).
set -uo pipefail

log(){ echo "[40] $*"; }
die(){ echo "[40] error: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO="bluebluesoda/lxc-hosting"

ensure_go(){
  command -v go >/dev/null 2>&1 && return 0
  log "installing golang-go (needed for local build)..."
  apt-get update -qq || return 1
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq golang-go || return 1
  command -v go >/dev/null 2>&1
}

build_local(){
  ensure_go || die "go toolchain install failed"
  log "building vpsmgr from source..."
  bash "$ROOT/build.sh" || die "build failed"
  cp "$ROOT/bin/vps" /usr/local/bin/vps
  chmod 755 /usr/local/bin/vps
  log "installed /usr/local/bin/vps from source ($(/usr/local/bin/vps version))"
}

install_prebuilt(){
  local arch dir bin_url sum_url
  case "$(uname -m)" in
    x86_64)  arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) log "warn: no prebuilt binary for $(uname -m), falling back to local build"; return 1 ;;
  esac
  dir="$(mktemp -d /tmp/vpsmgr-dl.XXXXXX)"
  bin_url="https://github.com/$REPO/releases/latest/download/vps-$arch"
  sum_url="https://github.com/$REPO/releases/latest/download/SHA256SUMS"
  log "downloading prebuilt vpsmgr (linux/$arch) from GitHub releases..."
  log "  $bin_url"
  curl -fsSL --max-time 120 -o "$dir/vps-$arch" "$bin_url" \
    || { log "warn: binary download failed"; rm -rf "$dir"; return 1; }
  if curl -fsSL --max-time 30 -o "$dir/SHA256SUMS" "$sum_url"; then
    (cd "$dir" && sha256sum -c --ignore-missing --status SHA256SUMS) \
      || { log "warn: checksum mismatch (corrupt download?), falling back to local build"; rm -rf "$dir"; return 1; }
  else
    log "warn: could not fetch checksums, skipping verification"
  fi
  cp "$dir/vps-$arch" /usr/local/bin/vps
  chmod 755 /usr/local/bin/vps
  rm -rf "$dir"
  log "installed /usr/local/bin/vps from release ($(/usr/local/bin/vps version))"
}

if [[ "${VPSMGR_BUILD_MODE:-}" == "local" ]]; then
  # --local-build always rebuilds from this repo (never reuse an installed
  # stable binary), so what runs is exactly what the shown branch compiled.
  log "local build requested (--local-build)"
  build_local
elif [[ ! -x /usr/local/bin/vps ]]; then
  if install_prebuilt; then
    :
  else
    log "prebuilt install failed — falling back to local build"
    build_local
  fi
else
  log "vps already installed, skipping ($(/usr/local/bin/vps version))"
fi

log "running vps install (config/cert/db/nft/systemd)..."
# Capture the install output so the one-time admin password printed by a fresh
# `vps install` can be re-shown at the very end of the main installer.
INSTALL_OUT="${TMPDIR:-/tmp}/vpsmgr-install.out"
if /usr/local/bin/vps install 2>&1 | tee "$INSTALL_OUT"; then
  :
else
  rm -f "$INSTALL_OUT"
  exit 1
fi

echo "[40] panel ready"
