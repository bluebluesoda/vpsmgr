#!/usr/bin/env bash
# 10-lxd.sh — install LXD snap, storage pool, bridge.
set -uo pipefail
export PATH="$PATH:/snap/bin"

log(){ echo "[10] $*"; }
die(){ echo "[10] error: $*" >&2; exit 1; }

# --- ensure LXD snap 5.21/stable ---
if snap list lxd >/dev/null 2>&1; then
  log "lxd snap already installed: $(snap list lxd | awk 'NR==2{print $2}')"
else
  log "installing lxd snap (5.21/stable)..."
  snap install lxd --channel=5.21/stable || die "snap install lxd failed"
fi
log "waiting for LXD daemon..."
lxd waitready --timeout=120 || die "lxd daemon not ready"

# --- storage pool ---
# No host zfsutils needed: Ubuntu 24.04 kernel ships the zfs kernel module and
# the LXD snap bundles the zfs userspace tools, so LXD creates the pool itself.
POOL=vpsmgr
POOL_EXISTS=0
if lxc storage show "$POOL" >/dev/null 2>&1; then
  POOL_EXISTS=1
  log "storage pool '$POOL' exists ($(lxc storage show "$POOL" | awk -F': ' '/driver:/{print $2}'))"
fi

# find a spare whole-disk block device (no partitions, not the root disk, unmounted)
find_spare_disk(){
  ROOT_DISK=$(lsblk -rno NAME,MOUNTPOINTS | awk '$2=="/"{print $1; exit}' | sed 's/[0-9]*$//')
  for d in $(lsblk -rno NAME,TYPE | awk '$2=="disk"{print $1}'); do
    [[ "$d" == "$ROOT_DISK" ]] && continue
    [[ -b "/dev/$d" ]] || continue
    NCHILD=$(lsblk -rno NAME "$d" | tail -n +2 | wc -l)
    [[ "$NCHILD" -gt 0 ]] && continue
    grep -q "/dev/$d" /proc/mounts && continue
    echo "/dev/$d"; return 0
  done
  return 1
}

# decide how to configure the pool in preseed
DRIVER=zfs
SPARE=""
POOL_SIZE_MB=""
if [[ $POOL_EXISTS -eq 1 ]]; then
  # adopt existing pool
  DRIVER=$(lxc storage show "$POOL" | awk -F': ' '/driver:/{print $2}')
  SRC_LINE="    source: \"$POOL\""
  SIZE_LINE=""
else
  SPARE=$(find_spare_disk || true)
  if [[ -n "$SPARE" ]]; then
    log "zfs pool '$POOL' will be created on spare block device $SPARE"
    SRC_LINE="    source: \"$SPARE\""
    SIZE_LINE=""
  else
    FREE_KB=$(df -k --output=avail / | tail -1 | tr -d ' ')
    # Pool ceiling = a share of the free space, as a sparse loop file that
    # grows on demand. Small disks keep 80% so the host / LXD image store /
    # snap keep enough headroom; big disks (>= 20 GiB free) can afford 90%.
    PCT=80
    if (( FREE_KB > 20 * 1024 * 1024 )); then
      PCT=90
    fi
    POOL_SIZE_MB=$(( FREE_KB * PCT / 100 / 1024 ))
    log "loop-file zfs pool '$POOL' (~${POOL_SIZE_MB} MiB = ${PCT}% of free, created by LXD)"
    SRC_LINE=""
    SIZE_LINE="    size: \"${POOL_SIZE_MB}MiB\""
  fi
fi

# --- lxd init (preseed) ---
# IPv6 pass-through: when VPSMGR_IPV6_SUBNET is set, lxdbr0 carries the global
# prefix (no NAT) and containers SLAAC global addresses from it. The bridge
# ADDRESS is deliberately not set here: the Go setup (SetupIPv6Bridge, run at
# `vps install`) picks a free address inside the prefix and skips any the host
# already uses — hardcoding prefix::1 at init time would claim the most common
# upstream-router / host address before that clash check ever runs.
V6_IP="none"
V6_NAT="false"
if [[ -n "${VPSMGR_IPV6_SUBNET:-}" ]]; then
  log "IPv6 pass-through enabled: lxdbr0 address chosen by \`vps install\` (clash-avoiding)"
fi
# Container IPv4 subnet (10.<n>.0.0/24): the bridge gateway is .1. Defaults to
# 10.115.0.1/24; 00-ip-ask.sh exports the chosen subnet before this step.
V4_GW="$(echo "${VPSMGR_IPV4_SUBNET:-10.115.0.0/24}" | cut -d. -f1-3).1/24"
if [[ $POOL_EXISTS -eq 0 ]] || ! lxc network show lxdbr0 >/dev/null 2>&1; then
  PRESEED=/tmp/vpsmgr-preseed.yaml
  cat > "$PRESEED" <<EOF
config: {}
networks:
- config:
    ipv4.address: $V4_GW
    ipv4.nat: "true"
    ipv6.address: $V6_IP
    ipv6.nat: "$V6_NAT"
    ipv6.dhcp.stateful: "true"
    ipv6.routing: "true"
    # Do not serve instance-name DNS: LXD's dnsmasq would publish
    # <username>.lxd records for every container, so any tenant could nmap the
    # subnet and read everyone's username. dns.mode=none keeps DHCP + upstream
    # forwarding but drops the instance-name records.
    dns.mode: none
  description: ""
  name: lxdbr0
  type: bridge
storage_pools:
- config:
$SRC_LINE
$SIZE_LINE
  description: ""
  name: $POOL
  driver: $DRIVER
profiles:
- config: {}
  description: Default LXD profile
  devices:
    eth0:
      name: eth0
      nictype: bridged
      parent: lxdbr0
      type: nic
    root:
      path: /
      pool: $POOL
      type: disk
  name: default
cluster: null
EOF
  log "running lxd init --preseed (driver=$DRIVER, subnet $V4_GW, ipv6=${VPSMGR_IPV6_SUBNET:-none})"
  if ! lxd init --preseed < "$PRESEED"; then
    log "preseed failed — creating missing pieces"
    if [[ -n "${VPSMGR_IPV6_SUBNET:-}" ]]; then
      lxc network show lxdbr0 >/dev/null 2>&1 || lxc network create lxdbr0 ipv4.address=$V4_GW ipv4.nat=true ipv6.address=none ipv6.nat=false ipv6.dhcp.stateful=true ipv6.routing=true dns.mode=none
    else
      lxc network show lxdbr0 >/dev/null 2>&1 || lxc network create lxdbr0 ipv4.address=$V4_GW ipv4.nat=true ipv6.address=none dns.mode=none
    fi
    if ! lxc storage show "$POOL" >/dev/null 2>&1; then
      if [[ -n "$SPARE" ]]; then
        lxc storage create "$POOL" zfs source="$SPARE" || true
      elif [[ -n "$POOL_SIZE_MB" ]]; then
        lxc storage create "$POOL" zfs size="${POOL_SIZE_MB}MiB" || true
      fi
      # last resort: dir backend (no quotas)
      lxc storage show "$POOL" >/dev/null 2>&1 || {
        log "  warn: zfs pool creation failed, using dir backend (quotas disabled)"
        lxc storage create "$POOL" dir
      }
    fi
  fi
  # ensure default profile devices point at our pool/bridge
  lxc profile set default root.pool "$POOL" 2>/dev/null || true
  lxc profile device set default root pool "$POOL" 2>/dev/null || true
  lxc profile device set default eth0 parent lxdbr0 2>/dev/null || true
else
  log "LXD already initialized (pool+network present)"
fi

# Always re-assert dns.mode=none (fresh install and upgrade alike): this is the
# only thing that stops LXD from publishing <username>.lxd DNS/PTR records that
# let any tenant enumerate every other user's username with a subnet scan.
lxc network set lxdbr0 dns.mode=none 2>/dev/null || true

DRIVER_NOW=$(lxc storage show "$POOL" | awk -F': ' '/driver:/{print $2}')
log "storage backend: $DRIVER_NOW"
if [[ "$DRIVER_NOW" == "dir" ]]; then
  log "  warn: dir backend — disk quotas NOT enforced"
fi

echo "[10] lxd ready"
