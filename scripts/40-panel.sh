#!/usr/bin/env bash
# 40-panel.sh — install vpsmgr binary and initialize panel (config/cert/db/systemd).
# Binary source: prebuilt GitHub release by default, local Go build as fallback
# (or VPSMGR_BUILD_MODE=local via --local-build; VPSMGR_BUILD_MODE=update via
# --update forces a fresh prebuilt download over an existing binary).
set -uo pipefail

log(){ echo "[40] $*"; }
die(){ echo "[40] error: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO="bluebluesoda/vpsmgr"

ensure_go(){
  # Reuse a usable go on PATH (>= 1.21, toolchain auto-switch works) when one
  # already exists; otherwise install the official Go from go.dev directly.
  # No distro package: Debian 12's golang-go is 1.19 (can't even parse
  # go.mod's `toolchain` directive) and the official tarball is one curl away.
  #
  # Some hosts resolve go.dev/dl through an IPv6 path that returns 404 for the
  # download files (the site and manifest work, the tarball CDN does not), so
  # every fetch below first tries the default address family and falls back to
  # forcing IPv4 (`curl -4`) on failure.
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
  # curl_retry: fetch a URL, trying the default address family first (works on
  # normal hosts and IPv6-first ones) and falling back to forcing IPv4 when the
  # IPv6 path is broken. Args: <max-time> <url> <outfile>.
  curl_retry(){
    local max_time="$1" url="$2" out="$3"
    if curl -fsSL --max-time "$max_time" -o "$out" "$url" 2>/dev/null; then
      return 0
    fi
    rm -f "$out"
    log "  IPv6 path failed, retrying via IPv4: $url"
    curl -4 -fsSL --max-time "$max_time" -o "$out" "$url" 2>/dev/null
  }
  # Read the official stable-download manifest instead of trusting the
  # VERSION endpoint alone. During a new release, VERSION can briefly name a
  # version whose architecture tarball is not available through every CDN
  # edge yet, which otherwise turns a transient 404 into a failed install.
  local candidates candidate filename manifest
  manifest="/tmp/go-manifest-$$.json"
  if curl_retry 30 "https://go.dev/dl/?mode=json" "$manifest"; then
    mapfile -t candidates < <(python3 -c 'import json,sys
try:
    releases=json.load(open(sys.argv[1]))
except Exception:
    sys.exit(1)
for release in releases:
    for f in release.get("files", []):
        if f.get("os") == "linux" and f.get("arch") == sys.argv[2] and f.get("kind") == "archive":
            print(release["version"] + " " + f["filename"])
            break' "$manifest" "$arch")
    rm -f "$manifest"
  fi
  # Keep a known previous toolchain as a last-resort candidate if the manifest
  # endpoint is temporarily unavailable. The download loop below still retries
  # every candidate before failing.
  if [[ ${#candidates[@]} -eq 0 ]]; then
    candidates=("go1.26.5 go1.26.5.linux-${arch}.tar.gz")
  fi
  local downloaded=0
  for candidate in "${candidates[@]}"; do
    filename="${candidate#* }"
    tarball="/tmp/${filename}"
    url="https://go.dev/dl/${filename}"
    for attempt in 1 2 3; do
      log "  downloading $url (attempt $attempt/3)"
      if curl_retry 300 "$url" "$tarball"; then
        downloaded=1
        break 2
      fi
      rm -f "$tarball"
    done
    log "  warn: $filename was unavailable; trying the previous stable release"
  done
  [[ "$downloaded" -eq 1 ]] || return 1
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

# stop_holders stops every vpsmgr unit that keeps /usr/local/bin/vps mapped
# (running), so the binary can be replaced without ETXTBSY ("Text file busy").
# vps.service is the panel; on the dev-ndp branch vps-ipv6.service is a
# long-lived `vps ipv6-proxy` process that ALSO maps the binary — stopping only
# the panel leaves it holding the file and cp then fails. Unconditional:
# stopping a unit that is not present is a harmless no-op.
stop_holders(){
  systemctl stop vps.service >/dev/null 2>&1 || true
  systemctl stop vps-ipv6.service >/dev/null 2>&1 || true
  log "stopped services holding the vps binary"
}

build_local(){
  ensure_go || die "go toolchain install failed"
  log "building vpsmgr from source..."
  bash "$ROOT/build.sh" || die "build failed"
  # Replacing a running binary fails with ETXTBSY ("Text file busy") on an
  # adopted install where a vpsmgr unit is already active. Stop the services
  # that map the binary first, then copy, then let the installer re-enable.
  stop_holders
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
  # EXIT trap for cleanup. Must NOT reference the local $dir: the trap string
  # is evaluated when the script EXITS, after the function returned and $dir
  # went out of scope — under `set -u` that would raise "unbound variable"
  # (and skip the cleanup) at the very end of a successful install. Globbing
  # the unique mktemp prefix is local-agnostic and only ever matches this dir.
  trap 'rm -rf /tmp/vpsmgr-dl.*' EXIT
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
  # Replace a running binary (ETXTBSY): stop the units that map it first,
  # then copy, then let the installer re-enable them below. This runs only
  # after the download verified, so a failed download never takes the panel
  # down.
  stop_holders
  cp "$dir/vps-$arch" /usr/local/bin/vps
  chmod 755 /usr/local/bin/vps
  log "installed /usr/local/bin/vps from release ($(/usr/local/bin/vps version))"
}

install_completions(){
  # Bash tab completion for the `vps` CLI: `vps config set <TAB>` completes
  # config keys (read live from `vps config completions` so they match the
  # registry), plus top-level commands and container names. Installed into the
  # Debian/Ubuntu bash-completion load path; sourced automatically in a new
  # shell. Non-fatal: if the dir is unavailable the binary still works.
  local dest="/usr/share/bash-completion/completions/vps"
  local src="$ROOT/configs/completions/vps.bash"
  if ! install -d -m 0755 "$(dirname "$dest")" 2>/dev/null; then
    log "warn: could not create bash-completion dir; skipping CLI completion"
    return 0
  fi
  if cp "$src" "$dest" && chmod 644 "$dest"; then
    log "installed shell completion: $dest"
  else
    log "warn: could not install shell completion to $dest"
  fi
}

if [[ "${VPSMGR_BUILD_MODE:-}" == "local" ]]; then
  # --local-build always rebuilds from this repo (never reuse an installed
  # stable binary), so what runs is exactly what the shown branch compiled.
  log "local build requested (--local-build)"
  build_local
elif [[ "${VPSMGR_BUILD_MODE:-}" == "update" ]]; then
  # --update forces a re-download of the latest prebuilt release over whatever
  # is installed. Conservative: on a download/checksum failure the existing
  # binary is kept (it was never touched), not replaced by a local build.
  log "update requested (--update) — replacing the installed binary with the latest prebuilt release"
  if ! install_prebuilt; then
    log "warn: prebuilt update failed — keeping the installed binary ($(/usr/local/bin/vps version))"
  fi
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

install_completions

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
