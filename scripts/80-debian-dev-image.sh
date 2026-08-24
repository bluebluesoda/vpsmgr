#!/usr/bin/env bash
# 80-debian-dev-image.sh — OPTIONAL: build a "Debian 13 dev" image on top of
# images:debian/13 with a full dev toolchain baked in: the universal
# sshd/tooling set from 50-image.sh, python3 + pip + venv,
# git/sqlite3/ripgrep/jq/gh/sshpass, Go 1.26.7 and nvm + Node.js 24. Run as
# root AFTER install.sh, only when you actually want the heavier dev image (a
# toy panel should not build it on every box).
#
#   sudo bash scripts/80-debian-dev-image.sh
#
# Same hygiene as 50-image.sh: apt lists/archives and logs cleaned inside the
# builder before publishing, and the Debian base image (a build intermediate
# only) is deleted afterwards so only vpsmgr/debian-dev-sshd stays on disk.
set -uo pipefail


REMOTE_ALIAS=images:debian/13
FALLBACK_ALIAS=images:debian/trixie
IMAGE=vpsmgr/debian-dev-sshd
BASE_ALIAS=vpsmgr-debian-13-dev

log(){ echo "[80] $*"; }

incus info >/dev/null 2>&1 || { echo "[80] error: Incus not ready" >&2; exit 1; }

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

log "building $IMAGE (this takes a while: apt + Go + Node downloads)..."
NAME=tmp-debian-dev-builder
incus delete --force "$NAME" >/dev/null 2>&1 || true
if incus launch "$BASE_ALIAS" "$NAME"; then
  # wait until usable
  for i in $(seq 1 60); do
    if incus exec "$NAME" -- /bin/true >/dev/null 2>&1; then break; fi
    sleep 2
  done
  # DNS can lag agent readiness; running apt before it is up fails the update
  # and, without a gate, a broken dev image would still be published.
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
  elif incus exec "$NAME" -- bash -c 'set -e
export DEBIAN_FRONTEND=noninteractive
# universal user tooling (same set as 50-image.sh, but with
# --no-install-recommends so the dev image stays lean)
apt-get update -qq
apt-get install -y -qq --no-install-recommends openssh-server ca-certificates curl wget less bind9-dnsutils openssh-client unzip nano
# dev toolchain. --no-install-recommends means pip/venv are NOT pulled in by
# python3 (Debian disables ensurepip), so they are explicit packages here.
apt-get install -y -qq --no-install-recommends python3 python3-pip python3-venv git sqlite3 ripgrep jq gh sshpass
# hard gate: never publish an image without sshd baked in (mgr.Provision does
# NOT rewrite sshd config for vpsmgr/* images)
command -v sshd >/dev/null || { echo "sshd install failed" >&2; exit 1; }
mkdir -p /etc/ssh/sshd_config.d
printf "PermitRootLogin yes\nPasswordAuthentication yes\n" > /etc/ssh/sshd_config.d/99-vpsmgr.conf
systemctl enable ssh
# same IPv6 on-link fix as 50-image.sh: containers route peer IPv6 through the
# host, so the parent prefix must not be treated as on-link
if [ -f /etc/systemd/network/eth0.network ]; then
  sed -i "s/^DHCP=true$/DHCP=ipv4/" /etc/systemd/network/eth0.network
  printf "\n[IPv6AcceptRA]\nUseOnLinkPrefix=false\nUseRoutePrefix=false\nUseAutonomousPrefix=false\nDHCPv6Client=no\n" >> /etc/systemd/network/eth0.network
fi
# --- Go 1.26.7 (manual install, arch-aware, checksum-verified) ---
GO_VERSION=1.26.7
case "$(uname -m)" in
  x86_64)  GO_ARCH=amd64 ;;
  aarch64) GO_ARCH=arm64 ;;
  *) echo "unsupported arch for Go: $(uname -m)" >&2; exit 1 ;;
esac
GO_TAR="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
curl -fsSL -O "https://go.dev/dl/${GO_TAR}"
# go.dev does not serve a plain .sha256 file for the tarball (the URL
# redirects to the download page), so the checksum is read from the dl JSON
# API and compared with jq.
GO_SHA=$(curl -fsSL "https://go.dev/dl/?mode=json" | jq -r --arg v "go${GO_VERSION}" --arg f "${GO_TAR}" ".[] | select(.version == \$v) | .files[] | select(.filename == \$f) | .sha256")
[ -n "$GO_SHA" ] && [ "$(sha256sum "${GO_TAR}" | cut -d" " -f1)" = "$GO_SHA" ] || { echo "go checksum mismatch for ${GO_TAR}" >&2; exit 1; }
rm -rf /usr/local/go
tar -C /usr/local -xzf "${GO_TAR}"
rm -f "${GO_TAR}"
cat > /etc/profile.d/go.sh <<EOF
export PATH=\$PATH:/usr/local/go/bin
export GOPATH=\$HOME/go
export PATH=\$PATH:\$GOPATH/bin
EOF
chmod 644 /etc/profile.d/go.sh
# --- nvm + Node.js 24 (manual install; nvm needs bash, so the builder runs
# under bash -c). The container only ever logs in as root, so baking into
# /root/.nvm covers every user. ---
export NVM_VERSION=v0.40.7
export NODE_VERSION=24
export NVM_DIR=/root/.nvm
curl -fsSL -o /tmp/nvm-install.sh "https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/install.sh"
NVM_DIR="${NVM_DIR}" bash /tmp/nvm-install.sh
rm -f /tmp/nvm-install.sh
. "${NVM_DIR}/nvm.sh"
nvm install "${NODE_VERSION}"
nvm use "${NODE_VERSION}"
nvm alias default "${NODE_VERSION}"
NODE_BIN_DIR="$(dirname "$(command -v node)")"
cat > /etc/profile.d/nodejs.sh <<EOF
export NVM_DIR="\$HOME/.nvm"
[ -s "\$NVM_DIR/nvm.sh" ] && . "\$NVM_DIR/nvm.sh"
export PATH="${NODE_BIN_DIR}:\$PATH"
EOF
chmod 644 /etc/profile.d/nodejs.sh
# hard gates: the dev image must actually ship go + node (PATH here does not
# yet include /usr/local/go/bin, so check the binaries directly)
[ -x /usr/local/go/bin/go ] || { echo "go install failed" >&2; exit 1; }
[ -n "$NODE_BIN_DIR" ] && [ -x "${NODE_BIN_DIR}/node" ] || { echo "node install failed" >&2; exit 1; }
# profile.d is only read by login shells; non-interactive `ssh host 'cmd'`
# (CI, rsync, scripts) runs bash -c and would miss go/node. Symlink the tools
# into /usr/local/bin, which is on the default PATH for every shell. nvm still
# wins when loaded (its bin dir is prepended), so a later `nvm install` can
# shadow these.
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
ln -sf "${NODE_BIN_DIR}/node" /usr/local/bin/node
ln -sf "${NODE_BIN_DIR}/npm" /usr/local/bin/npm
ln -sf "${NODE_BIN_DIR}/npx" /usr/local/bin/npx
# slim the published image: apt lists/archives, logs, nvm caches (.cache holds
# the downloaded node tarballs; .git is the repo cloned by nvm installer), npm
# and root caches. Without this the image balloons ~150MiB beyond the tools.
apt-get clean 2>/dev/null || true
rm -rf /var/lib/apt/lists/* /var/log/* /tmp/* /var/tmp/* 2>/dev/null || true
rm -rf /root/.nvm/.cache /root/.nvm/.git /root/.npm /root/.cache 2>/dev/null || true
# A machine-id baked into the image would be shared by every container:
# systemd-networkd derives its DHCPv6 DUID from it, so two containers look
# like the same DHCPv6 client and dnsmasq lease renewals break (the global
# IPv6 drops at the 1h lease mark). Drop it so each container generates its
# own on first boot.
rm -f /etc/machine-id /var/lib/dbus/machine-id 2>/dev/null || true'; then
    incus stop "$NAME" --timeout=30 || true
    if incus publish "$NAME" --alias "$IMAGE"; then
      incus delete --force "$NAME" || true
      # keep only the modified image — the Debian base was a build intermediate
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

echo "[80] done"