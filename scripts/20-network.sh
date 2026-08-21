#!/usr/bin/env bash
# 20-network.sh — sysctl + nftables basics.
set -uo pipefail

log(){ echo "[20] $*"; }

# ip_forward + BBR/fq TCP tuning + io_uring attack-surface clamp. Written every
# run (idempotent). io_uring_disabled=1 lets only init-userns CAP_SYS_ADMIN (the
# host's own root services) create io_uring; container tenants — even container
# root, which only has userns-scoped caps — get EPERM, closing the biggest
# kernel LPE attack surface for tenants. Value 1, not 2: the host stack (Incus,
# ZFS, Go panel/traefik) is untouched. Matches RHEL 9.3+'s shipped default.
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
net.ipv4.tcp_rmem = 8192 262144 4194304
net.ipv4.tcp_wmem = 4096 16384 4194304
net.core.rmem_max = 4194304
net.core.wmem_max = 4194304
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

echo "[20] network ready"
