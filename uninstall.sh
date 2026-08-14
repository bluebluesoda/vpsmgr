#!/usr/bin/env bash
# uninstall.sh — remove vpsmgr. Without --purge the manager's config/db/certs
# and the traefik config are KEPT so a plain reinstall adopts the previous
# users/domains/settings. --purge also removes those, plus containers/storage.
set -uo pipefail

PURGE=0
if [[ "${1:-}" == "--purge" ]]; then PURGE=1; fi
log(){ echo "[un] $*"; }

log "stopping services..."
for svc in vps vps-nft vps-ipv6 traefik; do
  systemctl disable --now "$svc.service" >/dev/null 2>&1 || true
done
systemctl daemon-reload >/dev/null 2>&1 || true

# --- IPv6 pass-through cleanup (before removing config, which holds the prefix) ---
V6SUBNET=""
if [[ -f /etc/vpsmgr/config.yaml ]]; then
  V6SUBNET=$(grep -E '^\s+ipv6_subnet:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
fi
if [[ -n "$V6SUBNET" ]]; then
  log "cleaning IPv6 pass-through ($V6SUBNET)..."
  # stop and disable the NDP proxy (ndppd) and drop its generated config
  systemctl disable --now ndppd.service >/dev/null 2>&1 || service ndppd stop >/dev/null 2>&1 || true
  rm -f /etc/vpsmgr/ndppd.conf
  rm -f /etc/ndppd.conf   # root-owned symlink -> /etc/vpsmgr/ndppd.conf (created at install)
  # remove proxy_ndp entries on the ext iface for the prefix
  EXT_IF=$(ip route show default 2>/dev/null | awk '{print $5; exit}')
  if [[ -n "$EXT_IF" ]]; then
    ip -6 neigh show proxy dev "$EXT_IF" 2>/dev/null | awk '{print $1}' | while read -r a; do
      case "$a" in
        ${V6SUBNET%%/*}*) ip -6 neigh del proxy "$a" dev "$EXT_IF" 2>/dev/null || true ;;
      esac
    done
    sysctl -w net.ipv6.conf."$EXT_IF".proxy_ndp=0 >/dev/null 2>&1 || true
  fi
  # remove per-container /128 routes via incusbr0 for the prefix
  ip -6 route show dev incusbr0 2>/dev/null | awk '{print $1}' | while read -r a; do
    case "$a" in
      ${V6SUBNET%%/*}*) ip -6 route del "$a" dev incusbr0 2>/dev/null || true ;;
    esac
  done
  # restore incusbr0 IPv6 to disabled (matches vpsmgr default)
  if command -v incus >/dev/null 2>&1 && incus network show incusbr0 >/dev/null 2>&1; then
    incus network set incusbr0 ipv6.address none 2>/dev/null || true
    incus network set incusbr0 ipv6.nat false 2>/dev/null || true
    incus network set incusbr0 ipv6.routing false 2>/dev/null || true
    incus network set incusbr0 ipv6.dhcp.stateful false 2>/dev/null || true
  fi
  # live sysctls back to defaults
  sysctl -w net.ipv6.conf.all.forwarding=0 net.ipv6.conf.default.forwarding=0 >/dev/null 2>&1 || true
fi

log "removing files..."
rm -f /usr/local/bin/vps /usr/local/bin/traefik
rm -f /etc/systemd/system/vps.service /etc/systemd/system/vps-nft.service /etc/systemd/system/vps-ipv6.service /etc/systemd/system/traefik.service
# Restore the host-wide io_uring clamp to the kernel default before dropping
# 99-vpsmgr.conf (which sets it to 1 at install time).
sysctl -w kernel.io_uring_disabled=0 >/dev/null 2>&1 || true
rm -f /etc/sysctl.d/99-vpsmgr.conf
nft delete table inet vpsmgr 2>/dev/null || true
# /etc/vpsmgr (config/db/certs) and /etc/traefik are deliberately KEPT here:
# without --purge, a reinstall should adopt the existing users/domains/settings.
log "  kept /etc/vpsmgr and /etc/traefik (reinstall will adopt them)"

# Remove the panel's privilege surface even on a plain uninstall (review
# P2-10): a leftover /etc/sudoers.d/vps grants the vps user root-level nft /
# ip / systemctl rights forever, and the two service accounts linger. A
# reinstall recreates user + whitelist, so adoption is unaffected — only the
# residual root-equivalent access is gone.
log "removing vpsmgr users and sudoers whitelist..."
rm -f /etc/sudoers.d/vps
if id -u vps >/dev/null 2>&1; then
  userdel vps >/dev/null 2>&1 || true
fi
if id -u traefik >/dev/null 2>&1; then
  userdel traefik >/dev/null 2>&1 || true
fi

if [[ $PURGE -eq 1 ]]; then
  log "purging vpsmgr config/db/certs and traefik config..."
  rm -rf /etc/vpsmgr /etc/traefik
  log "purging Incus instances..."
  for c in $(incus list --format=csv -c n 2>/dev/null); do
    log "  deleting container $c"
    incus delete --force "$c" >/dev/null 2>&1 || true
  done
  log "removing storage pool..."
  incus storage delete vpsmgr >/dev/null 2>&1 || true
  if command -v zpool >/dev/null 2>&1 && zpool list vpsmgr >/dev/null 2>&1; then
    zpool destroy -f vpsmgr >/dev/null 2>&1 || true
  fi
  rm -rf /var/lib/vpsmgr
  log "removing Incus (Zabbly package) and its repo..."
  DEBIAN_FRONTEND=noninteractive apt-get remove -y -qq incus >/dev/null 2>&1 || true
  apt-get autoremove -y -qq >/dev/null 2>&1 || true
  rm -f /etc/apt/sources.list.d/zabbly-incus-lts-7.0.sources /etc/apt/keyrings/zabbly.asc
  apt-get update -qq >/dev/null 2>&1 || true
fi

if [[ $PURGE -eq 1 ]]; then
  log "done. use ./install.sh to reinstall (config/db removed)."
else
  log "done. use ./install.sh to reinstall (config/db kept)."
fi
