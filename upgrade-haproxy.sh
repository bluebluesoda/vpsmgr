#!/usr/bin/env bash
# upgrade-haproxy.sh — follow the pinned HAProxy branch to its newest patch.
#
# Usage:
#   ./upgrade-haproxy.sh              # upgrade if a newer patch exists
#   ./upgrade-haproxy.sh --check      # report the current/available versions, change nothing
#
# WHY A SEPARATE SCRIPT
# ---------------------------------------------------------------------------
# vpsmgr pins the HAProxy MAJOR BRANCH (3.4 today) but deliberately follows the
# newest PATCH within it: patch releases only carry fixes, never behaviour
# changes, and staying current keeps security fixes flowing without touching
# the configuration semantics the panel generates.
#
# `install.sh --update` upgrades the panel binary AND calls this script, so a
# normal upgrade keeps both in step. Run it on its own to pick up an HAProxy
# patch without touching the panel.
#
# UPGRADING ACROSS BRANCHES IS A DELIBERATE, MANUAL EDIT
# ---------------------------------------------------------------------------
# The branch lives in ONE place: HAPROXY_BRANCH below (mirrored by
# 30-haproxy.sh and cfg.HaproxyBranch in the panel — change all three). This
# script never moves to another branch on its own; it only moves within the
# pinned one, and refuses to continue if the installed binary is not on it.
# ---------------------------------------------------------------------------
set -euo pipefail

log(){ echo "[upgrade] $*"; }
die(){ echo "[upgrade] error: $*" >&2; exit 1; }

CHECK_ONLY=0
[[ "${1:-}" == "--check" ]] && CHECK_ONLY=1

# --- pinned branch (see the header) ---------------------------------------
HAPROXY_BRANCH="3.4"
HAPROXY_BRANCH_RE='^3\.4\.'
HAPROXY_REPO_SLUG="ha34"
HAPROXY_PKG="haproxy-awslc"
HAPROXY_DIR="/etc/haproxy"
ENTRY_CFG="$HAPROXY_DIR/haproxy.cfg"
BACKUP_DIR="/etc/vpsmgr/haproxy-backup"

[[ $EUID -eq 0 ]] || die "must run as root"

# --- resolve the distribution keys of the vendor repository ---------------
OS_ID="${ID:-}"; OS_CODENAME="${VERSION_CODENAME:-}"
if [[ -r /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  OS_ID="${ID:-}"; OS_CODENAME="${VERSION_CODENAME:-}"
fi
[[ -n "$OS_CODENAME" ]] || die "could not determine VERSION_CODENAME"

installed_version(){
  # "HAProxy version 3.4.4-0+ha34+deb13u1 2026/08/27" -> 3.4.4-0+ha34+deb13u1
  [[ -x /usr/sbin/haproxy ]] || { echo ""; return; }
  /usr/sbin/haproxy -v 2>/dev/null | awk 'NR==1{print $3}'
}

current="$(installed_version)"
[[ -n "$current" ]] || die "HAProxy is not installed at /usr/sbin/haproxy (run ./install.sh first)"

# An install still on the pre-HAProxy branch is a BRANCH change, not a patch
# upgrade: refuse rather than silently migrating the domain proxy's version.
if [[ ! "$current" =~ $HAPROXY_BRANCH_RE ]]; then
  die "installed HAProxy '$current' is not on the pinned branch $HAPROXY_BRANCH.
       A branch change is a deliberate migration: update HAPROXY_BRANCH /
       HAPROXY_BRANCH_RE / HAPROXY_REPO_SLUG here, in scripts/30-haproxy.sh and
       in src/internal/cfg (HaproxyBranch), then re-run ./install.sh."
fi

# --- make sure the apt source points at the pinned branch ------------------
src="/etc/apt/sources.list.d/haproxy.list"
want="deb [signed-by=/usr/share/keyrings/HAPROXY-key-community.asc] https://www.haproxy.com/download/haproxy/performance/$OS_ID/$HAPROXY_REPO_SLUG $OS_CODENAME main"
if [[ ! -f "$src" ]] || [[ "$(cat "$src")" != "$want" ]]; then
  log "fixing the HAProxy apt source (it did not point at branch $HAPROXY_BRANCH)"
  # Leftover lists from another branch must not be merged into this one.
  rm -f /var/lib/apt/lists/*haproxy* 2>/dev/null || true
  printf '%s\n' "$want" > "$src"
  chmod 0644 "$src"
fi
apt-get update -qq || die "apt-get update failed (check the HAProxy repository)"

candidate="$(apt-cache policy "$HAPROXY_PKG" 2>/dev/null | awk '/Candidate:/{print $2}')"
[[ -n "$candidate" ]] || die "no $HAPROXY_PKG candidate in the configured repository"
log "installed: $current"
log "candidate: $candidate"

if [[ "$CHECK_ONLY" == "1" ]]; then
  exit 0
fi

# Nothing to do when the candidate is not newer (or is unknown to dpkg, which
# happens when the installed build came from another source).
if dpkg --compare-versions "$candidate" le "$current" 2>/dev/null; then
  log "already on the newest $HAPROXY_BRANCH patch — nothing to do"
  exit 0
fi

# --- back up before changing anything -------------------------------------
# The panel can rebuild every generation from the database, but the STATIC
# entry config and the unit are vpsmgr-owned files that a package upgrade could
# touch, so keep a copy to diff against if something goes wrong.
mkdir -p "$BACKUP_DIR"
cp -a "$ENTRY_CFG" "$BACKUP_DIR/haproxy.cfg.$current" 2>/dev/null || true
cp -a /etc/systemd/system/haproxy.service "$BACKUP_DIR/haproxy.service.$current" 2>/dev/null || true
log "backup: $BACKUP_DIR (entry config + unit as of $current)"

# --- upgrade, then prove the result --------------------------------------
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --only-upgrade "$HAPROXY_PKG" \
  || die "package upgrade failed — the previous version is still installed and untouched"

new="$(installed_version)"
if [[ ! "$new" =~ $HAPROXY_BRANCH_RE ]]; then
  die "upgraded binary reports '$new', which is not on branch $HAPROXY_BRANCH"
fi
log "upgraded: $current -> $new"

# The candidate configuration must still be valid under the new binary before
# it is allowed to serve traffic.
if ! /usr/sbin/haproxy -c -q -f "$ENTRY_CFG"; then
  log "configuration check FAILED under $new — rolling the package back to $current"
  apt-get install -y -qq --allow-downgrades "$HAPROXY_PKG=$current" || log "warn: rollback failed, fix by hand"
  die "HAProxy upgrade rolled back (the new binary rejected the configuration)"
fi
log "configuration check passed under $new"

# --- graceful reload: existing connections are NOT dropped ----------------
# SIGUSR2 tells the master to load the new binary + configuration into a fresh
# worker; the old worker drains its established connections and exits. A
# `systemctl restart` would kill them, so it is never used here.
if systemctl is-active --quiet haproxy.service; then
  systemctl reload haproxy.service || die "graceful reload failed — service left as it was"
  sleep 1
  systemctl is-active --quiet haproxy.service || die "service is not active after the reload"
  log "haproxy reloaded without dropping established connections"
else
  # Installed but intentionally disabled (net.haproxy false, or 80/443 taken):
  # do not start it behind the operator's back.
  log "haproxy is not running — binary upgraded, service left stopped"
fi

log "done: HAProxy $new on branch $HAPROXY_BRANCH"
