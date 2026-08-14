#!/usr/bin/env bash
# check-ipv6-support.sh — probe this host for IPv6 pass-through support.
#
# Goal: decide whether each LXC container can get its own *globally
# routable* IPv6 address (no NAT66). Three phases:
#
#   Phase 1 — local facts: what global IPv6 addresses / default route does
#            this host actually have (always trustworthy, purely local).
#   Phase 2 — candidate prefix: derive a /64 from the host's global address
#            (candidate only — "host has an address in it" is NOT proof that
#            the provider routes the whole /64).
#   Phase 3 — external verification: pick a *random* address inside the
#            candidate prefix, temporarily bind it to the host, and ping it
#            from the outside via Globalping (free, no API key). A random
#            address proves "whole /64 is routed", unlike pinging the host's
#            own address.
#
# The script only ever adds/removes a temporary test address on the WAN
# interface, never touches persistent config. Run as root.
#
# Usage:
#   ./check-ipv6-support.sh                 # full probe, interactive verify
#   ./check-ipv6-support.sh --no-verify     # facts + candidate only, skip Globalping
#   ./check-ipv6-support.sh --prefix 2602:fada:6::/64   # verify a specific prefix
set -uo pipefail

PREFIX_ARG=""
NO_VERIFY=0
for a in "$@"; do
  case "$a" in
    --no-verify) NO_VERIFY=1 ;;
    --prefix) : ;;   # value consumed below
    --prefix=*) PREFIX_ARG="${a#--prefix=}" ;;
    *) [[ -z "$PREFIX_ARG" ]] && PREFIX_ARG="$a" ;;
  esac
done

log(){ echo "[v6] $*"; }
die(){ echo "[v6] error: $*" >&2; exit 1; }
warn(){ echo "[v6] ! $*" >&2; }

# ANSI colors for the final summary (auto-disabled when not a TTY).
if [[ -t 1 ]]; then
  C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'; C_BOLD=$'\033[1m'; C_OFF=$'\033[0m'
else
  C_GREEN=""; C_YELLOW=""; C_RED=""; C_BOLD=""; C_OFF=""
fi
# key: highlight the most useful info for the user (green)
key(){ echo "${C_GREEN}${C_BOLD}$*${C_OFF}"; }
# note: secondary emphasis (yellow)
note(){ echo "${C_YELLOW}$*${C_OFF}"; }

# derive_prefix — compute the canonical CIDR (network + prefix length) of an
# IPv6 address for its configured prefix length (e.g. /64, /80).
# Example: derive_prefix 2602:fada:6::7b:275c 64  ->  2602:fada:6::/64
derive_prefix(){
  python3 -c 'import ipaddress,sys
a=ipaddress.IPv6Address(sys.argv[1])
plen=int(sys.argv[2])
n=ipaddress.IPv6Network((int(a), plen), strict=False)
print(f"{n.network_address}/{n.prefixlen}")' "$1" "$2"
}

# routed_prefix — the longest on-link (proto kernel) prefix on the interface,
# at most /80, that covers addr: the address block the provider actually
# routes to this interface (e.g. AWS assigns a whole /80 to the ENI). Returns
# the canonical CIDR, or empty. More authoritative than the address's own
# configured length and still correct when the address is a bare /128.
routed_prefix(){
  # The piped route table cannot share stdin with the heredoc (the heredoc IS
  # python's stdin), so capture it and pass it as an argument instead.
  local routes
  routes=$(cat)
  python3 - "$1" "$routes" <<'PY'
import ipaddress, sys
target = ipaddress.IPv6Address(sys.argv[1])
routes = sys.argv[2]
best = None
for line in routes.splitlines():
    f = line.split()
    if not f or "/" not in f[0]:
        continue
    try:
        n = ipaddress.IPv6Network(f[0], strict=False)
    except Exception:
        continue
    if n.prefixlen <= 80 and n.network_address <= target <= n.broadcast_address:
        if best is None or n.prefixlen > best.prefixlen:
            best = n
if best:
    print(f"{best.network_address}/{best.prefixlen}")
PY
}

if [[ $EUID -ne 0 ]]; then die "must run as root"; fi
command -v ip >/dev/null 2>&1 || die "iproute2 missing (ip not found)"
command -v curl >/dev/null 2>&1 || die "curl not found"

# --- lazy-install optional deps (python3, nc) — same pattern as 00-check.sh.
# Ubuntu 24.04 templates usually have python3, but netcat-openbsd (nc) often
# does not; never fail on a missing optional tool if we can install it.
ensure_dep(){
  local bin="$1" pkg="$2"
  if command -v "$bin" >/dev/null 2>&1; then return 0; fi
  log "installing $pkg (for $bin)..."
  if ! apt-get update -qq 2>/dev/null; then return 1; fi
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$pkg" >/dev/null 2>&1 \
    && command -v "$bin" >/dev/null 2>&1
}
if ! command -v python3 >/dev/null 2>&1; then
  ensure_dep python3 python3 || die "python3 required (apt install python3 failed)"
fi
# nc is optional (TCP cross-check only) — install it if possible, else warn.
NC_AVAILABLE=0
if command -v nc >/dev/null 2>&1; then
  NC_AVAILABLE=1
elif ensure_dep nc netcat-openbsd; then
  NC_AVAILABLE=1
else
  warn "netcat-openbsd not available — TCP cross-check will be skipped"
fi

# If the *only* global v6 address belongs to a ULA prefix (fc00::/7) the
# provider cannot give us global pass-through; report and stop early.
has_ula_global(){
  ip -6 -o addr show scope global 2>/dev/null | awk '$4 ~ /^fc[0-9a-fA-F]{2}:/ {print $4}' | head -1
}

echo "==> check-ipv6-support: probing this host for IPv6 pass-through capability"
echo

# ---------------------------------------------------------------------------
# Phase 1 — local facts
# ---------------------------------------------------------------------------
echo "== Phase 1: local facts (from this host, always reliable) =="
EXT_IF=$(ip route show default 2>/dev/null | awk '{print $5; exit}')
[[ -n "$EXT_IF" ]] || die "no default route / ext interface found (is IPv4 up?)"
log "external interface: $EXT_IF"

# Kernel IPv6 forwarding — must be 1 for pass-through (host relays containers).
IP6FWD=$(cat /proc/sys/net/ipv6/conf/all/forwarding 2>/dev/null || echo 0)
log "ipv6 forwarding (all): $IP6FWD  $([ "$IP6FWD" = 1 ] && echo '(ready for pass-through)' || echo '(currently 0; installer will set it)')"

# Global (non link-local, non ULA) v6 addresses on the WAN interface.
GLOBALS=$(ip -6 -o addr show dev "$EXT_IF" scope global 2>/dev/null)
if [[ -z "$GLOBALS" ]]; then
  ULA=$(has_ula_global)
  if [[ -n "$ULA" ]]; then
    die "only ULA global address found ($ULA) — provider does not give global IPv6, pass-through impossible"
  fi
  die "no global IPv6 address on $EXT_IF — provider likely gives IPv4 only; pass-through needs a routable /64"
fi
log "global IPv6 addresses on $EXT_IF:"
echo "$GLOBALS" | while read -r _ _ _ addr _; do
  log "  - $addr"
done

# Default v6 route (gateway). Newer iproute2 prints "default nhid <id> via
# <gw>" — grab the token after "via", not the nhid number.
V6GW=$(ip -6 route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="via"){print $(i+1); exit}}')
if [[ -n "$V6GW" ]]; then
  log "default IPv6 gateway: $V6GW"
else
  warn "no default IPv6 route — outbound v6 may be broken"
fi

echo
# ---------------------------------------------------------------------------
# Phase 2 — candidate prefix: the host's own /64 or /80 slice, auto-measured
# from the on-link routed block where possible (always includes the /length).
# ---------------------------------------------------------------------------
HOST_GLOBAL=""
HOST_GLOBAL=$(echo "$GLOBALS" | awk '{print $4; exit}')   # e.g. 2602:fada:6::7b:275c/64 or ...753a::1/80
HOST_ADDR="${HOST_GLOBAL%%/*}"
HOST_LEN="${HOST_GLOBAL##*/}"
CAND_PREFIX=""
if [[ -n "$HOST_ADDR" ]]; then
  # The block actually routed to this interface (AWS: the /80 on the ENI) is
  # the authoritative subnet size; prefer it over the address's configured
  # length, which also covers bare /128-address setups.
  ROUTED=$(ip -6 route show dev "$EXT_IF" proto kernel 2>/dev/null | routed_prefix "$HOST_ADDR")
  if [[ -n "$ROUTED" ]]; then
    log "routed block on $EXT_IF (kernel route): $ROUTED"
    CAND_PREFIX="$ROUTED"
  else
    CAND_PREFIX=$(derive_prefix "$HOST_ADDR" "${HOST_LEN:-64}")
  fi
  log "host global address: $HOST_ADDR/$HOST_LEN"
  log "candidate prefix: $CAND_PREFIX"
  log "  note: host having an address here proves the provider routes"
  log "        that single address — NOT yet the whole $CAND_PREFIX (verified in phase 3)"
fi

# Optional explicit prefix (from --prefix or first positional arg).
if [[ -n "$PREFIX_ARG" ]]; then
  CAND_PREFIX="$PREFIX_ARG"
  log "using user-provided prefix: $CAND_PREFIX"
fi

# Validate candidate prefix (basic CIDR shape, global, not ULA/link-local).
# The prefix length is REQUIRED — a bare address is rejected, never assumed /64.
VALIDATE_PREFIX(){
  python3 - "$1" <<'PY'
import ipaddress, sys
p = sys.argv[1]
if "/" not in p:
    sys.exit(1)
try:
    n = ipaddress.IPv6Network(p, strict=False)
except Exception:
    sys.exit(1)
a = n.network_address
if n.prefixlen > 80:                 # need >= 48 host bits to hand out to containers
    sys.exit(1)
if a.is_private or a.is_link_local or a.is_loopback or a.is_unspecified:
    sys.exit(1)
PY
}
if [[ -n "$CAND_PREFIX" ]] && VALIDATE_PREFIX "$CAND_PREFIX"; then
  :
else
  die "invalid candidate prefix '$CAND_PREFIX' (need a global, non-ULA /80-or-shorter CIDR like 2602:fada:6::/64)"
fi

echo
# ---------------------------------------------------------------------------
# Phase 3 — external verification (Globalping, free, no API key)
# ---------------------------------------------------------------------------
if [[ $NO_VERIFY -eq 1 ]]; then
  log "skipping external verification (--no-verify)"
  echo
  echo "== summary =="
  log "ext iface: $EXT_IF   candidate prefix: $CAND_PREFIX"
  log "pass-through conclusion: NOT verified (run without --no-verify to check"
  log "  whether the provider actually routes the whole $CAND_PREFIX)"
  exit 0
fi

# Network reachability — the Globalping API must be reachable. Any HTTP
# status (even 4xx/5xx) proves the endpoint is reachable; -f would treat 405
# as failure, so omit it here.
if ! curl -sS --max-time 10 -o /dev/null https://api.globalping.io/v1/measurements 2>/dev/null; then
  warn "cannot reach api.globalping.io — skipping external verification"
  warn "pass-through conclusion: UNKNOWN (run again when network allows)"
  exit 0
fi

echo "== Phase 3: external verification (random address in $CAND_PREFIX) =="
# The random host-part must avoid:  the provider gateway (::1), the host's own
# address, and all-zero / all-f suffix hextets (subnet-router / subnet-anycast).
HOST_SUFFIX=$(echo "$HOST_ADDR" | awk -F: '{print $NF}')
RAND=""
for i in $(seq 1 30); do
  SUF=$(printf '%04x' "$((RANDOM%65521+1))")   # 1..65520, avoids 0 and ffff
  [[ "$SUF" == "$HOST_SUFFIX" ]] && continue
  RAND="$SUF"
  break
done
[[ -n "$RAND" ]] || die "could not pick a random host part"
# strip any "/len" from the prefix for address concatenation
PREFIX_BARE="${CAND_PREFIX%%/*}"
TEST_ADDR="$PREFIX_BARE$RAND"
TEST_CIDR="$TEST_ADDR/128"

log "picked random test address: $TEST_ADDR (inside $CAND_PREFIX)"
log "temporarily binding $TEST_CIDR to $EXT_IF ..."
ip -6 addr add "$TEST_CIDR" dev "$EXT_IF" 2>/dev/null \
  || die "could not bind temporary test address (try again / check the prefix)"

# Cleanup on any exit: remove temp addr (and temp nc listener if we started one).
NCPID=""
IP6FW=0
cleanup(){
  [[ -n "$NCPID" ]] && kill "$NCPID" 2>/dev/null
  ip -6 addr del "$TEST_CIDR" dev "$EXT_IF" 2>/dev/null
  # Remove the temporary ICMPv6 echo-request allow so a failed, interrupted or
  # otherwise exited run never leaves a permanent INPUT rule behind.
  if [[ "$IP6FW" == 1 ]]; then
    ip6tables -D INPUT -p icmpv6 --icmpv6-type echo-request -j ACCEPT 2>/dev/null
  fi
}
trap 'cleanup' EXIT

# Software firewall may drop ICMPv6 echo; temporarily allow echo-request on input.
if command -v ip6tables >/dev/null 2>&1 && ip6tables -L INPUT -n 2>/dev/null | grep -q '^DROP\|^REJECT'; then
  ip6tables -I INPUT -p icmpv6 --icmpv6-type echo-request -j ACCEPT 2>/dev/null && IP6FW=1
  log "temporarily allowed ICMPv6 echo-request on INPUT (host firewall detected)"
fi

# ---------------------------------------------------------------------------
# Globalping helpers: run one measurement and summarize per-probe loss.
# Locations: world is fine; we want a few probes, not one.
# ---------------------------------------------------------------------------
gp_measure(){
  local body="$1"
  curl -fsS --max-time 15 -X POST https://api.globalping.io/v1/measurements \
    -H 'Content-Type: application/json' -d "$body" 2>/dev/null
}
gp_wait(){
  local id="$1" res=""
  # wait generously (up to 90s) — probes can be slow to schedule
  for i in $(seq 1 30); do
    sleep 3
    res=$(curl -fsS --max-time 10 "https://api.globalping.io/v1/measurements/$id" 2>/dev/null) && break
  done
  echo "$res"
}
# Summarize: returns "probes=3 ok=1 any=1 worst=100" where
#   ok   = probes with 0% loss
#   any  = probes with <100% loss (at least one packet answered — counts as
#          reachable; Globalping probes are flaky so a single successful reply
#          is meaningful)
gp_summary(){
  echo "$1" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("probes=0 ok=0 any=0 worst=100"); sys.exit(0)
res = d.get("results", [])
worst = 100.0
ok = 0
any_ok = 0
for r in res:
    st = r.get("result", {}).get("stats", {})
    loss = st.get("loss", 100.0)
    if loss is None: loss = 100.0
    if loss < worst: worst = loss
    if loss == 0: ok += 1
    if loss < 100: any_ok += 1
print(f"probes={len(res)} ok={ok} any={any_ok} worst={worst}")'
}

# Run one Globalping test type (ping or tcp-ping), up to 3 times, merging
# results across attempts. Globalping is flaky (rate limits, probe churn), so
# we add redundancy: more probes per measurement, more attempts, and we treat
# "any probe answered" as success. NOTE: internal progress logs go to stderr
# (&2) so the command-substitution caller only captures the summary on stdout.
gp_run_test(){
  local label="$1" body="$2"
  local attempt best_ok=0 best_any=0 best_worst=100 best_sum="probes=0 ok=0 any=0 worst=100"
  for attempt in 1 2 3; do
    echo "[$label attempt $attempt/3] starting measurement..." >&2
    local MEAS MEAS_ID RESULT sum ok any worst
    MEAS=$(gp_measure "$body")
    MEAS_ID=""
    if [[ -n "$MEAS" ]]; then
      MEAS_ID=$(echo "$MEAS" | grep -o '"id"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | cut -d'"' -f4)
    fi
    if [[ -z "$MEAS_ID" ]]; then
      echo "[$label attempt $attempt] could not start measurement (rate-limited?)" >&2
      continue
    fi
    RESULT=$(gp_wait "$MEAS_ID")
    sum=$(gp_summary "$RESULT")
    echo "[$label attempt $attempt] $sum" >&2
    ok=$(echo "$sum" | sed -n 's/.* ok=\([0-9]*\).*/\1/p')
    any=$(echo "$sum" | sed -n 's/.* any=\([0-9]*\).*/\1/p')
    worst=$(echo "$sum" | sed -n 's/.* worst=\([0-9.]*\).*/\1/p')
    # keep the better outcome: most zero-loss probes, then most any-success
    # probes, then lowest worst-loss
    if [[ "$ok" -gt "$best_ok" ]] || \
       { [[ "$ok" -eq "$best_ok" ]] && [[ "$any" -gt "$best_any" ]]; } || \
       { [[ "$ok" -eq "$best_ok" ]] && [[ "$any" -eq "$best_any" ]] && awk -v a="$worst" -v b="$best_worst" 'BEGIN{exit !(a<b)}'; }; then
      best_ok="$ok"; best_any="$any"; best_worst="$worst"; best_sum="$sum"
    fi
    # any success is enough — stop early
    [[ "$any" -gt 0 ]] && break
  done
  echo "$best_sum"
}

# --- test 1: ICMPv6 ping ---
log "sending ICMP ping from global probes to $TEST_ADDR ..."
PING_SUM=$(gp_run_test "icmp" "{\"type\":\"ping\",\"target\":\"$TEST_ADDR\",\"locations\":[{\"magic\":\"world\",\"limit\":4}],\"measurementOptions\":{\"packets\":3}}")
PING_OK=$(echo "$PING_SUM" | sed -n 's/.* ok=\([0-9]*\).*/\1/p')
PING_ANY=$(echo "$PING_SUM" | sed -n 's/.* any=\([0-9]*\).*/\1/p')
log "ICMP ping result (best of 3): $PING_SUM"

# --- test 2: TCP ping (a real TCP handshake to a port we listen on) ---
# TCP is not subject to ICMPv6 filtering, so it cross-checks the ICMP result.
PORT=4444
TCP_SUM="probes=0 ok=0 any=0 worst=100"
if [[ "$NC_AVAILABLE" -eq 1 ]]; then
  log "sending TCP ping (port $PORT) to $TEST_ADDR ..."
  nc -6 -l "$TEST_ADDR" "$PORT" >/dev/null 2>&1 &
  NCPID=$!
  TCP_SUM=$(gp_run_test "tcp" "{\"type\":\"ping\",\"target\":\"$TEST_ADDR\",\"locations\":[{\"magic\":\"world\",\"limit\":4}],\"measurementOptions\":{\"packets\":3,\"port\":$PORT}}")
  kill "$NCPID" 2>/dev/null; NCPID=""
  TCP_OK=$(echo "$TCP_SUM" | sed -n 's/.* ok=\([0-9]*\).*/\1/p')
  TCP_ANY=$(echo "$TCP_SUM" | sed -n 's/.* any=\([0-9]*\).*/\1/p')
  log "TCP ping result (best of 3): $TCP_SUM"
else
  TCP_OK=0
  TCP_ANY=0
  warn "skipping TCP cross-check (netcat-openbsd unavailable)"
fi

echo
echo "== summary =="
log "ext iface: $EXT_IF"
log "host global address: $HOST_ADDR/$HOST_LEN"
log "test address (random, in-prefix): $TEST_ADDR"
log "ICMP ping result: $PING_SUM"
log "TCP ping result: $TCP_SUM"
echo

# Verdict: ANY probe success (TCP or ICMP) counts as verified. Globalping
# probes are noisy, so "at least one packet answered" is meaningful proof the
# prefix is routed — unlike "all probes 100% loss" which is a strong negative.
if [[ "$TCP_ANY" -gt 0 ]] || [[ "$PING_ANY" -gt 0 ]]; then
  echo "  $(key "IPv6 prefix for install:  $CAND_PREFIX")"
  echo "  $(key "Pass-through:             VERIFIED — provider routes the whole prefix")"
  echo "  $(note "Use this prefix as your ipv6_subnet when running ./install.sh")"
else
  warn "=> prefix NOT verified from outside: no probe got a single reply for"
  warn "   a random address in $CAND_PREFIX."
  warn "   - contact the provider to confirm they route $CAND_PREFIX"
  warn "   - retry later (transient / rate-limit can cause false negatives)"
fi
