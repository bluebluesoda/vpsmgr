#!/usr/bin/env bash
# eol-debian11-image.sh — OPTIONAL: build a slim "Debian 11 (bullseye, EOL)
# + sshd" image on top of images:debian/11. Run as root AFTER install.sh,
# only when you actually want to offer Debian 11 containers (an EOL release
# with no security updates — never a default). Mirrors the slim tooling set
# from 50-image.sh.
#
#   sudo bash scripts/eol-debian11-image.sh
#
# Hygiene like 50/80: apt lists/archives and logs cleaned inside the builder
# before publishing, and the Debian 11 base image (a build intermediate only)
# is deleted afterwards so only vpsmgr/debian-11-sshd stays on disk.
set -uo pipefail

REMOTE_ALIAS=images:debian/11
FALLBACK_ALIAS=images:debian/bullseye
IMAGE=vpsmgr/debian-11-sshd
BASE_ALIAS=vpsmgr-debian-11

log(){ echo "[eol-debian11] $*"; }

incus info >/dev/null 2>&1 || { echo "[eol-debian11] error: Incus not ready" >&2; exit 1; }

if incus image show "$IMAGE" >/dev/null 2>&1; then
  log "image $IMAGE already present"
  exit 0
fi

if ! incus image list "$BASE_ALIAS" --format=csv 2>/dev/null | grep -q .; then
  log "pulling $REMOTE_ALIAS (fallback $FALLBACK_ALIAS, deleted after build)..."
  if ! incus image copy "$REMOTE_ALIAS" local: --alias "$BASE_ALIAS"; then
    log "  retrying with $FALLBACK_ALIAS"
    incus image copy "$FALLBACK_ALIAS" local: --alias "$BASE_ALIAS" \
      || { log "  warn: image pull failed — nothing built"; exit 0; }
  fi
else
  log "base image $BASE_ALIAS already present"
fi

log "building $IMAGE (this takes a few minutes)..."
NAME=tmp-debian11-builder
incus delete --force "$NAME" >/dev/null 2>&1 || true
if incus launch "$BASE_ALIAS" "$NAME"; then
  # wait until usable
  for i in $(seq 1 60); do
    if incus exec "$NAME" -- /bin/true >/dev/null 2>&1; then break; fi
    sleep 2
  done
  # DNS can lag agent readiness; running apt before it is up fails the update
  # and, without a gate, a broken image would still be published.
  DNS_OK=
  for i in $(seq 1 30); do
    if incus exec "$NAME" -- getent hosts deb.debian.org >/dev/null 2>&1; then
      DNS_OK=1; break
    fi
    sleep 2
  done
  if [ -z "$DNS_OK" ]; then
    log "  warn: builder never got working DNS; nothing built"
    incus delete --force "$NAME" >/dev/null 2>&1 || true
    incus image delete "$BASE_ALIAS" >/dev/null 2>&1 || true
  elif incus exec "$NAME" -- sh -c 'export DEBIAN_FRONTEND=noninteractive
set -e
# universal user tooling (same set as 50-image.sh). bind9-dnsutils exists in
# bullseye (split from bind9), so the debian/13 package list works as-is.
apt-get update -qq && apt-get install -y -qq openssh-server ca-certificates curl wget less bind9-dnsutils openssh-client unzip nano
# hard gate: never publish an image without sshd baked in
command -v sshd >/dev/null || { echo "sshd install failed" >&2; exit 1; }
mkdir -p /etc/ssh/sshd_config.d
printf "PermitRootLogin yes\nPasswordAuthentication yes\n" > /etc/ssh/sshd_config.d/99-vpsmgr.conf
systemctl enable ssh
# Containers route peer IPv6 through the host, so they must not treat the
# parent prefix as on-link. Debian 11 ships systemd 247, which supports the
# full [IPv6AcceptRA] section (UseOnLinkPrefix/UseRoutePrefix/
# UseAutonomousPrefix/DHCPv6Client) — the same fix as 50-image.sh applies.
if [ -f /etc/systemd/network/eth0.network ]; then
  sed -i "s/^DHCP=true$/DHCP=ipv4/" /etc/systemd/network/eth0.network
  printf "\n[IPv6AcceptRA]\nUseOnLinkPrefix=false\nUseRoutePrefix=false\nUseAutonomousPrefix=false\nDHCPv6Client=no\n" >> /etc/systemd/network/eth0.network
fi
# vpsmgr TCP tuning: BBR congestion control, larger autotuned buffers, window
# scaling on, and no slow-start reset after idle (better for long-lived SSH and
# tunnel connections). Baked into /etc/sysctl.conf so it applies at container
# boot; sysctl -p is a best-effort apply during the build (an unprivileged
# container may deny net.* sysctls — that is non-fatal).
cat >> /etc/sysctl.conf <<SYSCTL

# vpsmgr TCP tuning
net.ipv4.tcp_congestion_control = bbr
net.ipv4.tcp_rmem = 8192 262144 4194304
net.ipv4.tcp_wmem = 4096 16384 4194304
net.ipv4.tcp_window_scaling = 1
net.ipv4.tcp_slow_start_after_idle = 0
SYSCTL
sysctl -p 2>/dev/null || true

# slim the published image: drop apt lists/archives and logs.
apt-get clean 2>/dev/null || true
rm -rf /var/lib/apt/lists/* 2>/dev/null || true
rm -rf /var/log/* 2>/dev/null || true
rm -rf /tmp/* /var/tmp/* 2>/dev/null || true
# A machine-id baked into the image would be shared by every container:
# systemd-networkd derives its DHCPv6 DUID from it, so two containers look
# like the same DHCPv6 client and dnsmasq lease renewals break. Drop it so
# each container generates its own on first boot.
rm -f /etc/machine-id /var/lib/dbus/machine-id 2>/dev/null || true'; then
    incus stop "$NAME" --timeout=30 || true
    if incus publish "$NAME" --alias "$IMAGE"; then
      incus delete --force "$NAME" || true
      # keep only the modified image — the Debian 11 base was a build
      # intermediate and is never used to launch containers.
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

echo "[eol-debian11] done"
