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
  ubuntu:24.04|ubuntu:26.04) ;;
  *) die "this installer targets Ubuntu 24.04 / 26.04 (got ${PRETTY_NAME:-unknown})" ;;
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
# yes) to create and permanently enable a swap file of half the RAM.
SWAP_KB=$(awk '/SwapTotal/{print $2}' /proc/meminfo)
if [[ ${SWAP_KB:-0} -gt 0 ]]; then
  log "swap: $(awk '/SwapTotal/{printf "%.1f GiB", $2/1024/1024}' /proc/meminfo)"
else
  SWAP_MB=$(( MEM_KB / 2 / 1024 ))
  [[ $SWAP_MB -lt 64 ]] && SWAP_MB=64
  if [[ $SWAP_MB -ge 1024 ]]; then SIZE_HUMAN="$(( SWAP_MB / 1024 )) GiB"; else SIZE_HUMAN="${SWAP_MB} MiB"; fi
  log "no swap found (recommended: a swap file of half the RAM, ~${SIZE_HUMAN})"
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
for p in snapd nftables zstd curl; do
  if ! dpkg -s "$p" >/dev/null 2>&1; then
    log "installing $p"
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$p"
  fi
done

# --- Go toolchain (only needed for local build; installed lazily by 40-panel.sh) ---
if command -v go >/dev/null 2>&1; then
  log "go: $(go version 2>/dev/null | awk '{print $3}')"
fi

# --- LXD snap ---
if ! snap list lxd >/dev/null 2>&1; then
  log "lxd snap not installed yet (installed by 10-lxd.sh)"
fi

# --- UFW conflict ---
# LXD manages its own `table inet lxd` nftables rules (DHCP/DNS/forwarding) on
# lxdbr0, but UFW's `table ip filter` runs at the same priority with a DROP
# policy, so it drops container DHCP/forwarding traffic before LXD's rules
# ever see it. Result: containers get no IPv4 (no DHCP, no DNS, no NAT) and
# the image build fails. See:
#   https://canonical.com/lxd/docs/latest/howto/network_bridge_firewalld/
# vpsmgr manages its own firewall via `table inet vpsmgr` nftables, so the
# cleanest fix is to disable UFW during install (idempotent: skipped when it
# is already inactive).
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
  # Already handled from a previous install? Never disable again silently —
  # the user may have re-enabled it on purpose.
  V4_NET="${VPSMGR_IPV4_SUBNET:-10.115.0.0/24}"
  V4_MATCH="$(printf '%s' "${V4_NET%/*}" | sed 's/\./\\./g')"
  if ufw status verbose 2>/dev/null | grep -qE "lxdbr0|$V4_MATCH"; then
    log "ufw active but already has LXD/lxdbr0 allow rules — leaving it as-is"
  else
    log "ufw is ACTIVE with default-DROP policy — this breaks LXD container IPv4"
    log "  (LXD's own nftables rules are shadowed by ufw's DROP; known issue:"
    log "  https://canonical.com/lxd/docs/latest/howto/network_bridge_firewalld/)"
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
  log "  warn: cannot reach images.linuxcontainers.org — LXD image pull will fail unless cached"
fi

echo "[00] checks passed"
