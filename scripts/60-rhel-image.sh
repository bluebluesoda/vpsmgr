#!/usr/bin/env bash
# 60-rhel-image.sh — OPTIONAL: build a RHEL-family "sshd + universal tooling"
# image so users can reinstall their container with something other than the
# default Debian 13. Run as root AFTER install.sh, only when you actually want
# the extra image (a toy panel should not build it on every box).
#
#   sudo bash scripts/60-rhel-image.sh          # Alma 9 (recommended)
#   sudo bash scripts/60-rhel-image.sh rocky     # Rocky 9 instead
#
# Same hygiene as 50-image.sh: sshd + common tools baked in, dnf caches and
# logs cleaned before publishing, base image deleted afterwards.
set -uo pipefail


DISTRO="${1:-alma}"
case "$DISTRO" in
  alma)  REMOTE_ALIAS=images:almalinux/9; IMAGE=vpsmgr/alma-sshd;  BASE_ALIAS=vpsmgr-almalinux-9 ;;
  rocky) REMOTE_ALIAS=images:rockylinux/9; IMAGE=vpsmgr/rocky-sshd; BASE_ALIAS=vpsmgr-rockylinux-9 ;;
  *) echo "usage: $0 [alma|rocky]" >&2; exit 1 ;;
esac

log(){ echo "[60] $*"; }

incus info >/dev/null 2>&1 || { echo "[60] error: Incus not ready" >&2; exit 1; }

if incus image show "$IMAGE" >/dev/null 2>&1; then
  log "image $IMAGE already present"
  exit 0
fi

if ! incus image list "$BASE_ALIAS" --format=csv 2>/dev/null | grep -q .; then
  log "pulling $REMOTE_ALIAS (fallback base, deleted after build)..."
  if ! incus image copy "$REMOTE_ALIAS" local: --alias "$BASE_ALIAS"; then
    log "  warn: image pull failed — nothing built"
    exit 0
  fi
else
  log "base image $BASE_ALIAS already present"
fi

log "building $IMAGE (this takes a few minutes)..."
NAME=tmp-rhel-builder
incus delete --force "$NAME" >/dev/null 2>&1 || true
if incus launch "$BASE_ALIAS" "$NAME"; then
  # wait until usable
  for i in $(seq 1 60); do
    if incus exec "$NAME" -- /bin/true >/dev/null 2>&1; then break; fi
    sleep 2
  done
  # RHEL containers bring eth0 up with NetworkManager, which lags the Incus agent
  # by a few seconds: running dnf before DHCP has written resolv.conf makes it
  # die with "Curl error (6): Couldn't resolve host". Wait until DNS answers,
  # or the builder install fails and a broken image gets published.
  DNS_OK=
  for i in $(seq 1 30); do
    if incus exec "$NAME" -- getent hosts mirrors.almalinux.org >/dev/null 2>&1; then
      DNS_OK=1; break
    fi
    sleep 2
  done
  if [ -z "$DNS_OK" ]; then
    log "  warn: builder never got working DNS; nothing built"
    incus delete --force "$NAME" >/dev/null 2>&1 || true
    incus image delete "$BASE_ALIAS" >/dev/null 2>&1 || true
  elif incus exec "$NAME" -- sh -c '
set -e
# universal user tooling (mirrors the Debian image): sshd, curl/wget need
# ca-certificates or HTTPS fails; bind-utils is the RHEL nslookup/dig package.
# dnf is retried: the mirrorlist can flap on a slow uplink right after boot.
for attempt in 1 2 3; do
  dnf -y install openssh-server ca-certificates curl wget less bind-utils openssh-clients unzip nano && break
  sleep 5
done
# hard gate: never publish an image without sshd baked in
rpm -q openssh-server >/dev/null
mkdir -p /etc/ssh/sshd_config.d
printf "PermitRootLogin yes\nPasswordAuthentication yes\n" > /etc/ssh/sshd_config.d/99-vpsmgr.conf
systemctl enable sshd
# slim the published image: drop dnf caches and logs
dnf clean all 2>/dev/null || true
rm -rf /var/cache/dnf /var/log/* /tmp/* /var/tmp/* 2>/dev/null || true
# Drop the baked-in machine-id so every container boots its own: a shared
# machine-id means a shared DHCPv6 DUID, which breaks dnsmasq lease renewals
# and drops the container global IPv6 at the 1h lease mark.
rm -f /etc/machine-id /var/lib/dbus/machine-id 2>/dev/null || true
# IPv6 for RHEL containers is kernel-managed: take the RA default route but
# not the on-link prefix (peers route via the host), ignore redirects, and let
# a boot unit apply the deterministic primary /128 that vpsmgr writes to
# /etc/vpsmgr-ipv6.conf (NetworkManager ships ipv6.method=ignore and would
# otherwise fight these settings). Double-quoted printf is required here: the
# build runs inside a single-quoted sh -c block, so a single quote in the
# format would close it early and the bake would silently never run.
printf "net.ipv6.conf.eth0.accept_ra = 1\nnet.ipv6.conf.eth0.accept_ra_pinfo = 0\nnet.ipv6.conf.eth0.accept_redirects = 0\n" > /etc/sysctl.d/99-vpsmgr-ipv6.conf
# Fully static IPv6: the helper waits for the panel-written conf, temporarily
# enables RA to learn the gateway, then disables RA and pins the deterministic
# /128 + a static default route. Waits for DAD and exits 1 on any failure so
# the unit (Restart=on-failure) retries.
# NOTE: the whole bake runs inside a single-quoted sh -c block, so no single
# quotes may appear in the generated files.
printf "#!/bin/sh\nfor i in \$(seq 1 150); do\n  [ -f /etc/vpsmgr-ipv6.conf ] && break\n  sleep 2\ndone\n[ -f /etc/vpsmgr-ipv6.conf ] || exit 1\nADDR=\$(cat /etc/vpsmgr-ipv6.conf)\nADDR_BARE=\${ADDR%%/*}\nsysctl -w net.ipv6.conf.eth0.accept_ra=1 >/dev/null 2>&1\nsysctl -w net.ipv6.conf.all.accept_ra=1 >/dev/null 2>&1\nGW=\"\"\nfor i in \$(seq 1 60); do\n  GW=\$(ip -6 route show dev eth0 | awk \"/default/{print \\\$3; exit}\")\n  [ -n \"\$GW\" ] && break\n  sleep 2\ndone\n[ -n \"\$GW\" ] || exit 1\nsysctl -w net.ipv6.conf.eth0.accept_ra=0 >/dev/null 2>&1\nsysctl -w net.ipv6.conf.all.accept_ra=0 >/dev/null 2>&1\nip -6 addr replace \"\$ADDR\" dev eth0 2>/dev/null\nip -6 addr show dev eth0 scope global | grep inet6 | while read -r line; do\n  a=\$(echo \"\$line\" | awk \"{print \\\$2}\" | cut -d/ -f1)\n  [ \"\$a\" != \"\$ADDR_BARE\" ] && ip -6 addr del \"\$a\" dev eth0 2>/dev/null\ndone\nfor i in \$(seq 1 30); do\n  ip -6 addr show dev eth0 scope global | grep -q tentative || break\n  sleep 1\ndone\nip -6 route flush dev eth0 2>/dev/null\nip -6 route add default via \"\$GW\" dev eth0 src \"\$ADDR_BARE\" || exit 1\nip -6 route flush cache 2>/dev/null\n" > /usr/local/sbin/vpsmgr-ipv6
chmod +x /usr/local/sbin/vpsmgr-ipv6
printf "[Unit]\nDescription=vpsmgr IPv6 primary address\nAfter=network-online.target\nWants=network-online.target\n[Service]\nType=oneshot\nExecStart=/usr/local/sbin/vpsmgr-ipv6\nRemainAfterExit=yes\nRestart=on-failure\nRestartSec=10\n[Install]\nWantedBy=multi-user.target\n" > /etc/systemd/system/vpsmgr-ipv6.service
systemctl enable vpsmgr-ipv6.service >/dev/null 2>&1 || true'; then
    incus stop "$NAME" --timeout=30 || true
    if incus publish "$NAME" --alias "$IMAGE"; then
      incus delete --force "$NAME" || true
      # keep only the modified image — the base was a build intermediate
      if incus image delete "$BASE_ALIAS" >/dev/null 2>&1; then
        log "removed base image $BASE_ALIAS (only $IMAGE kept)"
      else
        log "  warn: could not remove base image $BASE_ALIAS"
      fi
      log "image published: $IMAGE"
    else
      log "  warn: publish FAILED — $IMAGE NOT built (base image kept; re-run to retry)"
      incus delete --force "$NAME" >/dev/null 2>&1 || true
      exit 1
    fi
  else
    log "  warn: install in builder failed; nothing built"
    incus delete --force "$NAME" >/dev/null 2>&1 || true
    incus image delete "$BASE_ALIAS" >/dev/null 2>&1 || true
  fi
else
  log "  warn: could not launch builder; nothing built"
fi

echo "[60] done"
