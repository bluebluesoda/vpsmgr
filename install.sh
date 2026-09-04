#!/usr/bin/env bash
# vpsmgr install.sh — main entry, idempotent, run as root.
# Usage:
#   ./install.sh                  # default: download latest prebuilt release binary (fallback: local build)
#   ./install.sh --local-build    # force local Go compilation of the panel binary
#   ./install.sh --update         # force re-download of the prebuilt release binary over an existing one
#   ./install.sh --disable-v4forward  # install IPv6-only inbound policy; skip reserved-port checks
#   ./install.sh --zfs-prealloc  # INTERNAL TESTING: preallocate the zfs backing file (oversold hosts)
set -euo pipefail

cd "$(dirname "$0")"
ROOT="$PWD"

BUILD_MODE=release
DISABLE_V4FORWARD=0
ZFS_PREALLOC=0
for arg in "$@"; do
  case "$arg" in
    --local-build) BUILD_MODE=local ;;
    --update)      BUILD_MODE=update ;;
    --disable-v4forward) DISABLE_V4FORWARD=1 ;;
    --zfs-prealloc) ZFS_PREALLOC=1 ;;
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

# Storage backend: zfs (default), btrfs, or dir. zfs and btrfs both provide
# quotas/snapshots/clone-on-create; dir has none and is only meant for
# throwaway test boxes. Never fall back automatically.
VPSMGR_STORAGE_SET="${VPSMGR_STORAGE:-}"
export VPSMGR_STORAGE="${VPSMGR_STORAGE:-zfs}"
case "$VPSMGR_STORAGE" in
  zfs|btrfs|dir) ;;
  *) echo "error: VPSMGR_STORAGE must be zfs, btrfs or dir (got '$VPSMGR_STORAGE')" >&2; exit 1 ;;
esac

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (sudo ./install.sh)" >&2
  exit 1
fi

# --- storage backend on a btrfs root filesystem ---
# A ZFS pool cannot be safely created on a btrfs root: its backing loop file
# would sit on a copy-on-write filesystem, which corrupts the pool. On such a
# host btrfs is the only full-featured backend (a native subvolume), so:
#   - VPSMGR_STORAGE unset (default zfs) -> warn and switch to btrfs after an
#     explicit confirmation (this doubles as the beta confirmation);
#   - explicit VPSMGR_STORAGE=zfs        -> hard abort (no ZFS on btrfs);
#   - explicit btrfs / dir               -> unchanged (dir stays the explicit
#     test-box opt-in, with a note).
# Non-btrfs roots never reach this block; a plain `./install.sh` there behaves
# exactly as before.
BTRFS_CONFIRMED=0
ROOTFS="$(findmnt -no FSTYPE / 2>/dev/null)" || ROOTFS=""
if [[ "$ROOTFS" == "btrfs" ]]; then
  if [[ -z "$VPSMGR_STORAGE_SET" ]]; then
    echo
    echo "!! This host's root filesystem is BTRFS !!"
    echo "   A ZFS pool cannot be created on a btrfs root — its backing loop file"
    echo "   would sit on a copy-on-write filesystem and corrupt the pool. The"
    echo "   installer will use the Btrfs (beta) storage backend instead, as a"
    echo "   native subvolume."
    if [[ ! -t 0 ]]; then
      echo "[install] non-interactive — switching to the Btrfs (beta) storage backend (btrfs root)"
    else
      read -r -p "Switch to the Btrfs (beta) storage backend? [Y/n] " BTRFS_CONFIRM
      case "$BTRFS_CONFIRM" in
        ''|y|Y|yes|YES) ;;
        n|N|no|NO) echo "[install] aborted by user (declined the Btrfs backend)"; exit 1 ;;
        *) echo "error: please answer Y or n" >&2; exit 1 ;;
      esac
    fi
    export VPSMGR_STORAGE=btrfs
    BTRFS_CONFIRMED=1
  elif [[ "$VPSMGR_STORAGE" == "zfs" ]]; then
    echo "error: VPSMGR_STORAGE=zfs cannot be used on a btrfs root filesystem" >&2
    echo "   a ZFS pool cannot be created on btrfs (the copy-on-write backing loop" >&2
    echo "   file would corrupt the pool); use VPSMGR_STORAGE=btrfs ./install.sh" >&2
    exit 1
  elif [[ "$VPSMGR_STORAGE" == "dir" ]]; then
    echo "   note: dir backend on a btrfs root — quotas, snapshots and clone-on-create are unavailable (test boxes only)"
  fi
fi

# --- btrfs backend confirmation (beta) ---
# The btrfs storage backend is a newer, beta feature. It is expressed by
# choosing VPSMGR_STORAGE=btrfs (or auto-selected on a btrfs root, confirmed
# above). On a btrfs host this is effectively the only full-featured option (a
# ZFS pool cannot be initialized on top of btrfs), so default to YES — but the
# operator is explicitly told before proceeding. Non-btrfs installs never reach
# this block (their default stays zfs, so a plain `./install.sh` behaves
# exactly as before).
if [[ "$VPSMGR_STORAGE" == "btrfs" && "$BTRFS_CONFIRMED" == "0" ]]; then
  echo
  echo "!! Btrfs storage backend selected (VPSMGR_STORAGE=btrfs) !!"
  echo "   Btrfs is a BETA feature. On a Btrfs host a ZFS pool cannot be created,"
  echo "   so this is the standard way to install there; on other hosts it falls"
  echo "   back to a loop-file btrfs pool."
  if [[ ! -t 0 ]]; then
    echo "[install] non-interactive — proceeding with the Btrfs (beta) storage backend"
  else
    read -r -p "Continue with the Btrfs storage backend? [Y/n] " BTRFS_CONFIRM
    case "$BTRFS_CONFIRM" in
      ''|y|Y|yes|YES) ;;
      n|N|no|NO) echo "[install] aborted by user (declined the Btrfs backend)"; exit 1 ;;
      *) echo "error: please answer Y or n" >&2; exit 1 ;;
    esac
  fi
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

# --- --zfs-prealloc: INTERNAL TESTING ONLY ---
# Preallocate the ZFS backing file with incompressible data before creating the
# pool, so the physical space is claimed up front instead of growing sparsely.
# This is for experimental use on oversold hosts (the backing file's physical
# footprint is the only thing the operator can truly reserve there). It MUST
# only ever run on a strictly fresh, ZFS-backed install: an existing pool, pool
# file, Incus storage pool, or /etc/vpsmgr config means there is data to lose,
# and there is deliberately NO force override.
if [[ "$ZFS_PREALLOC" == "1" ]]; then
  # --zfs-prealloc is only meaningful when the final storage backend is zfs.
  # This is checked AFTER the btrfs-root block above, which may have switched a
  # default (unset) VPSMGR_STORAGE to btrfs.
  if [[ "$VPSMGR_STORAGE" != "zfs" ]]; then
    echo "error: --zfs-prealloc requires the zfs storage backend (current: $VPSMGR_STORAGE)" >&2
    echo "   preallocating a backing file only applies to the zfs loop-file pool." >&2
    exit 1
  fi

  # Strict fresh-install gate. Any of these means prior state exists; refusing
  # is safer than guessing. No --force exists on purpose.
  echo "[install] --zfs-prealloc: checking for an existing install (strict fresh-install gate)"
  if [[ -f /etc/vpsmgr/config.yaml ]]; then
    echo "error: --zfs-prealloc refuses to run: /etc/vpsmgr/config.yaml already exists (prior install)" >&2
    exit 1
  fi
  if [[ -e /var/lib/incus/disks/vpsmgr.img ]]; then
    echo "error: --zfs-prealloc refuses to run: pool backing file /var/lib/incus/disks/vpsmgr.img already exists" >&2
    exit 1
  fi
  if command -v zpool >/dev/null 2>&1 && zpool list vpsmgr >/dev/null 2>&1; then
    echo "error: --zfs-prealloc refuses to run: a zpool named 'vpsmgr' already exists" >&2
    exit 1
  fi
  if command -v incus >/dev/null 2>&1 && incus storage show vpsmgr >/dev/null 2>&1; then
    echo "error: --zfs-prealloc refuses to run: an Incus storage pool named 'vpsmgr' already exists" >&2
    exit 1
  fi
  echo "[install] fresh-install gate passed (no config, no pool file, no zpool, no Incus pool)"

  echo
  echo "!! --zfs-prealloc: INTERNAL TESTING INSTALL !!"
  echo "   This preallocates the ZFS backing file with incompressible data"
  echo "   (openssl AES-CTR) so the space is physically claimed on the host now,"
  echo "   then creates the ZFS pool on that file. It is intended for experimental"
  echo "   use on oversold hosts and is NOT a supported production install path."
  if [[ ! -t 0 ]]; then
    echo "error: --zfs-prealloc requires an interactive confirmation" >&2
    exit 1
  fi
  read -r -p "Continue with the internal-testing --zfs-prealloc install? [y/N] " PREALLOC_CONFIRM
  case "$PREALLOC_CONFIRM" in
    y|Y|yes|YES) ;;
    *) echo "[install] aborted by user (declined --zfs-prealloc)"; exit 1 ;;
  esac
  export VPSMGR_ZFS_PREALLOC=1
  echo "[install] --zfs-prealloc enabled"
fi

# Defensive guard: a plain (no-flag) ./install.sh downloads the prebuilt
# RELEASE binary, which always comes from the `main` branch — regardless of
# which branch is checked out. If this is a real git checkout sitting on a
# non-`main` branch, that is almost certainly not what the operator intends
# (they usually expect the dev branch's code to be installed). Prompt for
# confirmation. Tarball / one-click pipe installs have no .git directory and
# therefore skip this check entirely. The branch is read directly from
# .git/HEAD so no git(1) binary is required.
if [[ "$BUILD_MODE" == "release" && -d "$ROOT/.git" ]]; then
  RELEASE_BRANCH="$(sed -n 's|^ref: refs/heads/||p' "$ROOT/.git/HEAD" 2>/dev/null)"
  if [[ "$RELEASE_BRANCH" != "main" ]]; then
    if [[ -z "$RELEASE_BRANCH" ]]; then
      RELEASE_BRANCH="(detached HEAD)"
    fi
    echo
    echo "!! You are on branch '$RELEASE_BRANCH', but a plain ./install.sh installs the !!"
    echo "   prebuilt RELEASE binary from the main branch — NOT this branch's code."
    echo "   (use ./install.sh --local-build to compile this checkout's branch instead)"
    if [[ ! -t 0 ]]; then
      echo "[install] non-interactive — proceeding with the release (prebuild) install"
    else
      read -r -p "Continue anyway? (y/N) " RELEASE_CONFIRM
      case "$RELEASE_CONFIRM" in
        y|Y|yes|YES) ;;
        *) echo "[install] aborted by user — release install declined (branch '$RELEASE_BRANCH'); use --local-build to install this branch"; exit 1 ;;
      esac
    fi
  fi
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

# incus.image decides whether 50-image.sh must run. A config that sets it to
# anything other than the default vpsmgr/debian-sshd means the operator runs
# custom container images (an advanced setup): 50-image.sh would pull Debian
# 13 and rebuild a default sshd image nobody launches, so it is skipped on
# both install and upgrade. The default value (or a fresh install with no
# config yet) still runs 50-image to build the stock image.
CFG_IMAGE=""
if [[ -f /etc/vpsmgr/config.yaml ]]; then
  CFG_IMAGE=$(grep -E '^\s+image:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"' || true)
fi
export VPSMGR_SKIP_IMAGE=0
if [[ -n "$CFG_IMAGE" && "$CFG_IMAGE" != "vpsmgr/debian-sshd" ]]; then
  echo "[install] incus.image is '$CFG_IMAGE' (not the default vpsmgr/debian-sshd) —"
  echo "          skipping the Debian 13 sshd image build (50-image.sh); using your custom image"
  export VPSMGR_SKIP_IMAGE=1
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
      # btrfs swapfiles are fussy: they must be NODATACOW (no CoW), fully
      # preallocated (no holes), single-device/single-profile, and cannot sit on
      # a compressed file. The generic fallocate-then-mkswap path frequently
      # fails on btrfs with "swapon: Invalid argument".
      ROOTFS="$(findmnt -no FSTYPE / 2>/dev/null)"
      if [[ "$ROOTFS" == "btrfs" ]]; then
        # btrfs-progs >= 6.1 ships `btrfs filesystem mkswapfile`, which does the
        # whole job correctly (prealloc + nodatacow + format) in one step. On
        # older btrfs-progs fall back to manual: truncate empty, set the
        # NODATACOW attr, then WRITE the file with dd — deliberately NOT
        # fallocate, whose preallocated extents swapon can reject.
        if btrfs filesystem mkswapfile --help >/dev/null 2>&1; then
          btrfs filesystem mkswapfile --size "${SWAP_MB}M" /swapfile 2>/dev/null \
            || { echo "[install] error: btrfs mkswapfile failed — install aborted" >&2; exit 1; }
        else
          truncate -s 0 /swapfile
          chattr +C /swapfile 2>/dev/null || true
          dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MB" status=none 2>/dev/null
          chmod 600 /swapfile
          mkswap /swapfile >/dev/null 2>&1
        fi
      else
        # Non-btrfs: existing behavior (sparse fallocate, dd fallback).
        if ! fallocate -l "${SWAP_MB}M" /swapfile 2>/dev/null; then
          dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MB" status=none 2>/dev/null
        fi
        chmod 600 /swapfile
        mkswap /swapfile >/dev/null 2>&1
      fi
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

for step in 00-check 10-incus 20-network 30-traefik 40-panel 50-image; do
  # Advanced setup: incus.image is a custom (non-default) alias — no Debian 13
  # sshd image build. Skipped the same way on install and upgrade.
  if [[ "$step" == "50-image" && "${VPSMGR_SKIP_IMAGE:-0}" == "1" ]]; then
    echo "[install] skipping $step (custom incus.image configured)"
    continue
  fi
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
