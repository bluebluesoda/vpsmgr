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
# IPv6 for RHEL containers is configured at runtime by the panel: the
# deterministic /128 + gateway are declared as a STATIC IPv6 connection
# (nmcli ipv6.method manual), which makes NetworkManager own the stack
# (accept_ra=0, no SLAAC, atomic address+route) with no extra daemon.
#'; then
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
