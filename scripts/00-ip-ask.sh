#!/usr/bin/env bash
# 00-ip-ask.sh — install-time network asks: whether to enable IPv6
# pass-through (with the global prefix) and the container IPv4 subnet octet
# (10.<n>.0.0/24, default n=115). Both are fixed after install. IPv4 inbound
# forwarding is always ON by default and never asked — toggle it later with
# `vps config set net.v4_forward true|false`.
#
# Behavior:
#   - IPv6: interactive asks y/N then the prefix (default = the host's own
#     global address if it has one); VPSMGR_IPV6_SUBNET env var used verbatim
#     (validated); a previous ipv6_subnet in the config is kept on adoption;
#     disabled on non-interactive installs without the env var.
#   - Subnet: interactive asks the second octet (default 115);
#     VPSMGR_IPV4_SUBNET env var used verbatim (validated); a previous subnet
#     in the config is kept on adoption; default 10.115.0.0/24 otherwise.
#
# Writes nothing itself; exports VPSMGR_IPV6_SUBNET / VPSMGR_IPV4_SUBNET for
# the rest of the install, and re-exports an adopted VPSMGR_V4_FORWARD so the
# later steps keep the existing policy.
set -uo pipefail

log(){ echo "[net] $*"; }
# NOTE: this script is `source`d by install.sh, so we must use `return` (not
# `exit`) at top level — `exit` would terminate the parent installer.
die(){ echo "[net] error: $*" >&2; return 1; }

# --- dependency: python3 (prefix/octet/overlap validation below). This script
# runs before 00-check.sh, so the check must live here. Ubuntu 24.04/26.04
# ship python3 by default, but install it on a minimal image if missing.
if ! command -v python3 >/dev/null 2>&1; then
  log "python3 not found, installing..."
  if apt-get update -qq 2>/dev/null \
     && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq python3 >/dev/null 2>&1 \
     && command -v python3 >/dev/null 2>&1; then
    :
  else
    die "python3 required (apt install python3 failed)"
    return 1
  fi
fi

# --- IPv6 prefix -------------------------------------------------------------

# validate_prefix: exit 0 if arg is a global IPv6 CIDR (/80 or shorter) with
# an explicit prefix length.
validate_prefix(){
  python3 - "$1" <<'PY'
import ipaddress, sys
p = sys.argv[1]
if "/" not in p:
    sys.exit(1)
try:
    n = ipaddress.IPv6Network(p, strict=False)
except Exception:
    sys.exit(1)
if n.prefixlen > 80:
    sys.exit(1)
a = n.network_address
if a.is_private or a.is_link_local or a.is_loopback or a.is_unspecified:
    sys.exit(1)
PY
}

# --- container subnet --------------------------------------------------------

# validate_octet: exit 0 if arg is an integer 1..254.
validate_octet(){
  python3 - "$1" <<'PY'
import sys
o = sys.argv[1]
if not o.isdigit() or not (1 <= int(o) <= 254):
    sys.exit(1)
PY
}

# overlaps_existing: exit 0 when 10.<octet>.0.0/24 does NOT overlap any existing
# IPv4 network on the host (routes or interface addresses). On overlap it prints
# the conflicting networks and exits 1.
overlaps_existing(){
  python3 - "$1" <<'PY'
import ipaddress, subprocess, sys
net = ipaddress.ip_network("10.%s.0.0/24" % sys.argv[1], strict=False)
out = []
for cmd in (["ip", "-4", "route", "show"], ["ip", "-4", "-o", "addr", "show"]):
    try:
        r = subprocess.run(cmd, capture_output=True, text=True).stdout
    except Exception:
        continue
    for line in r.splitlines():
        for tok in line.split():
            if "/" in tok:
                try:
                    n = ipaddress.ip_network(tok, strict=False)
                except Exception:
                    continue
                if n.version == 4 and net.overlaps(n):
                    out.append(str(n))
seen = sorted(set(out))
if seen:
    print(", ".join(seen))
    sys.exit(1)
PY
}

# --- IPv4 inbound forwarding policy ------------------------------------------
# Always ON by default, never asked. A VPSMGR_V4_FORWARD env var (e.g. for a
# scripted IPv6-only box) is honored; on adoption the recorded config value is
# re-exported so 30-traefik.sh keeps the existing policy. A fresh install
# leaves it unset → the config default (enabled) applies everywhere.
if [[ -n "${VPSMGR_V4_FORWARD:-}" ]]; then
  case "$VPSMGR_V4_FORWARD" in
    1|0|true|false) ;;
    *) die "VPSMGR_V4_FORWARD must be 1/0 (got '$VPSMGR_V4_FORWARD')"; return 1 ;;
  esac
elif [[ -f /etc/vpsmgr/config.yaml ]]; then
  V4_FWD=$(grep -E '^\s+v4_forward:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
  if [[ -n "$V4_FWD" ]]; then
    export VPSMGR_V4_FORWARD="$V4_FWD"
    log "existing config has v4_forward=$V4_FWD — keeping it"
  fi
fi

# --- ask: IPv6 pass-through --------------------------------------------------

ask_ipv6(){
  # If the env var is already set, just validate and use it.
  if [[ -n "${VPSMGR_IPV6_SUBNET:-}" ]]; then
    if validate_prefix "$VPSMGR_IPV6_SUBNET"; then
      log "IPv6 pass-through enabled with prefix $VPSMGR_IPV6_SUBNET (from env)"
      return 0
    fi
    die "VPSMGR_IPV6_SUBNET='$VPSMGR_IPV6_SUBNET' is not a valid global IPv6 CIDR — prefix length REQUIRED (e.g. 2602:fada:6::/64, or a /80 like 2406:da14:1dd2:a807:753a::/80)"
    return 1
  fi

  # Reinstall after a non-purging uninstall: an existing config already holds
  # ipv6_subnet — keep it instead of re-asking (a different answer would set the
  # bridge to a prefix the config doesn't know and break pass-through).
  if [[ -f /etc/vpsmgr/config.yaml ]]; then
    EXISTING=$(grep -E '^\s+ipv6_subnet:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
    if [[ -n "$EXISTING" ]]; then
      log "existing config has ipv6_subnet=$EXISTING — keeping it"
      export VPSMGR_IPV6_SUBNET="$EXISTING"
      return 0
    fi
  fi

  # Non-interactive with no env var: IPv6 stays disabled.
  if [[ ! -t 0 ]] && [[ -z "${FORCE_ASK:-}" ]]; then
    log "non-interactive install, no VPSMGR_IPV6_SUBNET set — IPv6 pass-through disabled"
    return 0
  fi

  echo
  echo "============================================================"
  echo " IPv6 pass-through  —  BETA / 实验性功能"
  echo "------------------------------------------------------------"
  echo " Each container gets its own public IPv6 address (no NAT)."
  echo " Requires a globally routable IPv6 prefix from your provider."
  echo " 每台小鸡将获得独立的公网 IPv6 地址（无 NAT）。"
  echo " 需要服务商提供可路由的全球 IPv6 前缀。"
  echo " Default: DISABLED. Only enable if you understand the risks."
  echo " 默认不启用，请确认理解后再开启。"
  echo "============================================================"
  echo
  read -r -p "Enable IPv6 pass-through? 启用 IPv6 直通? [y/N] " ans
  case "${ans,,}" in
    y|yes)
      ;;
    *)
      log "IPv6 pass-through disabled / 未启用"
      return 0
      ;;
  esac

  # Suggest a candidate prefix from the host's own global address, if any.
  CAND=""
  EXT_IF=$(ip route show default 2>/dev/null | awk '{print $5; exit}')
  GLOBAL=$(ip -6 -o addr show dev "$EXT_IF" scope global 2>/dev/null | awk '{print $4; exit}')
  if [[ -n "$GLOBAL" ]]; then
    GADDR="${GLOBAL%%/*}"
    GLEN="${GLOBAL##*/}"
    GLEN="${GLEN:-64}"
    CAND=$(python3 -c 'import ipaddress,sys
a=ipaddress.IPv6Address(sys.argv[1])
plen=int(sys.argv[2])
n=ipaddress.IPv6Network((int(a), plen), strict=False)
print(n.network_address)' "$GADDR" "$GLEN")
    CAND="$CAND/$GLEN"
  fi

  if [[ -n "$CAND" ]]; then
    echo
    log "detected host global address: $GLOBAL"
    read -r -p "Global prefix for containers — include the length (e.g. /64, /80) [default: $CAND]: " PREFIX
    PREFIX="${PREFIX:-$CAND}"
  else
    echo
    read -r -p "Global prefix for containers — include the length (e.g. 2001:db8::/64, provided by your ISP; up to /80): " PREFIX
  fi

  PREFIX="${PREFIX%$'\r'}"
  # Normalize to the canonical CIDR form (the length is mandatory — a bare
  # address is rejected, never silently assumed to be /64).
  PREFIX_NORM=$(python3 - "$PREFIX" <<'PY'
import ipaddress, sys
p = sys.argv[1]
if "/" not in p:
    sys.exit(1)
try:
    print(ipaddress.IPv6Network(p, strict=False))
except Exception:
    sys.exit(1)
PY
)
  if validate_prefix "$PREFIX" && [[ -n "$PREFIX_NORM" ]]; then
    export VPSMGR_IPV6_SUBNET="$PREFIX_NORM"
    log "IPv6 pass-through enabled with prefix $PREFIX_NORM"
  else
    die "invalid prefix '$PREFIX' — must be a global IPv6 CIDR with an explicit length (e.g. 2602:fada:6::/64, or a /80 like 2406:da14:1dd2:a807:753a::/80)"
    return 1
  fi
}

# --- ask: container IPv4 subnet octet ----------------------------------------

ask_subnet(){
  # If the env var is already set, just validate and use it.
  if [[ -n "${VPSMGR_IPV4_SUBNET:-}" ]]; then
    SUB="$VPSMGR_IPV4_SUBNET"
    if [[ "$SUB" =~ ^10\.([0-9]+)\.0\.0/24$ ]] && validate_octet "${BASH_REMATCH[1]}"; then
      log "container subnet: $SUB (from env)"
      export VPSMGR_IPV4_SUBNET="$SUB"
      return 0
    fi
    die "VPSMGR_IPV4_SUBNET='$SUB' must be 10.<n>.0.0/24 with n in 1..254 (e.g. 10.115.0.0/24)"
    return 1
  fi

  # Reinstall after a non-purging uninstall: an existing config already holds
  # the subnet — keep it instead of re-asking (a different answer would set the
  # bridge to a prefix the config doesn't know and break container networking).
  if [[ -f /etc/vpsmgr/config.yaml ]]; then
    EXISTING=$(grep -E '^\s+subnet:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
    if [[ -n "$EXISTING" ]]; then
      log "existing config has subnet=$EXISTING — keeping it"
      export VPSMGR_IPV4_SUBNET="$EXISTING"
      return 0
    fi
  fi

  # Non-interactive with no env var: default subnet.
  if [[ ! -t 0 ]] && [[ -z "${FORCE_ASK:-}" ]]; then
    log "non-interactive install — using default subnet 10.115.0.0/24"
    export VPSMGR_IPV4_SUBNET="10.115.0.0/24"
    return 0
  fi

  # Interactive: only the second octet is tweakable, and it is fixed at install.
  echo
  echo "容器子网 10.<n>.0.0/24（网关 .1）— 仅第二段八位组可自定义，安装后不可更改。"
  echo "Container subnet 10.<n>.0.0/24 (gateway .1) — only the second octet is tweakable; fixed after install."
  read -r -p "第二段八位组 / Second octet (1-254) [default: 115]: " OCT
  OCT="${OCT:-115}"
  if ! validate_octet "$OCT"; then
    die "'$OCT' is not an integer in 1..254"
    return 1
  fi
  if CONFLICT=$(overlaps_existing "$OCT"); then
    :
  else
    echo
    log "warn: 10.$OCT.0.0/24 overlaps an existing host network ($CONFLICT)."
    read -r -p "      continue anyway? [y/N] " ANS
    case "${ANS,,}" in
      y|yes) ;;
      *) die "aborted — pick another octet"; return 1 ;;
    esac
  fi
  export VPSMGR_IPV4_SUBNET="10.$OCT.0.0/24"
  log "container subnet: 10.$OCT.0.0/24"
}

ask_ipv6 || return 1
ask_subnet || return 1
