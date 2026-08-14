#!/usr/bin/env bash
# 50-image.sh — pre-pull Debian 13 and build a slim local "Debian 13 + sshd"
# image with the universal tooling baked in (curl/wget/ca-certificates/less/
# bind9-dnsutils/openssh-client/unzip/nano). apt lists/archives and logs are
# cleaned inside the builder before publishing, and the Debian base image (a
# build intermediate only) is deleted afterwards so only the modified
# vpsmgr/debian-sshd stays on disk.
set -uo pipefail
export PATH="$PATH:/snap/bin"

log(){ echo "[50] $*"; }

lxc info >/dev/null 2>&1 || { echo "[50] error: LXD not ready" >&2; exit 1; }

# --- base image ---
if lxc image show vpsmgr-debian-13 >/dev/null 2>&1; then
  log "base image vpsmgr-debian-13 already present"
else
  log "pulling images:debian/13 (fallback images:debian/trixie)..."
  if ! lxc image copy images:debian/13 local: --alias vpsmgr-debian-13; then
    log "  debian/13 alias failed, trying debian/trixie"
    lxc image copy images:debian/trixie local: --alias vpsmgr-debian-13 \
      || log "  warn: image pull failed — add-user needs network and will retry"
  fi
fi

# --- prebuilt sshd image ---
if lxc image show vpsmgr/debian-sshd >/dev/null 2>&1; then
  log "sshd image vpsmgr/debian-sshd already present"
  exit 0
fi

if ! lxc image show vpsmgr-debian-13 >/dev/null 2>&1; then
  log "  warn: base image unavailable, skipping sshd image build (add-user will install sshd on the fly)"
  exit 0
fi

log "building sshd image (this takes a few minutes)..."
NAME=tmp-sshd-builder
lxc delete --force "$NAME" >/dev/null 2>&1 || true
if lxc launch vpsmgr-debian-13 "$NAME"; then
  # wait until usable
  for i in $(seq 1 60); do
    if lxc exec "$NAME" -- /bin/true >/dev/null 2>&1; then break; fi
    sleep 2
  done
  # DNS can lag agent readiness; running apt before it is up fails the update
  # and, without a gate, a broken sshd image would still be published.
  DNS_OK=
  for i in $(seq 1 30); do
    if lxc exec "$NAME" -- getent hosts deb.debian.org >/dev/null 2>&1; then
      DNS_OK=1; break
    fi
    sleep 2
  done
  if [ -z "$DNS_OK" ]; then
    log "  warn: builder never got working DNS; skipping sshd image (add-user will install sshd on the fly)"
    lxc delete --force "$NAME" >/dev/null 2>&1 || true
  elif lxc exec "$NAME" -- sh -c 'export DEBIAN_FRONTEND=noninteractive
set -e
# universal user tooling (small, every container hits these): curl/wget need
# ca-certificates or HTTPS fails; dnsutils is bind9-dnsutils on Debian 13.
apt-get update -qq && apt-get install -y -qq openssh-server ca-certificates curl wget less bind9-dnsutils openssh-client unzip nano
# hard gate: never publish an image without sshd baked in
command -v sshd >/dev/null || { echo "sshd install failed" >&2; exit 1; }
mkdir -p /etc/ssh/sshd_config.d
printf "PermitRootLogin yes\nPasswordAuthentication yes\n" > /etc/ssh/sshd_config.d/99-vpsmgr.conf
systemctl enable ssh
# Containers route peer IPv6 through the host, so they must not treat the
# parent prefix as on-link. Tell systemd-networkd to skip RA on-link and route
# prefixes; peer traffic then always goes to the default gateway. DHCPv6 is
# off: its dynamic address would fall outside the routed /112 (dropped by
# ipv6_filtering), and on reinstall the stale DHCPv6 lease for the
# deterministic block would hand a fresh container that useless address.
if [ -f /etc/systemd/network/eth0.network ]; then
  sed -i "s/^DHCP=true\$/DHCP=ipv4/" /etc/systemd/network/eth0.network
  printf "\n[IPv6AcceptRA]\nUseOnLinkPrefix=false\nUseRoutePrefix=false\nUseAutonomousPrefix=false\nDHCPv6Client=no\n" >> /etc/systemd/network/eth0.network
fi
# slim the published image: drop apt lists/archives and logs. Without this the
# image balloons ~70MiB beyond just openssh (apt lists alone are ~50MiB).
apt-get clean 2>/dev/null || true
rm -rf /var/lib/apt/lists/* 2>/dev/null || true
rm -rf /var/log/* 2>/dev/null || true
rm -rf /tmp/* /var/tmp/* 2>/dev/null || true
# A machine-id baked into the image would be shared by every container:
# systemd-networkd derives its DHCPv6 DUID from it, so two containers look
# like the same DHCPv6 client and dnsmasq lease renewals break (the global
# IPv6 drops at the 1h lease mark). Drop it so each container generates its
# own on first boot.
rm -f /etc/machine-id /var/lib/dbus/machine-id 2>/dev/null || true'; then
    lxc stop "$NAME" --timeout=30 || true
    if lxc publish "$NAME" --alias vpsmgr/debian-sshd; then
      lxc delete --force "$NAME" || true
      # keep only the modified image — the Debian base was a build intermediate
      # and is never used to launch containers (fallback is the remote images:).
      if lxc image delete vpsmgr-debian-13 >/dev/null 2>&1; then
        log "removed base image vpsmgr-debian-13 (only vpsmgr/debian-sshd kept)"
      else
        log "  warn: could not remove base image vpsmgr-debian-13"
      fi
      log "sshd image published: vpsmgr/debian-sshd"
    else
      log "  warn: publish FAILED — sshd image NOT built (base image kept; re-run to retry)"
      lxc delete --force "$NAME" >/dev/null 2>&1 || true
      exit 1
    fi
  else
    log "  warn: sshd install in builder failed; add-user will install sshd on the fly"
    lxc delete --force "$NAME" >/dev/null 2>&1 || true
  fi
else
  log "  warn: could not launch builder; add-user will install sshd on the fly"
fi

echo "[50] image ready"
