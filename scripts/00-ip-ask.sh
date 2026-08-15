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

# --- IPv6 pass-through -------------------------------------------------------
#
# The installer offers THREE IPv6 outcomes, decided by probing the host with
# the (unchanged) check-ipv6-support.sh script:
#
#   1. prefix  — the provider routes a whole prefix (verified from outside):
#                containers get deterministic /112 blocks (classic mode).
#   2. pool    — no routable whole prefix, but the host has multiple global
#                addresses on its NIC (the "discrete whitelist" providers):
#                containers each get one address from the pool the user
#                confirms (the host keeps the first address for itself).
#   3. none    — pure IPv4 (user cancels, or no global IPv6 at all).
#
# The chosen mode is exported as VPSMGR_IPV6_MODE (none|prefix|pool) and the
# pool (if any) as VPSMGR_IPV6_POOL; the config's FillAuto reads both.

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

# is_global_v6: exit 0 if arg is a bare GLOBAL (non-ULA, non-link-local,
# non-loopback, non-unspecified) IPv6 address without a prefix length.
is_global_v6(){
  python3 - "$1" <<'PY'
import ipaddress, sys
try:
    a = ipaddress.IPv6Address(sys.argv[1])
except Exception:
    sys.exit(1)
if a.is_private or a.is_link_local or a.is_loopback or a.is_unspecified or a.is_multicast:
    sys.exit(1)
PY
}

# run_detector — run the (unchanged) check-ipv6-support.sh and capture its
# verdict. Prints "prefix" when the whole prefix was verified reachable from
# outside, "noverify" when no external verification was possible (offline /
# rate-limited), "unverified" when the random test address got no reply (the
# prefix is NOT routed as a whole). The script's own output is streamed to the
# user so the installer keeps the same look; only the verdict line is parsed.
run_detector(){
  local det
  det="${ROOT:-$(dirname "$(readlink -f "$0")")}/check-ipv6-support.sh"
  if [[ ! -x "$det" ]]; then
    log "check-ipv6-support.sh not found at $det — skipping automatic detection"
    echo "unverified"
    return 0
  fi
  local out
  out=$("$det" 2>&1 || true)
  printf '%s\n' "$out" >&2
  if grep -q "Pass-through:[[:space:]]*VERIFIED" <<< "$out"; then
    echo "prefix"
  elif grep -q "UNKNOWN (run again" <<< "$out"; then
    echo "noverify"
  else
    echo "unverified"
  fi
}

# ask_ipv6 — the mode-selection interview. Sets VPSMGR_IPV6_MODE and (for
# pool) VPSMGR_IPV6_POOL; leaves them empty for pure IPv4.
ask_ipv6(){
  # --- env override: VPSMGR_IPV6_MODE=none|prefix|pool (with pool list) ---
  if [[ -n "${VPSMGR_IPV6_MODE:-}" ]]; then
    case "$VPSMGR_IPV6_MODE" in
      none) log "IPv6 mode: none (from env)"; export VPSMGR_IPV6_MODE=none; return 0 ;;
      prefix)
        if [[ -z "${VPSMGR_IPV6_SUBNET:-}" ]]; then
          die "VPSMGR_IPV6_MODE=prefix requires VPSMGR_IPV6_SUBNET (e.g. 2602:fada:6::/64)"
          return 1
        fi
        log "IPv6 mode: prefix $VPSMGR_IPV6_SUBNET (from env)"; return 0 ;;
      pool)
        if [[ -z "${VPSMGR_IPV6_POOL:-}" ]]; then
          die "VPSMGR_IPV6_MODE=pool requires VPSMGR_IPV6_POOL (comma-separated global addresses)"
          return 1
        fi
        log "IPv6 mode: pool (from env)"; return 0 ;;
      *) die "VPSMGR_IPV6_MODE must be none|prefix|pool (got '$VPSMGR_IPV6_MODE')"; return 1 ;;
    esac
  fi

  # --- reinstall adoption: keep the previous mode ---
  if [[ -f /etc/vpsmgr/config.yaml ]]; then
    local EXISTING
    EXISTING=$(grep -E '^\s+ipv6_mode:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
    if [[ -n "$EXISTING" ]]; then
      log "existing config has ipv6_mode=$EXISTING — keeping it"
      export VPSMGR_IPV6_MODE="$EXISTING"
      if [[ "$EXISTING" == "pool" ]]; then
        local POOL
        POOL=$(grep -A200 '^\s+ipv6_pool:' /etc/vpsmgr/config.yaml 2>/dev/null | sed -n 's/^\s*-\s*//p' | tr '\n' ',' | sed 's/,$//')
        [[ -n "$POOL" ]] && export VPSMGR_IPV6_POOL="$POOL"
      fi
      return 0
    fi
    # Older config without ipv6_mode: fall back to ipv6_subnet presence.
    if grep -Eq '^\s+ipv6_subnet:' /etc/vpsmgr/config.yaml 2>/dev/null; then
      local OLDSUB
      OLDSUB=$(grep -E '^\s+ipv6_subnet:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
      if [[ -n "$OLDSUB" ]]; then
        log "existing config has ipv6_subnet=$OLDSUB — keeping prefix mode"
        export VPSMGR_IPV6_MODE=prefix VPSMGR_IPV6_SUBNET="$OLDSUB"
        return 0
      fi
    fi
  fi

  # --- non-interactive with no env var: IPv6 stays disabled ---
  if [[ ! -t 0 ]] && [[ -z "${FORCE_ASK:-}" ]]; then
    log "non-interactive install, no VPSMGR_IPV6_MODE set — IPv6 disabled"
    export VPSMGR_IPV6_MODE=none
    return 0
  fi

  echo
  echo "============================================================"
  echo " IPv6 pass-through  —  BETA / 实验性功能"
  echo "------------------------------------------------------------"
  echo " Each container gets its own public IPv6 address (no NAT)."
  echo " Requires either a routable prefix OR multiple global"
  echo " addresses on this host's NIC."
  echo " 每台小鸡将获得独立的公网 IPv6 地址（无 NAT）。"
  echo " 需要可路由前缀，或本机网卡上的多个公网地址。"
  echo " Default: DISABLED. Only enable if you understand the risks."
  echo " 默认不启用，请确认理解后再开启。"
  echo "============================================================"
  echo
  read -r -p "Enable IPv6 pass-through? 启用 IPv6 直通? [y/N] " ans
  case "${ans,,}" in
    y|yes) ;;
    *) log "IPv6 pass-through disabled / 未启用"; export VPSMGR_IPV6_MODE=none; return 0 ;;
  esac

  # Run the detector (unchanged script) to learn the host's capability.
  log "running check-ipv6-support.sh to probe this host..."
  DET=$(run_detector)
  log "detector verdict: $DET"

  if [[ "$DET" == "prefix" ]]; then
    # Whole prefix routed -> classic mode. Default candidate from the host's
    # own global address, same as before.
    local EXT_IF GLOBAL CAND
    EXT_IF=$(ip route show default 2>/dev/null | awk '{print $5; exit}')
    GLOBAL=$(ip -6 -o addr show dev "$EXT_IF" scope global 2>/dev/null | awk '{print $4; exit}')
    CAND=""
    if [[ -n "$GLOBAL" ]]; then
      local GADDR GLEN
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
    local PREFIX
    if [[ -n "$CAND" ]]; then
      log "detected host global address: $GLOBAL"
      read -r -p "Global prefix for containers — include the length (e.g. /64, /80) [default: $CAND]: " PREFIX
      PREFIX="${PREFIX:-$CAND}"
    else
      read -r -p "Global prefix for containers — include the length (e.g. 2001:db8::/64; up to /80): " PREFIX
    fi
    PREFIX="${PREFIX%$'\r'}"
    local PREFIX_NORM
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
      export VPSMGR_IPV6_MODE=prefix VPSMGR_IPV6_SUBNET="$PREFIX_NORM"
      log "IPv6 mode: prefix $PREFIX_NORM"
    else
      die "invalid prefix '$PREFIX' — must be a global IPv6 CIDR with an explicit length (e.g. 2602:fada:6::/64, or a /80 like 2406:da14:1dd2:a807:753a::/80)"
      return 1
    fi
    return 0
  fi

  # No verified whole prefix. Count the host's GLOBAL addresses: more than one
  # means pool mode is possible (one stays with the host, the rest go in the
  # pool). Private/ULA/link-local addresses are excluded.
  local EXT_IF GLOBALS COUNT
  EXT_IF=$(ip route show default 2>/dev/null | awk '{print $5; exit}')
  GLOBALS=$(ip -6 -o addr show dev "$EXT_IF" scope global 2>/dev/null | awk '{print $4}')
  COUNT=0
  local line
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    local a
    a="${line%%/*}"
    if is_global_v6 "$a"; then COUNT=$((COUNT+1)); fi
  done <<< "$GLOBALS"

  if [[ "$COUNT" -le 1 ]]; then
    warn "no routable whole prefix AND only $COUNT global IPv6 address(es) — IPv6 pass-through not possible"
    log "proceeding with pure IPv4"
    export VPSMGR_IPV6_MODE=none
    return 0
  fi

  # Pool mode offer: show the global addresses, keep the first for the host,
  # ask the user to confirm the rest as the pool.
  log "$COUNT global IPv6 addresses found on $EXT_IF (whole-prefix routing NOT verified):"
  local first=""
  local pool=""
  local n=0
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    local a
    a="${line%%/*}"
    if ! is_global_v6 "$a"; then continue; fi
    n=$((n+1))
    if [[ $n -eq 1 ]]; then
      first="$a"
      log "  [host] $a  (kept for the host itself)"
    else
      log "  [pool] $a"
      if [[ -z "$pool" ]]; then pool="$a"; else pool="$pool,$a"; fi
    fi
  done <<< "$GLOBALS"

  echo
  read -r -p "Add the $((COUNT-1)) addresses above to the container pool? 将以上 $((COUNT-1)) 个地址加入小鸡地址池? [y/N] " ans2
  case "${ans2,,}" in
    y|yes) ;;
    *) log "address pool declined — proceeding with pure IPv4"; export VPSMGR_IPV6_MODE=none; return 0 ;;
  esac

  # Validate every pool address once more before exporting.
  local ok=1
  IFS=',' read -r -a pool_arr <<< "$pool"
  for a in "${pool_arr[@]}"; do
    if ! is_global_v6 "$a"; then ok=0; break; fi
  done
  if [[ "$ok" -ne 1 || -z "$pool" ]]; then
    die "invalid pool addresses gathered ($pool)"
    return 1
  fi
  export VPSMGR_IPV6_MODE=pool VPSMGR_IPV6_POOL="$pool"
  log "IPv6 mode: pool ($((COUNT-1)) addresses)"
  return 0
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
