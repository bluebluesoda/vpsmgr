#!/usr/bin/env bash
# 20-network.sh — sysctl + nftables basics.
set -uo pipefail

log(){ echo "[20] $*"; }

# ip_forward + BBR/fq TCP tuning + io_uring attack-surface clamp. Written every
# run (idempotent). io_uring_disabled=1 lets only init-userns CAP_SYS_ADMIN (the
# host's own root services) create io_uring; container tenants — even container
# root, which only has userns-scoped caps — get EPERM, closing the biggest
# kernel LPE attack surface for tenants. Value 1, not 2: the host stack (Incus,
# ZFS, Go panel/HAProxy) is untouched. Matches RHEL 9.3+'s shipped default.
cat > /etc/sysctl.d/99-vpsmgr.conf <<EOF
# Managed by vpsmgr — generated file, do not edit by hand.
# Changes are overwritten on the next install.
net.ipv4.ip_forward=1
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
kernel.io_uring_disabled=1
net.ipv6.conf.all.use_tempaddr = 0
net.ipv6.conf.default.use_tempaddr = 0
net.core.netdev_max_backlog = 8192
net.core.rmem_default = 262144
net.core.wmem_default = 262144
net.ipv4.tcp_rmem = 8192 262144 8388608
net.ipv4.tcp_wmem = 4096 16384 8388608
net.core.rmem_max = 8388608
net.core.wmem_max = 8388608
net.ipv4.tcp_window_scaling = 1
net.ipv4.tcp_slow_start_after_idle = 0
EOF
# IPv6 pass-through. Only NAME-FREE keys are persisted here: `all.*` is
# inherited by interfaces created later, while the per-interface value for the
# EXISTING WAN interface is applied at runtime by `vps ipv6-reapply` / the panel
# (they know the real interface name — a name hard-coded here would silently
# miss ens3/enp1s0-style interfaces).
# proxy_ndp is inert without proxy entries, so it goes in unconditionally (it
# also future-proofs an extra prefix enabled later on a none-mode host).
# forwarding is NOT unconditional: on a host whose own IPv6 comes from a router
# advertisement, forwarding=1 suppresses the RA default route, so it is only set
# when vpsmgr itself relays container v6 (a base prefix, or a routed mode).
V6_ROUTED=0
case "${VPSMGR_IPV6_MODE:-}" in
  pool|none) V6_ROUTED=1 ;;
esac
if [[ -n "${VPSMGR_IPV6_SUBNET:-}" || "$V6_ROUTED" == 1 ]]; then V6_FWD=1; else V6_FWD=0; fi
cat >> /etc/sysctl.d/99-vpsmgr.conf <<'EOF'
net.ipv6.conf.all.proxy_ndp=1
EOF
if [[ "$V6_FWD" == 1 ]]; then
  cat >> /etc/sysctl.d/99-vpsmgr.conf <<'EOF'
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.default.forwarding=1
EOF
fi
V6_NOTE=" + ipv6 proxy_ndp"
[[ "$V6_FWD" == 1 ]] && V6_NOTE="$V6_NOTE + ipv6 forwarding"
log "wrote /etc/sysctl.d/99-vpsmgr.conf (ip_forward + bbr/fq + io_uring_disabled$V6_NOTE)"
SYSCTL_ARGS=(net.ipv4.ip_forward=1 net.core.default_qdisc=fq net.ipv4.tcp_congestion_control=bbr kernel.io_uring_disabled=1 net.ipv6.conf.all.proxy_ndp=1)
[[ "$V6_FWD" == 1 ]] && SYSCTL_ARGS+=(net.ipv6.conf.all.forwarding=1)
if ! sysctl -q -w "${SYSCTL_ARGS[@]}" 2>/dev/null; then
  log "warn: live sysctl apply failed (e.g. kernel too old for bbr/io_uring_disabled); config persisted and will apply on reboot"
fi
log "tcp congestion: $(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null || echo 'n/a')"

if ! dpkg -s nftables >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nftables
fi
log "nftables: $(nft --version 2>/dev/null | head -1)"

# --- Docker compatibility (DOCKER-USER chain) ---
# When Docker is installed on the host, it sets iptables FORWARD default policy
# to DROP. This breaks forwarded traffic for incusbr0 (both IPv4, and IPv6 if
# Docker's experimental/ipv6 is enabled). Docker provides the DOCKER-USER chain
# specifically for custom allow rules to be evaluated before Docker's own rules.
# Allow incusbr0 traffic in DOCKER-USER now, and install a systemd drop-in for
# docker.service so rules persist across Docker restarts.
apply_docker_rules(){
  if command -v iptables >/dev/null 2>&1 && iptables -L DOCKER-USER -n >/dev/null 2>&1; then
    iptables -C DOCKER-USER -i incusbr0 -j ACCEPT 2>/dev/null || iptables -I DOCKER-USER -i incusbr0 -j ACCEPT
    iptables -C DOCKER-USER -o incusbr0 -m conntrack --ctstate RELATED,ESTABLISHED 2>/dev/null || iptables -I DOCKER-USER -o incusbr0 -m conntrack --ctstate RELATED,ESTABLISHED 2>/dev/null
  fi
  if command -v ip6tables >/dev/null 2>&1 && ip6tables -L DOCKER-USER -n >/dev/null 2>&1; then
    ip6tables -C DOCKER-USER -i incusbr0 -j ACCEPT 2>/dev/null || ip6tables -I DOCKER-USER -i incusbr0 -j ACCEPT
    ip6tables -C DOCKER-USER -o incusbr0 -m conntrack --ctstate RELATED,ESTABLISHED 2>/dev/null || ip6tables -I DOCKER-USER -o incusbr0 -m conntrack --ctstate RELATED,ESTABLISHED 2>/dev/null
  fi
}
apply_docker_rules

if systemctl list-unit-files docker.service >/dev/null 2>&1 || [[ -f /lib/systemd/system/docker.service || -f /etc/systemd/system/docker.service ]]; then
  log "docker detected — ensuring incusbr0 forwarding rules in docker.service.d/vpsmgr.conf"
  install -d -m 0755 /etc/systemd/system/docker.service.d
  cat > /etc/systemd/system/docker.service.d/vpsmgr.conf <<'EOF'
# Managed by vpsmgr — generated file, do not edit by hand.
# Keeps incusbr0 traffic allowed in DOCKER-USER across Docker restarts.
[Service]
ExecStartPost=-/bin/sh -c 'iptables -C DOCKER-USER -i incusbr0 -j ACCEPT 2>/dev/null || iptables -I DOCKER-USER -i incusbr0 -j ACCEPT; iptables -C DOCKER-USER -o incusbr0 -m conntrack --ctstate RELATED,ESTABLISHED 2>/dev/null || iptables -I DOCKER-USER -o incusbr0 -m conntrack --ctstate RELATED,ESTABLISHED 2>/dev/null; ip6tables -L DOCKER-USER -n >/dev/null 2>&1 && { ip6tables -C DOCKER-USER -i incusbr0 -j ACCEPT 2>/dev/null || ip6tables -I DOCKER-USER -i incusbr0 -j ACCEPT; ip6tables -C DOCKER-USER -o incusbr0 -m conntrack --ctstate RELATED,ESTABLISHED 2>/dev/null || ip6tables -I DOCKER-USER -o incusbr0 -m conntrack --ctstate RELATED,ESTABLISHED 2>/dev/null; } || true'
EOF
  systemctl daemon-reload >/dev/null 2>&1 || true
fi

echo "[20] network ready"
