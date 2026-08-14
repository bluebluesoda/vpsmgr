#!/usr/bin/env bash
# 10-incus.sh — install Incus (Zabbly LTS repo), storage pool, bridge.
# Targets Debian 12/13 and Ubuntu 22.04/24.04/26.04 via the Zabbly package
# repository (the Incus team's official third-party repo, maintained by the
# project lead). Version follows the lts-7.0 channel.
set -uo pipefail

log(){ echo "[10] $*"; }
die(){ echo "[10] error: $*" >&2; exit 1; }

# --- ensure the Zabbly repo is present, then install Incus ---
ZABBLY_SOURCE="/etc/apt/sources.list.d/zabbly-incus-lts-7.0.sources"
if [[ -f "$ZABBLY_SOURCE" ]]; then
  log "zabbly incus lts-7.0 repo already configured"
else
  log "adding Zabbly Incus LTS 7.0 repository..."
  install -d -m 0755 /etc/apt/keyrings
  curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc \
    || die "could not download the Zabbly signing key"
  # Verify the published fingerprint (4EFC 5906 96CB 15B8 7C73 A3AD 82CC 8797 C838 DCFD).
  FP=$(gpg --show-keys --with-colons /etc/apt/keyrings/zabbly.asc 2>/dev/null | awk -F: '/^fpr:/{print $10; exit}')
  if [[ "$FP" != "4EFC590696CB15B87C73A3AD82CC8797C838DCFD" ]]; then
    die "zabbly key fingerprint mismatch ($FP) — refusing to install from an unverified repo"
  fi
  cat > "$ZABBLY_SOURCE" <<EOF
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/lts-7.0
Suites: $(. /etc/os-release && echo ${VERSION_CODENAME})
Components: main
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc
EOF
  apt-get update -qq || die "apt-get update failed (check the Zabbly repo)"
fi

if dpkg -s incus >/dev/null 2>&1; then
  log "incus already installed: $(incus version 2>/dev/null | head -1)"
else
  log "installing incus (Zabbly lts-7.0)..."
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq incus || die "apt install incus failed"
fi
log "incus version: $(incus version 2>/dev/null | head -1)"

# Make the incus-admin group available to the vps service user (created by
# 40-panel.sh / `vps install`). The panel talks to Incus over its Unix socket,
# which is group-readable by incus-admin — membership grants full management
# of Incus without running as root.
if id -u vps >/dev/null 2>&1; then
  usermod -aG incus-admin vps >/dev/null 2>&1 || true
  log "added user 'vps' to group incus-admin (panel can manage Incus via the API)"
fi

# Wait for the daemon socket to be up (systemd socket activation).
for i in $(seq 1 60); do
  [[ -S /var/lib/incus/unix.socket ]] && break
  sleep 1
done
[[ -S /var/lib/incus/unix.socket ]] || die "incus daemon socket not ready"

# --- storage pool ---
# ZFS only: Incus quotas, snapshots and clone-on-create (the whole disk model)
# depend on it. Incus does NOT bundle the ZFS userspace tools, so
# zfsutils-linux must be installed (00-check.sh ensures it). The dir backend is
# deliberately not offered — it has no quotas, which silently breaks the disk
# limits every container is supposed to have.
command -v zpool >/dev/null 2>&1 || die "zfsutils-linux is not installed (zpool missing) — 00-check.sh should have installed it; run it or: apt-get install -y zfsutils-linux"
POOL=vpsmgr
POOL_EXISTS=0
if incus storage show "$POOL" >/dev/null 2>&1; then
  POOL_EXISTS=1
  DRIVER=$(incus storage show "$POOL" | awk -F': ' '/driver:/{print $2}')
  if [[ "$DRIVER" != "zfs" ]]; then
    die "existing storage pool '$POOL' uses driver '$DRIVER' — this installer requires ZFS (dir has no quotas)"
  fi
  log "storage pool '$POOL' exists ($DRIVER)"
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

# Decide how to configure the pool in preseed.
DRIVER=zfs
SPARE=""
POOL_SIZE_MB=""
if [[ $POOL_EXISTS -eq 1 ]]; then
  # adopt existing pool
  DRIVER=$(incus storage show "$POOL" | awk -F': ' '/driver:/{print $2}')
  SRC_LINE="    source: \"$POOL\""
  SIZE_LINE=""
else
  SPARE=$(find_spare_disk || true)
  if [[ -n "$SPARE" ]]; then
    log "zfs pool '$POOL' will be created on spare block device $SPARE"
    SRC_LINE="    source: \"$SPARE\""
    SIZE_LINE=""
  else
    # Loop-file pool: the pool is a sparse file that grows on demand, so the
    # ceiling is a share of the host's FREE space. Before sizing it, make sure
    # the host actually has room: when free space is tight, reclaim caches and
    # build artifacts FIRST (apt cache, autoremove, journal, stale kernels,
    # /tmp, pip/go caches) so the pool + Incus image store + system still fit.
    # Without this, a host close to full would carve a pool that then starves
    # the root filesystem.
    FREE_KB=$(df -k --output=avail / | tail -1 | tr -d ' ')
    if (( FREE_KB < 25 * 1024 * 1024 )); then
      log "free space on / is $(( FREE_KB / 1024 / 1024 )) GiB (< 25 GiB) — reclaiming caches before sizing the pool..."
      # Cleanest wins first; each is idempotent and never touches user data.
      DEBIAN_FRONTEND=noninteractive apt-get clean 2>/dev/null || true
      rm -rf /var/lib/apt/lists/* 2>/dev/null || true
      DEBIAN_FRONTEND=noninteractive apt-get autoremove -y -qq 2>/dev/null || true
      journalctl --vacuum-time=3d >/dev/null 2>&1 || true
      rm -rf /tmp/* /var/tmp/* 2>/dev/null || true
      # Stale kernels: purge ALL linux-image-* except the RUNNING kernel — but
      # if a NEWER kernel than the running one is already installed (upgraded,
      # not yet rebooted), keep that newest one too: deleting it would make the
      # next reboot BOOT AN OLDER KERNEL than the one currently running. The
      # meta package linux-image-<arch> (no version, e.g. linux-image-amd64) is
      # excluded — it holds no image payload but keeps apt pulling future
      # kernels; purging it would silently stop kernel upgrades.
      RUNNING_KERNEL=$(uname -r)
      mapfile -t ALL_KERNELS < <(dpkg -l 'linux-image-*' 2>/dev/null | awk '/^ii/{print $2}' | grep -v -- "-dbg" | grep -E -- "-[0-9]+" | sort -V)
      KEEP=("linux-image-$RUNNING_KERNEL")
      NEWEST=${ALL_KERNELS[-1]}
      if [[ -n "$NEWEST" && "$NEWEST" != "linux-image-$RUNNING_KERNEL" ]]; then
        KEEP+=("$NEWEST")
        log "  keeping $NEWEST (newer than running kernel — reboot pending)"
      fi
      OLD_KERNELS=()
      for k in "${ALL_KERNELS[@]}"; do
        keep=0
        for kk in "${KEEP[@]}"; do [[ "$k" == "$kk" ]] && keep=1; done
        (( keep )) || OLD_KERNELS+=("$k")
      done
      if (( ${#OLD_KERNELS[@]} > 0 )); then
        log "  purging ${#OLD_KERNELS[@]} old kernel(s): ${OLD_KERNELS[*]}"
        DEBIAN_FRONTEND=noninteractive apt-get purge -y -qq "${OLD_KERNELS[@]}" >/dev/null 2>&1 || true
      fi
      rm -rf /root/.cache/pip /root/.cache/go-build /root/go/pkg/mod/cache/download 2>/dev/null || true
      FREE_KB=$(df -k --output=avail / | tail -1 | tr -d ' ')
      log "after reclaim: $(( FREE_KB / 1024 / 1024 )) GiB free on /"
    fi
    # Pool ceiling = a share of the free space, as a sparse loop file that
    # grows on demand. Small disks keep 80% so the host / Incus image store
    # keep enough headroom; big disks (>= 20 GiB free) can afford 90%.
    PCT=80
    if (( FREE_KB > 20 * 1024 * 1024 )); then
      PCT=90
    fi
    POOL_SIZE_MB=$(( FREE_KB * PCT / 100 / 1024 ))
    # Refuse to build a pool so small the host starves: require the pool to
    # leave at least 3 GiB free even after accounting for the loop file's
    # eventual size. (The loop file is sparse — it only occupies what's used —
    # so this is the worst-case guard.)
    if (( FREE_KB - POOL_SIZE_MB * 1024 < 3 * 1024 * 1024 )); then
      die "too little free space on / ($(( FREE_KB / 1024 / 1024 )) GiB) even after cleanup — cannot create a usable zfs pool"
    fi
    log "loop-file zfs pool '$POOL' (~${POOL_SIZE_MB} MiB = ${PCT}% of free, created by Incus)"
    SRC_LINE=""
    SIZE_LINE="    size: \"${POOL_SIZE_MB}MiB\""
  fi
fi

# --- incus admin init (preseed) ---
# IPv6 pass-through: when VPSMGR_IPV6_SUBNET is set, incusbr0 carries the global
# prefix (no NAT) and containers SLAAC global addresses from it. The bridge
# ADDRESS is deliberately not set here: the Go setup (SetupIPv6Bridge, run at
# `vps install`) picks a free address inside the prefix and skips any the host
# already uses — hardcoding prefix::1 at init time would claim the most common
# upstream-router / host address before that clash check ever runs.
V6_IP="none"
V6_NAT="false"
if [[ -n "${VPSMGR_IPV6_SUBNET:-}" ]]; then
  log "IPv6 pass-through enabled: incusbr0 address chosen by \`vps install\` (clash-avoiding)"
fi
# Container IPv4 subnet (10.<n>.0.0/24): the bridge gateway is .1. Defaults to
# 10.115.0.1/24; 00-ip-ask.sh exports the chosen subnet before this step.
V4_GW="$(echo "${VPSMGR_IPV4_SUBNET:-10.115.0.0/24}" | cut -d. -f1-3).1/24"
if [[ $POOL_EXISTS -eq 0 ]] || ! incus network show incusbr0 >/dev/null 2>&1; then
  PRESEED="$(mktemp /tmp/vpsmgr-preseed.XXXXXX.yaml)"
  trap 'rm -f "$PRESEED"' EXIT
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
    # Do not serve instance-name DNS: Incus's dnsmasq would publish
    # <username>.lxd records for every container, so any tenant could nmap the
    # subnet and read everyone's username. dns.mode=none keeps DHCP + upstream
    # forwarding but drops the instance-name records.
    dns.mode: none
  description: ""
  name: incusbr0
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
  description: Default Incus profile
  devices:
    eth0:
      nictype: bridged
      parent: incusbr0
      type: nic
    root:
      path: /
      pool: $POOL
      type: disk
  name: default
EOF
  log "running incus admin init --preseed (driver=$DRIVER, subnet $V4_GW, ipv6=${VPSMGR_IPV6_SUBNET:-none})"
  if ! incus admin init --preseed < "$PRESEED"; then
    log "preseed failed — creating missing pieces"
    if [[ -n "${VPSMGR_IPV6_SUBNET:-}" ]]; then
      incus network show incusbr0 >/dev/null 2>&1 || incus network create incusbr0 ipv4.address=$V4_GW ipv4.nat=true ipv6.address=none ipv6.nat=false ipv6.dhcp.stateful=true ipv6.routing=true dns.mode=none
    else
      incus network show incusbr0 >/dev/null 2>&1 || incus network create incusbr0 ipv4.address=$V4_GW ipv4.nat=true ipv6.address=none dns.mode=none
    fi
    if ! incus storage show "$POOL" >/dev/null 2>&1; then
      if [[ -n "$SPARE" ]]; then
        incus storage create "$POOL" zfs source="$SPARE" || die "zfs pool creation on $SPARE failed"
      elif [[ -n "$POOL_SIZE_MB" ]]; then
        incus storage create "$POOL" zfs size="${POOL_SIZE_MB}MiB" || die "zfs pool creation (loop file) failed"
      else
        die "no disk for the zfs pool and no loop-file size decided"
      fi
    fi
  fi
else
  log "Incus already initialized (pool+network present)"
fi

# Ensure the default profile carries the root disk + eth0 devices. This runs on
# EVERY install (not only preseed) because Incus's preseed may leave the default
# profile untouched ("unless you explicitly express that in the YAML") and an
# adopted/partially-initialized daemon can have an empty profile.
incus profile show default >/dev/null 2>&1 || incus profile create default
incus profile device list default | grep -q "root" || \
  incus profile device add default root disk path=/ pool="$POOL" || true
incus profile device set default root pool "$POOL" 2>/dev/null || true
incus profile device list default | grep -q "eth0" || \
  incus profile device add default eth0 nic nictype=bridged parent=incusbr0 || true
incus profile device set default eth0 parent incusbr0 2>/dev/null || true

# Always re-assert dns.mode=none (fresh install and upgrade alike): this is the
# only thing that stops Incus from publishing <username>.lxd DNS/PTR records
# that let any tenant enumerate every other user's username with a subnet scan.
incus network set incusbr0 dns.mode=none 2>/dev/null || true

DRIVER_NOW=$(incus storage show "$POOL" | awk -F': ' '/driver:/{print $2}')
log "storage backend: $DRIVER_NOW"
if [[ "$DRIVER_NOW" != "zfs" ]]; then
  die "storage pool '$POOL' is not ZFS ('$DRIVER_NOW') — this installer requires ZFS (quotas, snapshots, clones)"
fi

echo "[10] incus ready"
