#!/usr/bin/env bash
# vpsmgr install.sh — main entry, idempotent, run as root.
# Usage:
#   ./install.sh                  # default: download latest prebuilt release binary (fallback: local build)
#   ./install.sh --local-build    # force local Go compilation of the panel binary
#   ./install.sh --update         # force re-download of the prebuilt release binary over an existing one
#   ./install.sh --disable-v4forward  # install IPv6-only inbound policy; skip reserved-port checks
set -euo pipefail

cd "$(dirname "$0")"
ROOT="$PWD"

BUILD_MODE=release
DISABLE_V4FORWARD=0
for arg in "$@"; do
  case "$arg" in
    --local-build) BUILD_MODE=local ;;
    --update)      BUILD_MODE=update ;;
    --disable-v4forward) DISABLE_V4FORWARD=1 ;;
  esac
done
export VPSMGR_BUILD_MODE="$BUILD_MODE"
if [[ "$DISABLE_V4FORWARD" == "1" ]]; then
  # This explicit installer choice overrides an adopted config and any
  # VPSMGR_V4_FORWARD value inherited from the caller.  00-check.sh uses the
  # separate marker to skip checks for ports that vpsmgr will not expose.
  export VPSMGR_V4_FORWARD=0
  export VPSMGR_DISABLE_V4FORWARD=1
fi

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

if [[ "$DISABLE_V4FORWARD" == "1" ]]; then
  echo
  echo "!! --disable-v4forward: install with IPv4 inbound forwarding disabled !!"
  echo "   Containers will have no IPv4 SSH/port DNAT; Traefik will be installed but stopped."
  echo "   Reserved vpsmgr port checks will be skipped. You can re-enable later with:"
  echo "   vps config set net.v4_forward true"
  if [[ ! -t 0 ]]; then
    echo "error: --disable-v4forward requires an interactive confirmation" >&2
    exit 1
  fi
  read -r -p "Continue with IPv4 inbound disabled? [Y/n] " V4_CONFIRM
  case "$V4_CONFIRM" in
    ''|y|Y|yes|YES) ;;
    n|N|no|NO) echo "[install] aborted by user"; exit 1 ;;
    *) echo "error: please answer Y or n" >&2; exit 1 ;;
  esac
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

# --- swap (MANDATORY, asked before the IPv6 prompts) ---
# vpsmgr REQUIRES swap: without it a container memory spike OOMs the host
# instead of throttling. We decide this up front, before the IPv6 questions.
# If swap already exists we keep it. If not, we ask the user to create one and
# how many GiB; the DEFAULT is NO and declining aborts the install — a
# swap-less host is not supported.
SWAP_KB=$(awk '/SwapTotal/{print $2}' /proc/meminfo)
if [[ ${SWAP_KB:-0} -gt 0 ]]; then
  echo "[install] swap present: $(awk '/SwapTotal/{printf "%.1f GiB", $2/1024/1024}' /proc/meminfo)"
else
  echo "[install] no swap detected — vpsmgr requires swap."
  SWAP_DECLINE=0
  # 'y' answers with a 1:1 auto size: host RAM in GiB.
  AUTO_GIB=$(( $(awk '/MemTotal/{print $2}' /proc/meminfo) / 1024 / 1024 ))
  [[ $AUTO_GIB -lt 1 ]] && AUTO_GIB=1
  read -r -p "Add a swap file? Enter size in GiB, y for ${AUTO_GIB} GiB (host RAM 1:1), or N to abort [${AUTO_GIB} GiB/y/N]: " SWAP_ANS
  case "$SWAP_ANS" in
    ''|n|N|no|NO) SWAP_DECLINE=1 ;;
    y|Y|yes|YES) SWAP_GIB=$AUTO_GIB ;;
    *)
      if [[ "$SWAP_ANS" =~ ^[0-9]+$ ]] && [[ $SWAP_ANS -ge 1 ]]; then
        SWAP_GIB=$SWAP_ANS
      else
        echo "[install] invalid size '$SWAP_ANS'" >&2
        SWAP_DECLINE=1
      fi
      ;;
  esac
  if [[ $SWAP_DECLINE -eq 0 ]]; then
    SWAP_MB=$(( SWAP_GIB * 1024 ))
    if [[ ! -e /swapfile ]]; then
      echo "[install] creating ${SWAP_GIB} GiB swap file (this can take a moment)..."
      if ! fallocate -l "${SWAP_MB}M" /swapfile 2>/dev/null; then
        dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MB" status=none 2>/dev/null
      fi
      chmod 600 /swapfile
      mkswap /swapfile >/dev/null 2>&1
    fi
    if swapon /swapfile 2>/dev/null; then
      grep -q '^/swapfile' /etc/fstab 2>/dev/null || echo '/swapfile none swap sw 0 0' >> /etc/fstab
      echo "[install] swap enabled (${SWAP_GIB} GiB, persisted in /etc/fstab)"
    else
      echo "[install] error: could not enable /swapfile (filesystem may not support swap files) — install aborted" >&2
      exit 1
    fi
  fi
  if [[ $SWAP_DECLINE -eq 1 ]]; then
    echo
    echo "==================================================================="
    echo "ERROR: vpsmgr requires swap. Installation aborted."
    echo
    echo "vpsmgr 必须要有 swap。你可以创建并启用 swap 文件，或使用"
    echo "systemd-zram-generator 配置 zram 作为 swap（不占硬盘空间）。"
    echo "配置好适量 swap 后再运行安装程序即可。"
    echo
    echo "vpsmgr requires swap. Create and enable a swap file, or use"
    echo "systemd-zram-generator to configure zram as swap (no disk usage)."
    echo "Configure adequate swap, then re-run the installer."
    echo "==================================================================="
    exit 1
  fi
fi

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
  # 80/443 already in use (detected by 00-check): force net.traefik false for
  # the rest of the install — 30-traefik keeps the binary installed but stops
  # it, and `vps install` writes net.traefik: false. The marker is cleared at
  # the start of the next 00-check run.
  if [[ -f /etc/vpsmgr/.install-traefik-off ]]; then
    export VPSMGR_TRAEFIK=0
  fi
done
export VPSMGR_TRAEFIK="${VPSMGR_TRAEFIK:-}"

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
