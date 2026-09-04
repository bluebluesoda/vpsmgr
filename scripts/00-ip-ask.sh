#!/usr/bin/env bash
# 00-ip-ask.sh — install-time network asks: whether to enable IPv6
# pass-through (with the global prefix), the container IPv4 subnet octet
# (10.<n>.0.0/24, default n=115), and the user-port ranges new containers may
# use (10000-29999 by default). Subnet and port ranges are fixed after install.
# IPv4 inbound forwarding is always ON by default and never asked — toggle it
# later with `vps config set net.v4_forward true|false`.
#
# Behavior:
#   - IPv6: interactive asks y/N then the prefix (default = the host's own
#     global address if it has one); VPSMGR_IPV6_SUBNET env var used verbatim
#     (validated); a previous ipv6_subnet in the config is kept on adoption;
#     disabled on non-interactive installs without the env var.
#   - Subnet: interactive asks the second octet (default 115);
#     VPSMGR_IPV4_SUBNET env var used verbatim (validated); a previous subnet
#     in the config is kept on adoption; default 10.115.0.0/24 otherwise.
#   - User ports: interactive asks one or more comma-separated port ranges
#     (default 10000-29999, no confirm on the default; a changed value echoes
#     the capacity and asks Y/n); VPSMGR_USER_PORTS env var used verbatim
#     (validated); a previous user_ports in the config is kept on adoption; a
#     legacy slot_range is converted once and the new key persisted.
#
# Writes nothing itself except the one-time legacy slot_range→user_ports
# migration; exports VPSMGR_IPV6_SUBNET / VPSMGR_IPV4_SUBNET / VPSMGR_USER_PORTS
# for the rest of the install, and re-exports an adopted VPSMGR_V4_FORWARD so
# the later steps keep the existing policy.
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
        # Pool mode starts EMPTY by design (addresses are added later in the
        # admin panel); an env-provided pool list is optional.
        log "IPv6 mode: pool (from env)"
        export VPSMGR_IPV6_MODE=pool
        return 0 ;;
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

  # Run the detector (unchanged script) to learn the host's capability. The
  # external reachability probe is powered by Globalping.
  echo
  echo "自动测试已开始，如果您不够了解IPv6，请在接下来的选项中一路默认"
  echo "Auto test running, recommened you KEEP ALL Default Settings later on."
  echo "  (The Test Powered by Globalping.io )"
  log "running check-ipv6-support.sh to probe this host..."
  DET=$(run_detector)
  log "detector verdict: $DET"

  if [[ "$DET" == "prefix" ]]; then
    # Whole prefix verified routed -> classic prefix mode. Default candidate
    # from the host's own global address, same as before.
    ask_prefix_mode
    return 0
  fi

  # The whole prefix was NOT verified from outside. Two ways forward:
  #   - pool mode: the provider hands out discrete /128 addresses (a
  #     whitelist); the user adds them later in the admin UI (the NIC only
  #     shows the one address the provider assigned at boot, so there is
  #     nothing to auto-collect at install).
  #   - manual prefix: the user knows their provider routes a prefix and
  #     types it in; we trust their choice (the external probe may have
  #     failed for transient reasons, or the user has a routed subnet).
  echo
  echo "The provider's whole prefix was NOT verified as routable from"
  echo "outside (this is typical for whitelist-style IPv6 providers)."
  echo "  [p] Pool mode  — each container gets one address you add later"
  echo "                  in the admin panel's IPv6 pool page"
  echo "  [m] Manual     — you type the prefix you know is routed"
  echo "  [n] none       — disable IPv6, proceed with IPv4 only"
  read -r -p "Choose IPv6 mode? 选择 IPv6 模式? [P/m/n] " ans3
  case "${ans3,,}" in
    p|pool|"")
      export VPSMGR_IPV6_MODE=pool VPSMGR_IPV6_POOL=""
      log "IPv6 mode: pool (empty — add addresses later in the admin panel)"
      return 0
      ;;
    m|manual)
      ask_prefix_mode
      return 0
      ;;
    *)
      log "IPv6 disabled / 未启用 — proceeding with pure IPv4"
      export VPSMGR_IPV6_MODE=none
      return 0
      ;;
  esac
}

# ask_prefix_mode — classic prefix mode: propose the host's own global
# address prefix (or let the user type one) and validate it. On success sets
# VPSMGR_IPV6_MODE=prefix + VPSMGR_IPV6_SUBNET.
ask_prefix_mode(){
  local EXT_IF GLOBAL CAND
  # ext iface: IPv4 default route first, then IPv6 default route (covers
  # IPv6-only hosts / policy-routed boxes), then the first non-virtual UP link.
  EXT_IF=$(ip route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')
  if [[ -z "$EXT_IF" ]]; then
    EXT_IF=$(ip -6 route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')
  fi
  if [[ -z "$EXT_IF" ]]; then
    EXT_IF=$(ip -o link show up 2>/dev/null | grep -v -E 'lo|incusbr|virbr|docker|veth|warp|wg' | awk -F': ' '{print $2; exit}')
  fi
  GLOBAL=""
  if [[ -n "$EXT_IF" ]]; then
    GLOBAL=$(ip -6 -o addr show dev "$EXT_IF" scope global 2>/dev/null | awk '{print $4; exit}')
  fi
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
  # Normalize to the canonical CIDR form (the length is mandatory — a bare
  # address is rejected, never silently assumed to be /64).
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

# normalize_user_ports: echoes "canonical" (whole-hundred aligned, merged,
# comma-separated) and the container capacity on one line ("canon|cap"). Exit 0
# only when the value yields at least one usable 100-port block. Mirrors
# cfg.ParseUserPorts: low end rounds up to a block start, high end rounds down
# then +99 (never ends in ...00); ranges outside 10000-29999 are clamped, and
# fully-outside ranges contribute nothing.
normalize_user_ports(){
  python3 - "$1" <<'PY'
import sys
def norm(s):
    out = []
    for tok in s.split(','):
        tok = tok.strip()
        if not tok:
            continue
        if '-' not in tok:
            raise ValueError(tok)
        a, b = tok.split('-', 1)
        a, b = int(a), int(b)
        if a > b:
            raise ValueError(tok)
        a = max(a, 10000)
        b = min(b, 29999)
        if a > b:
            continue
        lo = ((a + 99) // 100) * 100
        hi = (b // 100) * 100 + 99
        if lo > hi:
            continue
        out.append([lo, hi])
    if not out:
        raise ValueError("no usable range")
    out.sort()
    merged = [out[0]]
    for lo, hi in out[1:]:
        if lo <= merged[-1][1] + 1:
            merged[-1][1] = max(merged[-1][1], hi)
        else:
            merged.append([lo, hi])
    canon = ', '.join('%d-%d' % (lo, hi) for lo, hi in merged)
    cap = sum((hi - lo + 1) // 100 for lo, hi in merged)
    return canon, cap
try:
    canon, cap = norm(sys.argv[1])
except Exception:
    sys.exit(1)
print("%s|%d" % (canon, cap))
PY
}

# slot_to_ports: convert a LEGACY slot_range ("lo-hi", inclusive v4 last
# octets) into the equivalent user-port range string. Only used to migrate old
# configs at install; the old key is left in the file but ignored from then on.
slot_to_ports(){
  local s="$1" lo hi plo phi
  lo="${s%%-*}"; hi="${s##*-}"
  plo=$(( 10000 + (lo - 2) * 100 ))
  phi=$(( 10000 + (hi - 2) * 100 + 99 ))
  printf '%d-%d' "$plo" "$phi"
}

# user_ports_summary: one-line hint echoing what the chosen user-port ranges
# mean — the container capacity they allow (each 100-port block = one
# container). SSH ports (30000-31999) are separate.
user_ports_summary(){
  local canon="$1" cap="$2"
  log "换算: 容器总数量上限 ${cap} 台; 可用用户端口 ${canon} (SSH 端口 30000-31999 独立分配)"
  log "capacity: up to ${cap} containers; usable user ports ${canon} (SSH 30000-31999 separate)"
}

# write_user_ports: add/update the net.user_ports key in an existing config so
# a legacy slot_range migration is persisted to the file (the old slot_range
# key is left in place, unused). No-op when the config does not exist yet.
write_user_ports(){
  [[ -f /etc/vpsmgr/config.yaml ]] || return 0
  python3 - "$1" <<'PY'
import sys, os
try:
    import yaml
except ImportError:
    sys.exit(0)
p = "/etc/vpsmgr/config.yaml"
try:
    with open(p) as f:
        cfg = yaml.safe_load(f) or {}
    cfg.setdefault("net", {})["user_ports"] = sys.argv[1]
    with open(p, "w") as f:
        yaml.safe_dump(cfg, f, default_flow_style=False, sort_keys=False)
except Exception:
    sys.exit(0)
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

# --- ask: container user-port ranges -------------------------------------------
# The port ranges a NEW container's 100-port block may be drawn from. Directly
# configurable (no slot/octet concept): multiple comma-separated ranges are
# allowed, values are auto-aligned to whole hundreds, and only-new-users effect
# (existing containers keep their ports). Default 10000-29999 = 200 containers.
# Env override: VPSMGR_USER_PORTS. Operator-editable afterwards via
# `vps config set net.user_ports`.
ask_user_ports(){
  local RES CANON CAP
  # env override: VPSMGR_USER_PORTS
  if [[ -n "${VPSMGR_USER_PORTS:-}" ]]; then
    if RES=$(normalize_user_ports "$VPSMGR_USER_PORTS"); then
      CANON="${RES%%|*}"; CAP="${RES##*|}"
      log "user port ranges: $CANON (from env)"
      export VPSMGR_USER_PORTS="$CANON"
      user_ports_summary "$CANON" "$CAP"
      return 0
    fi
    die "VPSMGR_USER_PORTS='$VPSMGR_USER_PORTS' must be one or more 'A-B' ranges overlapping 10000-29999 (e.g. 10000-29999, 10000-20000, 25000-30000)"
    return 1
  fi

  # Adoption: keep the recorded user_ports instead of re-asking.
  if [[ -f /etc/vpsmgr/config.yaml ]]; then
    EXISTING=$(grep -E '^\s+user_ports:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
    if [[ -n "$EXISTING" ]]; then
      if RES=$(normalize_user_ports "$EXISTING"); then
        CANON="${RES%%|*}"; CAP="${RES##*|}"
        log "existing config has user_ports=$EXISTING — keeping it (normalized $CANON)"
        export VPSMGR_USER_PORTS="$CANON"
        user_ports_summary "$CANON" "$CAP"
        return 0
      fi
      die "existing config has an invalid user_ports='$EXISTING'"
      return 1
    fi
    # Legacy config with ONLY slot_range: convert it once and persist the new
    # key. The old slot_range key stays in the file (ignored from now on).
    LEGACY=$(grep -E '^\s+slot_range:' /etc/vpsmgr/config.yaml 2>/dev/null | awk -F': ' '{print $2}' | tr -d '"')
    if [[ -n "$LEGACY" ]]; then
      CONVERTED=$(slot_to_ports "$LEGACY")
      if RES=$(normalize_user_ports "$CONVERTED"); then
        CANON="${RES%%|*}"; CAP="${RES##*|}"
        log "existing config has legacy slot_range=$LEGACY — converted to user_ports=$CANON"
        write_user_ports "$CANON"
        export VPSMGR_USER_PORTS="$CANON"
        user_ports_summary "$CANON" "$CAP"
        return 0
      fi
      die "existing config has an invalid legacy slot_range='$LEGACY'"
      return 1
    fi
    # Older config WITHOUT either key (pre-slot-range version): this box is
    # being upgraded. Leave it at the default (full 10000-29999) and DO NOT
    # prompt — a one-click upgrade of an existing panel must not be interrupted
    # by a new question. Existing users keep their ports untouched.
    log "existing config has no user_ports (older version) — keeping default 10000-29999 (upgrade not interrupted)"
    export VPSMGR_USER_PORTS="10000-29999"
    user_ports_summary "10000-29999" "200"
    return 0
  fi

  # Non-interactive with no env var: default range (full capacity).
  if [[ ! -t 0 ]] && [[ -z "${FORCE_ASK:-}" ]]; then
    log "non-interactive install — using default user ports 10000-29999"
    export VPSMGR_USER_PORTS="10000-29999"
    user_ports_summary "10000-29999" "200"
    return 0
  fi

  # Interactive: defaults are used without further confirmation; a changed
  # value echoes the capacity it implies and asks for a Y/n confirm (default
  # Y). Affects new containers only.
  echo
  echo "用户可用端口范围设置 User port ranges"
  echo "每个容器占用一个整百的100端口块; 支持多个区间用逗号分隔; 自动按整百对齐。"
  echo "10000-29999 = 最多 200 台容器; 例如 10000-20000, 25000-30000 可跳过中间段"
  echo "10000-29999 = up to 200 containers; e.g. 10000-20000, 25000-30000 to leave a gap"
  read -r -p "端口范围 / Port ranges [default: 10000-29999]: " RANGE
  RANGE="${RANGE:-10000-29999}"
  if [[ "$RANGE" == "10000-29999" ]]; then
    log "using default user ports 10000-29999 (max 200 containers)"
    export VPSMGR_USER_PORTS="10000-29999"
    user_ports_summary "10000-29999" "200"
    return 0
  fi
  if ! RES=$(normalize_user_ports "$RANGE"); then
    die "'$RANGE' invalid — must be one or more 'A-B' ranges overlapping 10000-29999 with at least one whole 100-port block"
    return 1
  fi
  CANON="${RES%%|*}"; CAP="${RES##*|}"
  log "输入换算: 有效用户端口 ${CANON} — 容器总数量上限 ${CAP} 台"
  log "normalized: $CANON — up to ${CAP} containers"
  read -r -p "确认以上端口范围？Confirm these port ranges [Y/n]: " ANS
  case "${ANS,,}" in
    n|no) die "aborted — pick another port range"; return 1 ;;
  esac
  export VPSMGR_USER_PORTS="$CANON"
  log "user port ranges: $CANON"
  user_ports_summary "$CANON" "$CAP"
}

ask_ipv6 || return 1
ask_subnet || return 1
ask_user_ports || return 1
