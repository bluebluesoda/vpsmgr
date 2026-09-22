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
# IPv6 pass-through: forwarding must be on so the host relays container v6.
if [[ -n "${VPSMGR_IPV6_SUBNET:-}" ]]; then
  cat >> /etc/sysctl.d/99-vpsmgr.conf <<'EOF'
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.default.forwarding=1
EOF
fi
log "wrote /etc/sysctl.d/99-vpsmgr.conf (ip_forward + bbr/fq + io_uring_disabled${VPSMGR_IPV6_SUBNET:+ + ipv6 forwarding})"
SYSCTL_ARGS=(net.ipv4.ip_forward=1 net.core.default_qdisc=fq net.ipv4.tcp_congestion_control=bbr kernel.io_uring_disabled=1)
[[ -n "${VPSMGR_IPV6_SUBNET:-}" ]] && SYSCTL_ARGS+=(net.ipv6.conf.all.forwarding=1)
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
