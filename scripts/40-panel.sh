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
  # Reuse a usable go on PATH (>= 1.21, toolchain auto-switch works) when one
  # already exists; otherwise install the official Go from go.dev directly.
  # No distro package: Debian 12's golang-go is 1.19 (can't even parse
  # go.mod's `toolchain` directive) and the official tarball is one curl away.
  if command -v go >/dev/null 2>&1; then
    if GO_VER=$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//'); then
      major=${GO_VER%%.*}; minor=${GO_VER#*.}; minor=${minor%%.*}
      if (( major > 1 || (major == 1 && minor >= 21) )); then
        log "go $GO_VER is new enough (>= 1.21, toolchain auto-switch works)"
        return 0
      fi
      log "go $GO_VER on PATH is too old (< 1.21) — installing official Go"
    fi
  fi
  # go.mod requires go >= 1.21 (it pins `toolchain go1.26.5` and relies on
  # auto toolchain download). Install the official Go from go.dev into
  # /usr/local/go and put it first on PATH for this installer's shell.
  log "installing official Go to /usr/local/go..."
  local arch tarball url
  case "$(uname -m)" in
    x86_64)  arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) die "no official Go for arch $(uname -m)" ;;
  esac
  # resolve the latest 1.26.x patch version from go.dev (returns e.g. go1.26.6)
  local latest
  latest=$(curl -fsSL --max-time 20 https://go.dev/VERSION?m=text 2>/dev/null | head -1) || latest="go1.26.5"
  tarball="/tmp/${latest}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${latest}.linux-${arch}.tar.gz"
  log "  downloading $url"
  curl -fsSL --max-time 300 -o "$tarball" "$url" || return 1
  rm -rf /usr/local/go && tar -C /usr/local -xzf "$tarball" || return 1
  rm -f "$tarball"
  export PATH="/usr/local/go/bin:$PATH"
  # Remember that WE installed this toolchain so build_local can remove it
  # (plus all build caches) right after the binary is produced.
  export GO_FROM_OFFICIAL=1
  command -v go >/dev/null 2>&1
}

# cleanup_go removes the go toolchain and every build cache (compiler cache,
# module cache including the auto-downloaded go1.26.5 toolchain) when the
# installer brought the toolchain itself. go is only needed to produce the
# binary — once bin/vps is in place it is multi-hundred-MB dead weight on a
# small host. If go pre-existed on the host it is left untouched.
cleanup_go(){
  [[ "${GO_FROM_OFFICIAL:-}" == "1" ]] || return 0
  log "removing go toolchain and build caches..."
  if command -v go >/dev/null 2>&1; then
    local modcache buildcache
    modcache=$(go env GOMODCACHE 2>/dev/null || echo /root/go/pkg/mod)
    buildcache=$(go env GOCACHE 2>/dev/null || echo /root/.cache/go-build)
    rm -rf /usr/local/go "$modcache" "$buildcache"
  else
    rm -rf /usr/local/go /root/go/pkg/mod /root/.cache/go-build
  fi
  unset GO_FROM_OFFICIAL
  log "go toolchain removed; host left clean"
}

build_local(){
  ensure_go || die "go toolchain install failed"
  log "building vpsmgr from source..."
  bash "$ROOT/build.sh" || die "build failed"
  # Replacing a running binary fails with ETXTBSY ("Text file busy") on an
  # adopted install where vps.service is already active. Stop the service
  # first, then copy, then let the installer re-enable it below.
  if systemctl is-active --quiet vps.service 2>/dev/null; then
    systemctl stop vps.service >/dev/null 2>&1 || true
    log "stopped vps.service to replace the running binary"
  fi
  if ! cp "$ROOT/bin/vps" /usr/local/bin/vps; then
    die "could not copy built binary to /usr/local/bin/vps"
  fi
  chmod 755 /usr/local/bin/vps
  log "installed /usr/local/bin/vps from source ($(/usr/local/bin/vps version))"
  # The toolchain was only needed to produce the binary — drop it and every
  # build cache now that bin/vps is in place (no-op when go pre-existed).
  cleanup_go
}

install_prebuilt(){
  local arch dir bin_url sum_url
  case "$(uname -m)" in
    x86_64)  arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) log "warn: no prebuilt binary for $(uname -m), falling back to local build"; return 1 ;;
  esac
  dir="$(mktemp -d /tmp/vpsmgr-dl.XXXXXX)"
  trap 'rm -rf "$dir"' EXIT
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
# `vps install` can be re-shown at the very end of the main installer. The
# file lives under /etc/vpsmgr (root-only) with 0600, NOT a fixed /tmp path a
# local attacker could pre-create or read (review P2-9 — it carries a one-time
# admin password).
INSTALL_OUT=/etc/vpsmgr/.last-install.out
rm -f "$INSTALL_OUT"
# `vps install` creates /etc/vpsmgr itself — make sure it exists before tee
# opens the capture file, or the pipeline fails on a fresh host.
install -d -m 0755 /etc/vpsmgr
if /usr/local/bin/vps install 2>&1 | tee "$INSTALL_OUT"; then
  chmod 600 "$INSTALL_OUT"
else
  rm -f "$INSTALL_OUT"
  exit 1
fi

echo "[40] panel ready"
