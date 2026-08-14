#!/usr/bin/env bash
# vpsmgr install.sh — main entry, idempotent, run as root.
# Usage:
#   ./install.sh                  # default: download latest prebuilt release binary (fallback: local build)
#   ./install.sh --local-build    # force local Go compilation of the panel binary
set -euo pipefail

cd "$(dirname "$0")"
ROOT="$PWD"

BUILD_MODE=release
if [[ "${1:-}" == "--local-build" ]]; then
  BUILD_MODE=local
fi
export VPSMGR_BUILD_MODE="$BUILD_MODE"

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (sudo ./install.sh)" >&2
  exit 1
fi

# The Incus-based vpsmgr is a new product line. A leftover binary from the old
# LXD-based v0.2.x/early-v0.3 line is not compatible — refuse to run over it
# so the box cannot end up half-upgraded. A non-purging uninstall of the old
# line leaves no binary (uninstall.sh removes it), so this only fires when the
# old panel was never uninstalled.
if [[ -x /usr/local/bin/vpsmgr ]] && ! /usr/local/bin/vpsmgr version 2>/dev/null | grep -q "0.3"; then
  echo "error: an old LXD-based vpsmgr binary is still installed." >&2
  echo "       this installer uses Incus and cannot run over it." >&2
  echo "       uninstall the old version first (bash uninstall.sh --purge)." >&2
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

echo "==> vpsmgr installer starting (panel binary mode: $BUILD_MODE)"
echo
echo "===== 00-ip-ask ====="
# shellcheck disable=SC1090
source "$ROOT/scripts/00-ip-ask.sh" || { echo "error: install-time network asks failed — aborting" >&2; exit 1; }
export VPSMGR_IPV6_SUBNET="${VPSMGR_IPV6_SUBNET:-}"
export VPSMGR_IPV4_SUBNET="${VPSMGR_IPV4_SUBNET:-}"

# NDP proxy for IPv6 pass-through: each container owns a /112 block and ndppd
# relays neighbor discovery on the upstream interface for it, so every address
# a container binds is reachable from the internet. Only needed when IPv6 is
# enabled; small, no data-plane involvement.
if [[ -n "${VPSMGR_IPV6_SUBNET:-}" ]]; then
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
  INSTALL_OUT="${TMPDIR:-/tmp}/vpsmgr-install.out"
  if [ -s "$INSTALL_OUT" ]; then
    echo
    grep -E "admin password|admin panel initialized" "$INSTALL_OUT" || true
    rm -f "$INSTALL_OUT"
  fi
  echo "forgot the admin password? run: vps admin-passwd"
fi
echo "try: vps add alice"
echo "     ssh -p <base> root@<public-ip>"
