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
TRAEFIK_VERSION=3.7.12
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
  # read /etc/traefik (config + dynamic rules) and bind ports 80/443. The
  # dynamic dir is world-readable (traefik reads it as any other user); the
  # panel installer assigns its ownership after creating the vps user.
  if ! id -u traefik >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin traefik
    log "created unprivileged user 'traefik'"
  fi
  chown -R root:traefik /etc/traefik
  chmod 755 /etc/traefik
  chmod 755 /etc/traefik/dynamic
  chmod 640 /etc/traefik/traefik.yaml
  cp "$ROOT/configs/systemd/traefik.service" /etc/systemd/system/traefik.service
  systemctl daemon-reload
  log "installed traefik.service (unprivileged)"
fi

# Effective net.traefik toggle: forced by the installer (VPSMGR_TRAEFIK=0 when
# 80/443 is already taken, exported by install.sh from the 00-check marker),
# else adopted from an existing config, else default (enabled).
TRAEFIK_CFG=
if [[ -n "${VPSMGR_TRAEFIK:-}" ]]; then
  case "${VPSMGR_TRAEFIK,,}" in
    1|true)  TRAEFIK_CFG=true ;;
    0|false) TRAEFIK_CFG=false ;;
    *) die "VPSMGR_TRAEFIK must be 1/0/true/false (got '$VPSMGR_TRAEFIK')" ;;
  esac
elif [[ -f /etc/vpsmgr/config.yaml ]]; then
  TRAEFIK_CFG=$(grep -E '^\s+traefik:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
fi

# Traefik disabled (port 80/443 conflict forced off, or adopted config with
# net.traefik false): install the binary but keep it DISABLED — the domain
# proxy is not offered. Config is kept so a later `vps config set net.traefik
# true` re-enables it. Also covers v4_forward off (IPv6-only box): no domain
# proxy there either.
if [[ "$TRAEFIK_CFG" == "false" || "$TRAEFIK_CFG" == "0" ]]; then
  systemctl disable --now traefik >/dev/null 2>&1 || true
  log "net.traefik false — traefik installed but disabled (not started/autostarted; domains kept)"
elif [[ "$TRAEFIK_CFG" == "true" || "$TRAEFIK_CFG" == "1" || "${VPSMGR_V4_FORWARD:-1}" == "1" || "${VPSMGR_V4_FORWARD:-1}" == "true" ]]; then
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
