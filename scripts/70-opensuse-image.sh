#!/usr/bin/env bash
# 70-opensuse-image.sh — OPTIONAL: build an openSUSE Leap 16 "sshd + universal
# tooling" image so users can reinstall their container with something other
# than the default Debian 13. Run as root AFTER install.sh, only when you
# actually want the extra image (a toy panel should not build it on every box).
#
#   sudo bash scripts/70-opensuse-image.sh
#
# Same hygiene as 50-image.sh: sshd + common tools baked in, zypper caches and
# logs cleaned before publishing, base image deleted afterwards.
set -uo pipefail


REMOTE_ALIAS=images:opensuse/16.0
IMAGE=vpsmgr/opensuse-sshd
BASE_ALIAS=vpsmgr-opensuse-16

log(){ echo "[70] $*"; }

incus info >/dev/null 2>&1 || { echo "[70] error: Incus not ready" >&2; exit 1; }

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
NAME=tmp-opensuse-builder
incus delete --force "$NAME" >/dev/null 2>&1 || true
if incus launch "$BASE_ALIAS" "$NAME"; then
  # wait until usable
  for i in $(seq 1 60); do
    if incus exec "$NAME" -- /bin/true >/dev/null 2>&1; then break; fi
    sleep 2
  done
  # openSUSE brings eth0 up via systemd-networkd, which lags the Incus agent by
  # a few seconds: running zypper before DHCP has written resolv.conf makes it
  # die with "Couldn't resolve". Wait until DNS answers, or the builder install
  # fails and a broken image gets published.
  DNS_OK=
  for i in $(seq 1 30); do
    if incus exec "$NAME" -- getent hosts download.opensuse.org >/dev/null 2>&1; then
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
# ca-certificates or HTTPS fails; bind-utils is the openSUSE dig/nslookup
# package. zypper is retried: the mirror can flap on a slow uplink right after
# boot.
for attempt in 1 2 3; do
  zypper --non-interactive install --no-recommends \
    openssh-server ca-certificates curl wget less bind-utils openssh-clients unzip nano \
    && break
  sleep 5
done
# hard gate: never publish an image without sshd baked in
rpm -q openssh-server >/dev/null
mkdir -p /etc/ssh/sshd_config.d
printf "PermitRootLogin yes\nPasswordAuthentication yes\n" > /etc/ssh/sshd_config.d/99-vpsmgr.conf
systemctl enable sshd
# slim the published image: drop zypper caches and logs
zypper clean --all 2>/dev/null || true
rm -rf /var/cache/zypp /var/log/* /tmp/* /var/tmp/* 2>/dev/null || true
# Drop the baked-in machine-id so every container boots its own: a shared
# machine-id means a shared DHCPv6 DUID, which breaks dnsmasq lease renewals
# and drops the container global IPv6 at the 1h lease mark.
rm -f /etc/machine-id /var/lib/dbus/machine-id 2>/dev/null || true
# IPv6 for openSUSE containers is configured at runtime by the panel: Leap 16
# ships systemd-networkd by default, so the panel networkd branch applies
# (static /128 + fe80::1 gateway, no SLAAC/DHCPv6) — nothing to bake here.
'; then
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

echo "[70] done"
