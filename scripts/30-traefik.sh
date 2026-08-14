#!/usr/bin/env bash
# 30-traefik.sh — install Traefik binary + static config + systemd service.
set -uo pipefail

log(){ echo "[30] $*"; }
die(){ echo "[30] error: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Private scratch dir (root-only, unique): never write downloads to a fixed
# /tmp path another process could pre-create or race (review P2-9).
SCRATCH="$(mktemp -d /tmp/vpsmgr-traefik.XXXXXX)"
trap 'rm -rf "$SCRATCH"' EXIT

# fixed version + per-arch download URLs (no bundled binaries in the repo)
TRAEFIK_VERSION=3.3.5
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  TARCH=amd64 ;;
  aarch64) TARCH=arm64 ;;
  *) die "unsupported architecture: $ARCH" ;;
esac
TRAEFIK_URL="https://github.com/traefik/traefik/releases/download/v${TRAEFIK_VERSION}/traefik_v${TRAEFIK_VERSION}_linux_${TARCH}.tar.gz"

# Defensive dependency check (00-check.sh already installs these on a full
# install; this covers running 30-traefik.sh standalone on a bare Debian).
for p in curl tar ca-certificates; do
  if ! dpkg -s "$p" >/dev/null 2>&1; then
    log "installing $p (needed for the traefik download)"
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$p"
  fi
done

if [[ ! -x /usr/local/bin/traefik ]]; then
  log "downloading traefik ${TRAEFIK_VERSION} (${TARCH})"
  log "  ${TRAEFIK_URL}"
  curl -fsSL -o "$SCRATCH/traefik.tar.gz" "$TRAEFIK_URL" || die "traefik download failed"
  tar -xzf "$SCRATCH/traefik.tar.gz" -C "$SCRATCH" traefik
  cp "$SCRATCH/traefik" /usr/local/bin/traefik
  chmod 755 /usr/local/bin/traefik
  log "installed /usr/local/bin/traefik"
fi
log "traefik version: $(/usr/local/bin/traefik version 2>/dev/null | awk '/Version:/{print $2; exit}')"

mkdir -p /etc/traefik/dynamic
if [[ ! -f /etc/traefik/traefik.yaml ]]; then
  cp "$ROOT/configs/traefik.yaml" /etc/traefik/traefik.yaml
  log "wrote /etc/traefik/traefik.yaml"
fi

if [[ ! -f /etc/systemd/system/traefik.service ]]; then
  # Fresh install: run Traefik as a dedicated unprivileged user that can only
  # read /etc/traefik (config + dynamic rules) and bind ports 80/443. The panel
  # (unprivileged 'vps') writes the dynamic files, so the two groups cross-link:
  # vps joins traefik (to traverse /etc/traefik) and traefik joins vps (to read
  # the dynamic dir, which vps owns). Existing installs are untouched.
  if ! id -u traefik >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin traefik
    log "created unprivileged user 'traefik'"
  fi
  if id -u vps >/dev/null 2>&1; then
    usermod -aG traefik vps >/dev/null 2>&1 || true
    usermod -aG vps traefik >/dev/null 2>&1 || true
    log "cross-linked groups: vps<->traefik (panel writes dynamic, traefik reads it)"
  fi
  chown -R root:traefik /etc/traefik
  chmod 750 /etc/traefik
  # The panel user 'vps' is created later by 40-panel.sh / `vps install`; when
  # it does not exist yet, skip the chown here and let ensureVPSUser do it
  # (it re-chowns the dynamic dir on every install).
  if id -u vps >/dev/null 2>&1; then
    chown vps:vps /etc/traefik/dynamic
  fi
  chmod 750 /etc/traefik/dynamic
  chmod 640 /etc/traefik/traefik.yaml
  cp "$ROOT/configs/systemd/traefik.service" /etc/systemd/system/traefik.service
  systemctl daemon-reload
  log "installed traefik.service (unprivileged)"
fi

# v4 forwarding off (adopted config or VPSMGR_V4_FORWARD=0, IPv6-only box):
# install traefik but keep it DISABLED — the domain proxy is not offered.
# Config is kept so a later `vps config set net.v4_forward true` re-enables it.
V4_FWD="${VPSMGR_V4_FORWARD:-1}"
if [[ "$V4_FWD" == "1" ]]; then
  systemctl enable --now traefik >/dev/null 2>&1 || die "cannot start traefik"
  sleep 1
  if systemctl is-active traefik >/dev/null 2>&1; then
    log "traefik running"
  else
    systemctl status traefik --no-pager | tail -5
    die "traefik failed to start"
  fi
else
  systemctl disable --now traefik >/dev/null 2>&1 || true
  log "v4 forwarding off — traefik installed but disabled (domains kept)"
fi

echo "[30] traefik ready"
