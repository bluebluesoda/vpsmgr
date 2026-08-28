#!/usr/bin/env bash
# vpsmgr oneclick.sh — gitless one-shot installer.
# Downloads the repo as a tarball (no git needed on the host), runs install.sh,
# then removes the extracted directory on exit so nothing is left behind.
#
# Usage:
#   bash <(curl -fsSL https://raw.githubusercontent.com/bluebluesoda/vpsmgr/refs/heads/main/oneclick.sh)
#   bash <(curl -fsSL https://raw.githubusercontent.com/bluebluesoda/vpsmgr/refs/heads/main/oneclick.sh) --update
#
# Supported args (passed straight through to install.sh):
#   --update        re-download the latest prebuilt release binary over an existing one
#   --local-build   force local Go compilation of the panel binary
#   --disable-v4forward  install with IPv4 inbound forwarding disabled
# Env:
#   VPSMGR_STORAGE=zfs|dir   storage backend (default zfs)
#   VPSMGR_BRANCH=<branch>   branch/tag to fetch (default main)
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (sudo bash ...)" >&2
  exit 1
fi

REPO="bluebluesoda/vpsmgr"
BRANCH="${VPSMGR_BRANCH:-main}"
# The installer may reclaim /tmp and /var/tmp when disk space is tight.
# Keep the active checkout outside those directories until installation ends.
WORKDIR="$(mktemp -d /var/lib/vpsmgr-oneclick.XXXXXX)"

cleanup() {
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

ARGS=()
for a in "$@"; do
  case "$a" in
    --update|--local-build|--disable-v4forward) ARGS+=("$a") ;;
    *) echo "oneclick.sh: ignoring unknown arg '$a'" >&2 ;;
  esac
done

echo "==> downloading $REPO@$BRANCH (gitless) into $WORKDIR"
curl -fsSL "https://codeload.github.com/$REPO/tar.gz/refs/heads/$BRANCH" \
  | tar -xz --strip-components=1 -C "$WORKDIR"

echo "==> starting installer ${ARGS[*]:-}"
bash "$WORKDIR/install.sh" "${ARGS[@]:-}"

echo "==> install finished; temp dir $WORKDIR will be removed on exit"
