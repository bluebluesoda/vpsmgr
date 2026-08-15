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
# zfs (default) or dir, chosen by install.sh / VPSMGR_STORAGE. ZFS gives Incus
# quotas, snapshots and clone-on-create — the whole disk model. The dir backend
# has none of those and is only offered for throwaway test boxes; it is NEVER a
# fallback (a failed zfs pool is a hard error, not a silent downgrade).
# New installations use a sparse loop-file ZFS pool on /. Incus does NOT bundle
# the ZFS userspace tools, so zfsutils-linux must be installed (00-check.sh
# ensures it).
STORAGE="${VPSMGR_STORAGE:-zfs}"
case "$STORAGE" in
  zfs|dir) ;;
  *) die "VPSMGR_STORAGE must be zfs or dir (got '$STORAGE')" ;;
esac
if [[ "$STORAGE" == "zfs" ]]; then
  command -v zpool >/dev/null 2>&1 || die "zfsutils-linux is not installed (zpool missing) — 00-check.sh should have installed it; run it or: apt-get install -y zfsutils-linux"
fi
POOL=vpsmgr
POOL_EXISTS=0
if incus storage show "$POOL" >/dev/null 2>&1; then
  POOL_EXISTS=1
  DRIVER=$(incus storage show "$POOL" | awk -F': ' '/driver:/{print $2}')
  if [[ "$STORAGE" == "zfs" && "$DRIVER" != "zfs" ]]; then
    die "existing storage pool '$POOL' uses driver '$DRIVER' — zfs mode requires ZFS (dir has no quotas)"
  fi
  log "storage pool '$POOL' exists ($DRIVER)"
fi

# Decide how to configure the pool in preseed.
# New installations always use a sparse loop-file ZFS pool on /. The installer
# deliberately never scans, selects, formats, or modifies secondary block
# devices: virtual CD/ISO devices and provider-specific disk layouts are left
# completely untouched. Existing vpsmgr pools are adopted unchanged.
DRIVER=$STORAGE
POOL_SIZE_MB=""
if [[ $POOL_EXISTS -eq 1 ]]; then
  # Adopt existing pool.
  DRIVER=$(incus storage show "$POOL" | awk -F': ' '/driver:/{print $2}')
  SRC_LINE="    source: \"$POOL\""
  SIZE_LINE=""
elif [[ "$STORAGE" == "zfs" ]]; then
  # Loop-file pool: the pool is sparse and grows on demand inside /. Before
  # sizing it, reclaim caches and build artifacts when free space is tight so
  # the pool, Incus image store, and host remain usable.
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
    # meta package linux-image-<arch> (no version, e.g. linux-image-amd64)
    # is excluded — it holds no image payload but keeps apt pulling future
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
  # Small disks keep 80% so the host and Incus image store retain headroom;
  # larger free roots can afford 90%. The loop file is sparse, so creation
  # itself consumes no space; actual container data grows it on demand.
  PCT=80
  if (( FREE_KB > 20 * 1024 * 1024 )); then
    PCT=90
  fi
  POOL_SIZE_MB=$(( FREE_KB * PCT / 100 / 1024 ))
  log "loop-file zfs pool '$POOL' on / (~${POOL_SIZE_MB} MiB = ${PCT}% of free)"
  SRC_LINE=""
  SIZE_LINE="    size: \"${POOL_SIZE_MB}MiB\""
else
  log "dir pool '$POOL' (no quotas, snapshots or clone-on-create)"
  SRC_LINE="    {}"
  SIZE_LINE=""
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
      if [[ "$STORAGE" == "zfs" ]]; then
        [[ -n "$POOL_SIZE_MB" ]] || die "no loop-file size decided for the ZFS pool"
        incus storage create "$POOL" zfs size="${POOL_SIZE_MB}MiB" || die "zfs pool creation (loop file) failed"
      else
        incus storage create "$POOL" dir || die "dir pool creation failed"
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
if [[ "$STORAGE" == "zfs" ]]; then
  if [[ "$DRIVER_NOW" != "zfs" ]]; then
    die "storage pool '$POOL' is not ZFS ('$DRIVER_NOW') — zfs mode requires ZFS (quotas, snapshots, clones)"
  fi
else
  log "  warn: dir backend — disk quotas, snapshots and clone-on-create are NOT enforced"
fi

echo "[10] incus ready"
