#!/usr/bin/env bash
# 90-arch-image.sh — OPTIONAL: build an "Arch Linux" image so users can
# reinstall their container with something other than the default Debian 13.
# Run as root AFTER install.sh, only when you actually want it (a toy panel
# should not build it on every box).
#
#   sudo bash scripts/90-arch-image.sh
#
# Arch is a ROLLING release, so unlike the other image scripts this one
# deliberately does NOT skip when the image already exists: every run deletes
# the old vpsmgr/arch-sshd, re-pulls the latest upstream Arch base and rebuilds
# a fresh snapshot. The published image's description records the build's
# version code — "Archlinux<YYMM>", e.g. Archlinux2607 for a 2026-07 build —
# so the age of the snapshot is visible in `incus image show vpsmgr/arch-sshd`.
#
# Same hygiene as the others: openssh + common tools baked in, pacman caches
# and logs cleaned before publishing, and the base image (a build intermediate
# only) is deleted afterwards so only vpsmgr/arch-sshd stays on disk.
set -uo pipefail


REMOTE_ALIAS=images:archlinux/current
IMAGE=vpsmgr/arch-sshd
BASE_PREFIX=vpsmgr-arch
VER="Archlinux$(date +%y%m)"

log(){ echo "[90] $*"; }

incus info >/dev/null 2>&1 || { echo "[90] error: Incus not ready" >&2; exit 1; }

# Rolling release: drop the existing image so the next publish gets a fresh
# alias (incus refuses to publish over an existing alias). If a running
# container still uses it, deleting fails — tell the operator instead of
# silently keeping a stale snapshot.
if incus image show "$IMAGE" >/dev/null 2>&1; then
  if incus image delete "$IMAGE" >/dev/null 2>&1; then
    log "removed existing $IMAGE"
  else
    echo "[90] error: could not delete $IMAGE (is a container using it?) — rebuild aborted" >&2
    exit 1
  fi
fi

# Clean up old build-intermediate bases (each run pulls a fresh upstream), so
# they don't accumulate on disk. First CSV field is the fingerprint.
while IFS= read -r line; do
  fp="${line%%,*}"
  if [ -n "$fp" ]; then
    incus image delete "$fp" >/dev/null 2>&1 || true
  fi
done < <(incus image list "$BASE_PREFIX-" --format=csv 2>/dev/null)

BASE_ALIAS="$BASE_PREFIX-$(date +%y%m)"
log "pulling $REMOTE_ALIAS (base $BASE_ALIAS, deleted after build)..."
if ! incus image copy "$REMOTE_ALIAS" local: --alias "$BASE_ALIAS"; then
  log "  warn: image pull failed — nothing built"
  exit 0
fi

log "building $IMAGE (rolling snapshot $VER, this takes a few minutes)..."
NAME=tmp-arch-builder
incus delete --force "$NAME" >/dev/null 2>&1 || true
if incus launch "$BASE_ALIAS" "$NAME"; then
  # wait until usable
  for i in $(seq 1 60); do
    if incus exec "$NAME" -- /bin/true >/dev/null 2>&1; then break; fi
    sleep 2
  done
  # Arch's pacman needs working DNS; gate like the other scripts so a broken
  # image is never published.
  DNS_OK=
  for i in $(seq 1 30); do
    if incus exec "$NAME" -- getent hosts archlinux.org >/dev/null 2>&1; then
      DNS_OK=1; break
    fi
    sleep 2
  done
  if [ -z "$DNS_OK" ]; then
    log "  warn: builder never got working DNS; nothing built"
    incus delete --force "$NAME" >/dev/null 2>&1 || true
    incus image delete "$BASE_ALIAS" >/dev/null 2>&1 || true
  elif incus exec "$NAME" -- sh -c 'set -e
# universal user tooling (same idea as the Debian image): openssh provides
# both sshd AND the ssh client on Arch (there is no openssh-client package),
# curl/wget need ca-certificates or HTTPS fails, bind-tools is dig/nslookup.
# base-devel is left out to keep the image slim. pacman is retried: the mirror
# can flap right after boot. -Syu upgrades the rolling base to current first;
# --needed skips reinstalling packages the base already has.
for attempt in 1 2 3; do
  pacman -Syu --needed --noconfirm openssh ca-certificates curl wget less bind-tools unzip nano \
    && break
  sleep 5
done
# apt-autoremove equivalent: drop packages nothing depends on anymore (after a
# -Syu a rolling base can leave orphans). Never fails the build on its own.
if orphans=$(pacman -Qtdq 2>/dev/null); then
  pacman -Rns --noconfirm $orphans 2>/dev/null || true
fi
# hard gate: never publish an image without sshd baked in (mgr.Provision does
# NOT rewrite sshd config for vpsmgr/* images)
command -v sshd >/dev/null || { echo "sshd install failed" >&2; exit 1; }
mkdir -p /etc/ssh/sshd_config.d
printf "PermitRootLogin yes\nPasswordAuthentication yes\n" > /etc/ssh/sshd_config.d/99-vpsmgr.conf
systemctl enable sshd
# slim the published image: drop pacman caches (packages + sync dbs), logs.
# A bare rm avoids the pacman -Scc interactive prompts in a non-tty exec.
rm -rf /var/cache/pacman/pkg/* /var/lib/pacman/sync/* /var/log/* /tmp/* /var/tmp/* 2>/dev/null || true
# A machine-id baked into the image would be shared by every container:
# systemd-networkd derives its DHCPv6 DUID from it, so two containers look
# like the same DHCPv6 client and dnsmasq lease renewals break (the global
# IPv6 drops at the 1h lease mark). Drop it so each container generates its
# own on first boot.
rm -f /etc/machine-id /var/lib/dbus/machine-id 2>/dev/null || true
# IPv6 for Arch containers is configured at runtime by the panel (Arch ships
# systemd-networkd, so the panel networkd branch applies) — nothing to bake.
'; then
    incus stop "$NAME" --timeout=30 || true
    if incus publish "$NAME" --alias "$IMAGE" "description=$VER"; then
      incus delete --force "$NAME" || true
      # keep only the modified image — the base was a build intermediate
      if incus image delete "$BASE_ALIAS" >/dev/null 2>&1; then
        log "removed base image $BASE_ALIAS (only $IMAGE kept)"
      else
        log "  warn: could not remove base image $BASE_ALIAS"
      fi
      log "image published: $IMAGE ($VER)"
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

echo "[90] done"
