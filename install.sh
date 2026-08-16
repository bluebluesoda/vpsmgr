#!/usr/bin/env bash
# vpsmgr install.sh — main entry, idempotent, run as root.
# Usage:
#   ./install.sh                  # default: download latest prebuilt release binary (fallback: local build)
#   ./install.sh --local-build    # force local Go compilation of the panel binary
#   ./install.sh --update         # force re-download of the prebuilt release binary over an existing one
set -euo pipefail

cd "$(dirname "$0")"
ROOT="$PWD"

BUILD_MODE=release
for arg in "$@"; do
  case "$arg" in
    --local-build) BUILD_MODE=local ;;
    --update)      BUILD_MODE=update ;;
  esac
done
export VPSMGR_BUILD_MODE="$BUILD_MODE"

# Storage backend: zfs (default) or dir. dir has no quotas/snapshots/clones —
# only meant for throwaway test boxes. Never fall back automatically.
export VPSMGR_STORAGE="${VPSMGR_STORAGE:-zfs}"
case "$VPSMGR_STORAGE" in
  zfs|dir) ;;
  *) echo "error: VPSMGR_STORAGE must be zfs or dir (got '$VPSMGR_STORAGE')" >&2; exit 1 ;;
esac

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (sudo ./install.sh)" >&2
  exit 1
fi

# Local build: make it obvious WHICH branch will be compiled, and give the user
# a chance to abort — some people want a dev build but end up building stable.
if [[ "$BUILD_MODE" == "local" ]]; then
  BRANCH=$(git -C "$ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "(no git repo / unknown)")
  echo
  echo "!! --local-build: compiling vpsmgr from this repository — branch: $BRANCH !!"
  echo "   Install starts in 10 seconds; Ctrl-C now if this is not the branch you intended."
  sleep 10
fi

# Reinstall after a non-purging uninstall: /etc/vpsmgr survives, adopt the
# previous users/domains/settings instead of starting over.
if [[ -f /etc/vpsmgr/config.yaml ]]; then
  echo "[install] found existing /etc/vpsmgr/config.yaml — adopting previous setup"
fi

echo "==> vpsmgr installer starting (panel binary mode: $BUILD_MODE, storage: $VPSMGR_STORAGE)"
echo
echo "===== 00-ip-ask ====="
# shellcheck disable=SC1090
source "$ROOT/scripts/00-ip-ask.sh" || { echo "error: install-time network asks failed — aborting" >&2; exit 1; }
export VPSMGR_IPV6_SUBNET="${VPSMGR_IPV6_SUBNET:-}"
export VPSMGR_IPV4_SUBNET="${VPSMGR_IPV4_SUBNET:-}"
export VPSMGR_IPV6_MODE="${VPSMGR_IPV6_MODE:-}"
export VPSMGR_IPV6_POOL="${VPSMGR_IPV6_POOL:-}"

# NDP proxy for IPv6 pass-through. Prefix mode: each container owns a /112
# block and ndppd relays neighbor discovery on the upstream interface for it.
# Pool mode: kernel proxy_ndp handles each /128 (no ndppd needed). Only
# needed in prefix mode; small, no data-plane involvement.
if [[ "${VPSMGR_IPV6_MODE:-}" == "prefix" && -n "${VPSMGR_IPV6_SUBNET:-}" ]]; then
  echo
  echo "===== installing ndppd (IPv6 NDP proxy) ====="
  apt-get update -qq 2>/dev/null || true
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ndppd
  systemctl enable ndppd.service >/dev/null 2>&1 || true
fi

for step in 00-check 10-incus 20-network 30-traefik 40-panel 50-image; do
  echo
  echo "===== $step ====="
  bash "$ROOT/scripts/$step.sh"
done

echo
echo "===== cleaning apt cache ====="
apt-get clean 2>/dev/null || true
rm -rf /var/lib/apt/lists/* 2>/dev/null || true

echo
echo "===== install complete ====="
if command -v vps >/dev/null 2>&1; then
  echo "panel address:"
  vps panel-url
  # On a FRESH install `vps install` printed the one-time admin password
  # mid-install (captured by 40-panel.sh); re-show it here so it cannot be
  # missed. On adoption/upgrade nothing is printed (no new password exists).
  INSTALL_OUT=/etc/vpsmgr/.last-install.out
  if [ -s "$INSTALL_OUT" ]; then
    echo
    grep -E "admin password|admin panel initialized" "$INSTALL_OUT" || true
    rm -f "$INSTALL_OUT"
  fi
  echo "forgot the admin password? run: vps admin-passwd"
fi
echo "try: vps add alice"
echo "     ssh -p <base> root@<public-ip>"
