#!/usr/bin/env bash
# 00-check.sh — environment sanity checks.
set -uo pipefail

log(){ echo "[00] $*"; }
die(){ echo "[00] error: $*" >&2; exit 1; }

if [[ $EUID -ne 0 ]]; then die "must run as root"; fi

# --- distro ---
if [[ ! -f /etc/os-release ]]; then die "cannot find /etc/os-release"; fi
. /etc/os-release
case "${ID:-}:${VERSION_ID:-}" in
  debian:12|debian:13) ;;
  ubuntu:22.04|ubuntu:24.04|ubuntu:26.04) ;;
  *) die "this installer targets Debian 12/13 or Ubuntu 22.04/24.04/26.04 (got ${PRETTY_NAME:-unknown})" ;;
esac

# --- virtualization (require physical or KVM) ---
if command -v systemd-detect-virt >/dev/null 2>&1; then
  VIRT=$(systemd-detect-virt)
  case "$VIRT" in
    openvz|lxc|lxc-libvirt|wsl|container|vm-other|podman|docker)
      die "unsupported environment: '$VIRT'. Need a physical machine or KVM VM."
      ;;
  esac
  log "virtualization: ${VIRT:-none (physical)}"
fi

# --- architecture ---
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *) die "unsupported architecture: $ARCH (only amd64 / arm64 are supported)" ;;
esac
log "architecture: $ARCH ($GOARCH)"

# --- hardware (warn only) ---
CPUS=$(nproc 2>/dev/null || echo 0)
MEM_KB=$(awk '/MemTotal/{print $2}' /proc/meminfo)
MEM_GB=$(( MEM_KB / 1024 / 1024 ))
log "cpu: ${CPUS} cores, mem: ${MEM_GB} GiB"
[[ $CPUS -lt 2 ]] && log "  warn: < 2 CPU may be slow"
[[ $MEM_GB -lt 2 ]] && log "  warn: < 2 GiB RAM is tight"

# --- disk ---
FREE_KB=$(df -k --output=avail / | tail -1 | tr -d ' ')
log "free disk on /: $(( FREE_KB / 1024 )) MiB"
[[ $FREE_KB -lt 5*1024*1024 ]] && die "need at least 5 GiB free on /"

# --- swap (recommend a swap file when absent) ---
# Containers can spike memory; a small box without swap OOMs the host instead
# of throttling. If any swap exists, leave it alone. Otherwise ask (default
# yes) to create and permanently enable a swap file the size of RAM (1:1),
# floored at 1 GiB — enough for `--local-build`'s go build without eating
# disk on small boxes.
SWAP_KB=$(awk '/SwapTotal/{print $2}' /proc/meminfo)
if [[ ${SWAP_KB:-0} -gt 0 ]]; then
  log "swap: $(awk '/SwapTotal/{printf "%.1f GiB", $2/1024/1024}' /proc/meminfo)"
else
  SWAP_MB=$(( MEM_KB / 1024 ))
  [[ $SWAP_MB -lt 1024 ]] && SWAP_MB=1024
  if [[ $SWAP_MB -ge 1024 ]]; then SIZE_HUMAN="$(( SWAP_MB / 1024 )) GiB"; else SIZE_HUMAN="${SWAP_MB} MiB"; fi
  log "no swap found (recommended: a swap file of ~${SIZE_HUMAN})"
  read -r -p "[00] create and permanently enable a ${SIZE_HUMAN} swap file now? [Y/n] " ANS
  case "${ANS:-y}" in
    y|Y|"")
      if [[ ! -e /swapfile ]]; then
        log "creating ${SIZE_HUMAN} swap file (this can take a moment)..."
        if ! fallocate -l "${SWAP_MB}M" /swapfile 2>/dev/null; then
          dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MB" status=none 2>/dev/null
        fi
        chmod 600 /swapfile
        mkswap /swapfile >/dev/null 2>&1
      fi
      if swapon /swapfile 2>/dev/null; then
        grep -q '^/swapfile' /etc/fstab 2>/dev/null || echo '/swapfile none swap sw 0 0' >> /etc/fstab
        log "swap enabled (${SIZE_HUMAN}, persisted in /etc/fstab)"
      else
        log "  warn: could not enable /swapfile (filesystem may not support swap files)"
      fi
      ;;
    *)
      log "skipping swap setup (warn: no swap may OOM the host under load)"
      ;;
  esac
fi

# --- packages ---
# Debian minimal images are notoriously bare (sometimes even curl is missing),
# so every tool the installer needs is ensured here, before any later step
# depends on it:
#   - zfsutils-linux: Incus does NOT bundle the ZFS userspace tools; the
#     storage pool is ZFS-only (dir backend is not supported), so zpool/zfs
#     must be present or pool creation fails
#   - linux-headers-amd64 (meta, no version) + build-essential: Debian only.
#     On Debian the ZFS kernel modules are compiled with DKMS against the
#     running kernel; the meta package tracks the installed kernel so the
#     postinst hook rebuilds the module after every kernel upgrade. Ubuntu
#     ships the ZFS module PREBUILT inside the kernel (linux-modules), so it
#     needs none of the DKMS toolchain — no compile time at all.
#   - ca-certificates: without it every curl HTTPS call (Zabbly key, traefik
#     download, image pulls) fails with a certificate error
#   - python3: used by 00-ip-ask.sh (prefix/subnet validation) and the
#     check-ipv6-support.sh probe
#   - tar/xz-utils: traefik tarball extraction; curl: downloads; gpg: Zabbly
#     key verification; nftables/zstd: firewall + Incus image compression
#   - sudo: hard requirement — the panel runs unprivileged and escalates ONLY
#     the sudoers whitelist commands via `sudo -n`. Without it every
#     privileged operation (nft reload, traefik/systemctl, IPv6 wiring, ndppd)
#     fails at runtime. visudo ships with the sudo package.
KERNEL_REL=$(uname -r)
BASE_DEPS="sudo zfsutils-linux ca-certificates python3 tar xz-utils nftables zstd curl gpg"
if [[ "$ID" == "debian" ]]; then
  BASE_DEPS="$BASE_DEPS linux-headers-amd64 build-essential"
  log "Debian detected — ZFS module will be DKMS-compiled (one-time build)"
else
  log "Ubuntu detected — ZFS module is prebuilt in the kernel, no compilation"
  # Ubuntu minimal/cloud images often ship with only the 'main' component; the
  # installer needs packages from universe (ndppd for IPv6 pass-through, among
  # others). Enable it idempotently.
  if ! grep -rhE "^[^#].*universe" /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null | grep -q .; then
    log "enabling the Ubuntu universe repository (needed for ndppd etc.)"
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq software-properties-common >/dev/null 2>&1 || true
    add-apt-repository -y universe >/dev/null 2>&1 || die "could not enable the universe repository"
    apt-get update -qq
  fi
fi
for p in $BASE_DEPS; do
  if ! dpkg -s "$p" >/dev/null 2>&1; then
    log "installing $p"
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$p" || log "  warn: could not install $p"
  fi
done

# ZFS module present and loadable for the RUNNING kernel? On Debian a kernel
# upgrade can leave the DKMS module missing (headers were unavailable at
# postinst time); rebuild it for the running kernel rather than failing later
# at pool creation. On Ubuntu the module ships with the kernel, so this is a
# no-op.
if ! modprobe zfs 2>/dev/null; then
  log "ZFS module missing for $KERNEL_REL — rebuilding via dkms (takes a minute)..."
  for v in $(dkms status 2>/dev/null | awk -F'[,/ ]+' '/^zfs\//{print $2}' | sort -u); do
    dkms build "zfs/$v" -k "$KERNEL_REL" >/dev/null 2>&1
    dkms install "zfs/$v" -k "$KERNEL_REL" >/dev/null 2>&1
  done
  modprobe zfs || die "ZFS kernel module failed to load — check 'dkms status' and 'dmesg | grep zfs'"
fi
log "zfs: $(zpool version 2>/dev/null | head -1)"

# Make ZFS boot-safe: the module must be loaded and the pool imported on every
# boot, or Incus's storage pool is unavailable after a reboot. zfsutils-linux
# normally enables these in its postinst; ensure them explicitly so a kernel
# update + manual reboot "just works": module loads, pool imports, datasets
# mount, then Incus and the panel come up on top.
for unit in zfs-import-cache.service zfs-mount.service zfs-zed.service zfs.target; do
  systemctl enable "$unit" >/dev/null 2>&1 || log "  warn: could not enable $unit"
done
log "zfs boot units enabled (import + mount on every boot)"

# --- Go toolchain (only needed for local build; installed lazily by 40-panel.sh) ---
if command -v go >/dev/null 2>&1; then
  log "go: $(go version 2>/dev/null | awk '{print $3}')"
fi

# --- Incus ---
if ! command -v incus >/dev/null 2>&1; then
  log "incus not installed yet (installed by 10-incus.sh)"
fi

# --- UFW conflict ---
# Incus manages its own `table inet incus` nftables rules (DHCP/DNS/forwarding)
# on incusbr0, but UFW's `table ip filter` runs at the same priority with a DROP
# policy, so it drops container DHCP/forwarding traffic before Incus's rules
# ever see it. Result: containers get no IPv4 (no DHCP, no DNS, no NAT) and
# the image build fails. See:
#   https://linuxcontainers.org/incus/docs/main/howto/network_bridge_firewalld/
# vpsmgr manages its own firewall via `table inet vpsmgr` nftables, so the
# cleanest fix is to disable UFW during install (idempotent: skipped when it
# is already inactive).
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
  # Already handled from a previous install? Never disable again silently —
  # the user may have re-enabled it on purpose.
  V4_NET="${VPSMGR_IPV4_SUBNET:-10.115.0.0/24}"
  V4_MATCH="$(printf '%s' "${V4_NET%/*}" | sed 's/\./\\./g')"
  if ufw status verbose 2>/dev/null | grep -qE "incusbr0|$V4_MATCH"; then
    log "ufw active but already has Incus/incusbr0 allow rules — leaving it as-is"
  else
    log "ufw is ACTIVE with default-DROP policy — this breaks Incus container IPv4"
    log "  (Incus's own nftables rules are shadowed by ufw's DROP; known issue:"
    log "  https://linuxcontainers.org/incus/docs/main/howto/network_bridge_firewalld/)"
    log "  vpsmgr manages its firewall via nftables; disabling ufw."
    ufw disable >/dev/null 2>&1 && log "  ufw disabled" || die "failed to disable ufw"
    # Keep it off across reboots. Snap/systemd enable it on boot otherwise.
    if command -v systemctl >/dev/null 2>&1; then
      systemctl disable ufw.service >/dev/null 2>&1 || true
      systemctl stop ufw.service >/dev/null 2>&1 || true
    fi
  fi
else
  log "ufw: not active (or not installed) — no conflict"
fi

# --- port occupancy ---
# Ports reserved for vpsmgr must be free on a fresh install. On adoption the
# panel and traefik are already running and owned by vpsmgr — those listeners
# are excluded by process name. Checks TCP and UDP (the user port block is
# DNAT-ed for both).
port_reserved(){
  local p="$1"
  [[ "$p" == "80" || "$p" == "443" ]] && return 0
  (( p >= 10000 && p <= 29999 )) && return 0
  (( p >= 30000 && p <= 31999 )) && return 0
  return 1
}
log "checking reserved ports (80/443, 10000-29999, 30000-31999)..."
CONFLICTS=""
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  ADDR=$(awk '{print $4}' <<<"$line")
  [[ -z "$ADDR" ]] && continue
  PORT="${ADDR##*:}"
  [[ "$PORT" =~ ^[0-9]+$ ]] || continue
  PROC=$(sed -n 's/.*users:(("\([^"]*\)".*/\1/p' <<<"$line")
  case "${PROC##*/}" in
    vpsmgr|traefik) continue ;;
  esac
  if port_reserved "$PORT"; then
    CONFLICTS+="  $PORT (${ADDR}) — bound by ${PROC:-unknown process}"$'\n'
  fi
done < <(ss -H -tlnp 2>/dev/null; ss -H -ulnp 2>/dev/null)
if [[ -n "$CONFLICTS" ]]; then
  die "ports reserved for vpsmgr are already in use:
$CONFLICTS
Free these ports (or remove the programs above) and re-run install."
fi
log "reserved ports are free"

# --- detect public ip / ext iface ---
EXT_IF=$(ip route show default | awk '{print $5; exit}')
PUB_IP=""
if [[ -n "$EXT_IF" ]]; then
  PUB_IP=$(ip -4 -o addr show dev "$EXT_IF" scope global | awk '{print $4}' | cut -d/ -f1 | head -1)
fi
if [[ -z "$PUB_IP" ]]; then
  PUB_IP=$(hostname -I | awk '{print $1}')
  log "  warn: no public IP detected on $EXT_IF, using $PUB_IP (private) as fallback"
fi
log "public/panel IP: $PUB_IP  (ext iface: ${EXT_IF:-auto})"

# --- network reachability (warn only) ---
if ! curl -sI --max-time 8 https://images.linuxcontainers.org >/dev/null 2>&1; then
  log "  warn: cannot reach images.linuxcontainers.org — Incus image pull will fail unless cached"
fi

echo "[00] checks passed"
