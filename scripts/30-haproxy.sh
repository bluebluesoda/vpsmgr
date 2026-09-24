#!/usr/bin/env bash
# 30-haproxy.sh — install the HAProxy domain proxy, migrate away from Traefik.
#
# WHAT THIS SCRIPT OWNS
#   1. The HAProxy binary, pinned to the release BRANCH below (official HAProxy
#      performance packages; the branch is the only thing a future major
#      upgrade has to change).
#   2. The static entry configuration /etc/haproxy/haproxy.cfg. The panel
#      publishes per-generation configs and repoints /etc/haproxy/current at
#      them; this file only includes the current one.
#   3. The systemd unit (overriding any unit the distribution package ships).
#   4. The one-way migration OFF Traefik (see "TRAEFIK REMOVAL" below).
#
# ---------------------------------------------------------------------------
# UPGRADING FROM A TRAEFIK-ERA INSTALL (read before touching this script)
# ---------------------------------------------------------------------------
# vpsmgr used to front user domains with Traefik. An operator may run an old
# version for a long time before upgrading, so this script must converge a
# Traefik host onto HAProxy in ONE run, and never leave both proxies on the box:
#
#   * net.traefik (the config key) was RENAMED to net.haproxy. Values are
#     unchanged. The Go side reads BOTH (see cfg.Load / cfg.FillAuto) so an old
#     config.yaml or an old installer exporting VPSMGR_TRAEFIK still works.
#   * Traefik's program, unit and service account are REMOVED here. Only its
#     config FILE /etc/traefik/traefik.yaml is kept (harmless, no longer read).
#   * The old /etc/traefik/dynamic directory is REMOVED: it holds per-domain
#     YAML that nothing reads any more, and keeping it would invite confusion
#     about which proxy is live.
#   * The pre-rename marker /etc/vpsmgr/.install-traefik-off is still honored
#     (it records "80/443 was taken, so keep the proxy installed but down"),
#     and is deleted once the new state has been recorded.
#   * Ordering is deliberate: install + validate the new proxy FIRST, stop
#     Traefik, start HAProxy, and only then delete the Traefik files. A failure
#     before the start leaves the box on Traefik and aborts the install loudly.
#
# Do NOT add a "keep Traefik as a fallback" branch: a fallback would mean two
# proxies fighting for 80/443 after a reboot.
# ---------------------------------------------------------------------------
set -uo pipefail

log(){ echo "[30] $*"; }
die(){ echo "[30] error: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# ---------------------------------------------------------------------------
# 1. Pinned version: BRANCH is the contract, the patch level floats within it.
# ---------------------------------------------------------------------------
# Changing HAPROXY_BRANCH is the ONLY edit needed for a future major upgrade.
# It must stay in sync with cfg.HaproxyBranch in the panel (the panel refuses
# to manage a binary from a different branch) and with the two places below:
#   HAPROXY_REPO_SLUG  -> the apt source path (…/performance/<distro>/ha<XY>)
#   HAPROXY_BRANCH_RE -> the version assertion in verify_version
# A wrong slug makes `apt-get update` 404 and the install fails loudly; a wrong
# regex makes verify_version fail. Neither can silently install the wrong proxy.
HAPROXY_BRANCH="3.4"
HAPROXY_BRANCH_RE='^3\.4\.'
HAPROXY_REPO_SLUG="ha34"
HAPROXY_KEY_URL="https://pks.haproxy.com/linux/community/RPM-GPG-KEY-HAProxy"
HAPROXY_KEY="/usr/share/keyrings/HAPROXY-key-community.asc"
HAPROXY_PKG="haproxy-awslc"

# The published layout root. Must match cfg.DefaultProxyDir.
HAPROXY_DIR="/etc/haproxy"
ENTRY_CFG="$HAPROXY_DIR/haproxy.cfg"
CURRENT_LINK="$HAPROXY_DIR/current"

# ---------------------------------------------------------------------------
# 2. Distribution keys for the apt source. The vendor repo is per-distribution
#    AND per-branch, so both have to be resolved before writing the source.
# ---------------------------------------------------------------------------
OS_ID=""
OS_CODENAME=""
if [[ -r /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  OS_ID="${ID:-}"
  OS_CODENAME="${VERSION_CODENAME:-}"
fi
case "$OS_ID" in
  debian|ubuntu) ;;
  *) die "unsupported distribution '${OS_ID:-unknown}' for the HAProxy package repository (need debian or ubuntu)" ;;
esac
[[ -n "$OS_CODENAME" ]] || die "could not determine VERSION_CODENAME from /etc/os-release"

# Surface any previous HAProxy install early. An OLDER branch (e.g. 3.2) is
# upgraded in place by the branch switch below, which is expected; the version
# assertion at the end is what proves the switch worked.
if [[ -x /usr/sbin/haproxy ]]; then
  log "existing haproxy: $(/usr/sbin/haproxy -v 2>/dev/null | head -1)"
fi

# ---------------------------------------------------------------------------
# 3. Install the pinned branch from the official vendor repository.
# ---------------------------------------------------------------------------
install_repo(){
  # gpg + curl + ca-certificates are needed for the key and the repo; the
  # installer's 00-check step normally provides them, this covers a standalone
  # run of this script on a bare host.
  for p in curl ca-certificates gpg; do
    dpkg -s "$p" >/dev/null 2>&1 || {
      log "installing $p (needed for the HAProxy repository)"
      apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$p"
    }
  done

  if [[ ! -s "$HAPROXY_KEY" ]]; then
    # download_keyring: the vendor key endpoint is plain HTTPS (no TLS client
    # auth), so a fetch failure is fatal — never continue without the key, or
    # apt would trust an unverified repository.
    log "fetching the HAProxy repository key"
    mkdir -p /usr/share/keyrings
    curl -fsSL --max-time 60 -o "$HAPROXY_KEY.tmp" "$HAPROXY_KEY_URL" \
      || die "could not download the HAProxy repository key"
    mv "$HAPROXY_KEY.tmp" "$HAPROXY_KEY"
    chmod 0644 "$HAPROXY_KEY"
  fi

  local src="/etc/apt/sources.list.d/haproxy.list"
  local want="deb [signed-by=$HAPROXY_KEY] https://www.haproxy.com/download/haproxy/performance/$OS_ID/$HAPROXY_REPO_SLUG $OS_CODENAME main"
  if [[ ! -f "$src" ]] || [[ "$(cat "$src" 2>/dev/null)" != "$want" ]]; then
    # Any older source line (a previous branch) is replaced wholesale: leaving
    # both would let apt pick whichever branch has the higher version.
    printf '%s\n' "$want" > "$src"
    chmod 0644 "$src"
    log "configured the HAProxy $HAPROXY_BRANCH repository for $OS_ID/$OS_CODENAME"
    # A stale package list from the previous branch must not be reused.
    rm -f /var/lib/apt/lists/*haproxy* 2>/dev/null || true
  fi
  apt-get update -qq || die "apt-get update failed (check the HAProxy repository)"
}

verify_version(){
  local bin="${1:-/usr/sbin/haproxy}"
  [[ -x "$bin" ]] || die "HAProxy binary not found at $bin"
  local out
  out="$("$bin" -v 2>/dev/null | head -1)"
  local ver
  ver="$(awk '{print $3}' <<<"$out")"
  # The vendor package reports "HAProxy version 3.4.4-0+ha34+deb13u1 2026/08/27".
  if [[ ! "$ver" =~ $HAPROXY_BRANCH_RE ]]; then
    die "installed HAProxy is '$ver', which is not on the pinned branch $HAPROXY_BRANCH ($out)
       a newer/older branch source is still configured — fix /etc/apt/sources.list.d/haproxy.list"
  fi
  log "haproxy $ver (branch $HAPROXY_BRANCH)"
}

install_repo
if ! dpkg -s "$HAPROXY_PKG" >/dev/null 2>&1; then
  log "installing $HAPROXY_PKG (HAProxy $HAPROXY_BRANCH, AWS-LC build)"
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$HAPROXY_PKG" \
    || die "could not install $HAPROXY_PKG"
else
  # Already present: make sure it is the newest patch of the pinned branch.
  # `--only-upgrade` never installs anything new, so a no-op stays a no-op.
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --only-upgrade "$HAPROXY_PKG" >/dev/null 2>&1 || true
fi
verify_version /usr/sbin/haproxy

# ---------------------------------------------------------------------------
# 4. Directories. The panel (user 'vps') owns the layout root and publishes
#    generations into it; HAProxy only READS them, so the dirs stay 0755.
# ---------------------------------------------------------------------------
mkdir -p "$HAPROXY_DIR" /run/haproxy
# Keep any configuration an operator already had before we overwrite it. This
# is a one-time courtesy for the very first install of this script on a host
# that ran the distribution's haproxy package.
if [[ -f "$ENTRY_CFG" ]] && ! grep -q "Managed by vpsmgr" "$ENTRY_CFG" 2>/dev/null; then
  cp -a "$ENTRY_CFG" "$ENTRY_CFG.before-vpsmgr" 2>/dev/null || true
  log "kept the previous $ENTRY_CFG as $ENTRY_CFG.before-vpsmgr"
fi
cp "$ROOT/configs/haproxy/haproxy.cfg" "$ENTRY_CFG"
chmod 0644 "$ENTRY_CFG"

# A generation must exist before the unit is ever started, because the entry
# config resolves its map paths through "$CURRENT_LINK/maps/" and HAProxy
# refuses to start when a map cannot be opened. The panel republishes this from
# the DB at startup; this is only the bootstrap (empty maps = every request
# falls through to the reject backends).
# The unit loads BOTH the entry config and this file, so "the generation exists"
# means domains.cfg plus both maps.
if [[ ! -f "$CURRENT_LINK/domains.cfg" ]] || [[ ! -f "$CURRENT_LINK/maps/http.map" ]]; then
  log "bootstrapping an empty HAProxy generation"
  gen="$HAPROXY_DIR/releases/000001"
  mkdir -p "$gen/maps"
  cp "$ROOT/configs/haproxy/domains.cfg" "$gen/domains.cfg"
  cp "$ROOT/configs/haproxy/maps/http.map" "$gen/maps/http.map"
  cp "$ROOT/configs/haproxy/maps/sni.map" "$gen/maps/sni.map"
  printf 'generation: 1\ndomains: 0\n' > "$gen/manifest"
  printf '1\n' > "$HAPROXY_DIR/generation"
  # Atomic switch: a temp link renamed over the real one, so a reader (or a
  # concurrent panel publish) never observes a missing link.
  ln -sfn "releases/000001" "$CURRENT_LINK.tmp" && mv -Tf "$CURRENT_LINK.tmp" "$CURRENT_LINK"
fi

# ---------------------------------------------------------------------------
# 5. The unit. Debian's haproxy package ships /lib/systemd/system/haproxy.service
#    plus an /etc/default/haproxy conffile; ours lives in /etc/systemd/system,
#    which takes precedence, so exactly one process can ever own 80/443.
# ---------------------------------------------------------------------------
if [[ -f /lib/systemd/system/haproxy.service ]] || [[ -f /usr/lib/systemd/system/haproxy.service ]]; then
  log "distribution haproxy unit present — overridden by /etc/systemd/system/haproxy.service"
fi
cp "$ROOT/configs/systemd/haproxy.service" /etc/systemd/system/haproxy.service
chmod 0644 /etc/systemd/system/haproxy.service
systemctl daemon-reload

# The haproxy account is created by the package's maintainer scripts. If a
# future package drops it (or we ever switch to a hand-built binary), create it
# here; the config's "user haproxy" would otherwise fail to start.
if ! id -u haproxy >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin haproxy
  log "created the 'haproxy' service account"
fi

# ---------------------------------------------------------------------------
# 6. Effective enablement: an IPv4 domain-proxy-eligible policy AND the
#    domain-proxy switch. web-only qualifies; false does not.
# ---------------------------------------------------------------------------
V4_PROXY_ALLOWED=1
case "${VPSMGR_V4_FORWARD:-1}" in
  1|true|True|web-only) V4_PROXY_ALLOWED=1 ;;
  *)                   V4_PROXY_ALLOWED=0 ;;
esac

# Proxy switch: installer override first (VPSMGR_HAPROXY, or the legacy
# VPSMGR_TRAEFIK an old installer exported), then an adopted config file
# (net.haproxy, or the pre-rename net.traefik key), then the default (enabled).
PROXY_CFG=""
pick_bool(){
  case "${1,,}" in
    1|true)  PROXY_CFG=true ;;
    0|false) PROXY_CFG=false ;;
    *) die "$2 must be 1/0/true/false (got '$1')" ;;
  esac
}
if [[ -n "${VPSMGR_HAPROXY:-}" ]]; then
  pick_bool "$VPSMGR_HAPROXY" VPSMGR_HAPROXY
elif [[ -n "${VPSMGR_TRAEFIK:-}" ]]; then
  pick_bool "$VPSMGR_TRAEFIK" VPSMGR_TRAEFIK
elif [[ -f /etc/vpsmgr/config.yaml ]]; then
  # Read the new key when present, else the legacy one. Both are plain "true"/
  # "false" values under the net: section.
  v="$(grep -E '^\s+haproxy:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')"
  if [[ -z "$v" ]]; then
    v="$(grep -E '^\s+traefik:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')"
  fi
  [[ -n "$v" ]] && pick_bool "$v" config
fi

# ---------------------------------------------------------------------------
# 7. Port check BEFORE touching Traefik, so a conflict can never leave the box
#    with no proxy at all. A Traefik/HAProxy listener we already own is not a
#    conflict (that is the normal upgrade path).
# ---------------------------------------------------------------------------
ports_busy(){
  local port="$1" proc
  while read -r line; do
    [[ -z "$line" ]] && continue
    proc="$(sed -n 's/.*users:(("\([^"]*\)".*/\1/p' <<<"$line")"
    case "${proc##*/}" in
      haproxy|traefik|vpsmgr) continue ;; # ours
    esac
    return 0
  done < <(ss -H -tlnp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"')
  return 1
}

# ---------------------------------------------------------------------------
# 8. TRAEFIK REMOVAL — one-way, after the new proxy is proven startable.
# ---------------------------------------------------------------------------
traefik_present(){
  [[ -x /usr/local/bin/traefik ]] \
    || [[ -f /etc/systemd/system/traefik.service ]] \
    || [[ -d /etc/traefik ]] \
    || id -u traefik >/dev/null 2>&1
}

remove_traefik(){
  traefik_present || return 0
  log "removing the Traefik era install (one-way: HAProxy is the only proxy now)"

  # Stop and disable FIRST: a disabled unit must not come back on reboot and
  # race HAProxy for 80/443.
  systemctl disable --now traefik.service >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/traefik.service
  systemctl daemon-reload >/dev/null 2>&1 || true

  # Program and service account.
  rm -f /usr/local/bin/traefik
  if id -u traefik >/dev/null 2>&1; then
    userdel traefik >/dev/null 2>&1 || true
  fi
  # userdel leaves the same-named primary group behind on some systems; a
  # stale group makes a later `useradd traefik` fail with exit 9.
  if getent group traefik >/dev/null 2>&1; then
    groupdel traefik >/dev/null 2>&1 || true
  fi

  # Per-domain YAML from the old proxy. Nothing reads it now; leaving it would
  # only confuse an operator debugging "which proxy is serving this domain".
  rm -rf /etc/traefik/dynamic
  # The config FILE is kept deliberately: it documents the previous setup and
  # removing it would discard host-specific provider settings. It is inert.
  [[ -f /etc/traefik/traefik.yaml ]] && log "kept /etc/traefik/traefik.yaml (no longer read)"

  log "traefik removed: binary, unit, service account and dynamic rules"
}

# The marker records "80/443 was already taken when this host was installed, so
# keep the domain proxy installed but down". It predates the rename; honor it
# and then retire it (the state it describes now lives in net.haproxy).
if [[ -f /etc/vpsmgr/.install-traefik-off ]]; then
  log "found .install-traefik-off — keeping the domain proxy installed but disabled"
  PROXY_CFG=false
fi
[[ -n "$PROXY_CFG" ]] || PROXY_CFG=true

CONFLICT=""
if ports_busy 80; then CONFLICT="80"; fi
if ports_busy 443; then CONFLICT="${CONFLICT:+$CONFLICT and }443"; fi

want_start=1
if [[ "$V4_PROXY_ALLOWED" -eq 0 ]]; then
  want_start=0
  log "IPv4 inbound fully off — HAProxy installed but disabled (domains kept)"
elif [[ "$PROXY_CFG" != "true" ]]; then
  want_start=0
  log "net.haproxy is false — HAProxy installed but disabled (not started/autostarted; domains kept)"
elif [[ -n "$CONFLICT" ]]; then
  want_start=0
  log "port $CONFLICT is in use by another process — HAProxy installed but disabled"
  log "free the port, then run: vps config set net.haproxy true"
fi

# The candidate configuration must be valid BEFORE we touch the running proxy:
# a broken config would otherwise take the domain service down with it.
# Validate EXACTLY the command line the unit is about to run (see
# configs/systemd/haproxy.service): the entry config alone is not the
# configuration, it only declares the frontends, the reject backends and the
# timeouts. The per-domain backends and the routing maps live in the
# generation, so checking one file would happily pass a setup that answers 404
# on :80 and closes every :443 connection.
/usr/sbin/haproxy -c -q -f "$ENTRY_CFG" -f "$CURRENT_LINK/domains.cfg" 2>/dev/null \
  || { /usr/sbin/haproxy -c -f "$ENTRY_CFG" -f "$CURRENT_LINK/domains.cfg";
       die "the generated HAProxy configuration is invalid"; }
log "configuration check passed (entry config + generation)"

if [[ "$want_start" -eq 1 ]]; then
  systemctl enable --now haproxy.service >/dev/null 2>&1 || true
  sleep 1
  if ! systemctl is-active --quiet haproxy.service; then
    # Only NOW is a message about the still-running Traefik accurate.
    if systemctl is-active --quiet traefik.service 2>/dev/null; then
      systemctl status haproxy.service --no-pager | tail -5
      die "HAProxy failed to start while Traefik is still running — free 80/443 or disable it, then re-run the installer"
    fi
    systemctl status haproxy.service --no-pager | tail -5
    die "HAProxy failed to start"
  fi
  log "HAProxy running"
  # Only after HAProxy is confirmed active do we tear Traefik down, so a failed
  # start leaves the host exactly as it was.
  remove_traefik
else
  # Not starting: disable boot autostart so a reboot cannot bring up a proxy
  # the operator asked to keep off.
  systemctl disable --now haproxy.service >/dev/null 2>&1 || true
  # Traefik must still go: it must never be the process serving domains after
  # this script runs, even when HAProxy stays down.
  remove_traefik
fi

rm -f /etc/vpsmgr/.install-traefik-off 2>/dev/null || true
log "domain proxy: $( [[ "$want_start" -eq 1 ]] && echo "running (HAProxy $HAPROXY_BRANCH)" || echo "installed but disabled" )"
echo "[30] haproxy ready"
