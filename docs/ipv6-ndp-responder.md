# In-tree NDP responder for prefix mode

This document explains **why** vpsmgr replaced the distro `ndppd` daemon with
its own small NDP responder in **prefix mode**, and what to verify during
wide-scale testing. It is the design rationale for the change on the
`dev-ndp` branch.

## The problem

In prefix mode each container owns a global /112 block described in
[ipv6.md](ipv6.md). For the outside world to reach a container, the provider's
upstream router sends a Neighbor Solicitation (NS) for the container address
on the host's external link; the host must answer with a Neighbor
Advertisement (NA) so the router learns which MAC to forward packets to.

Two mechanisms answered those NSes before:

- The kernel's `proxy_ndp` — but it only answers **single /128 addresses** and
  ignores prefix-cover queries, so it cannot serve a whole routed /112.
- **ndppd** — proxied ND for a whole prefix. This was vpsmgr's choice.

### The silent failure

A class of upstream IPv6 providers **drops NAs whose source address is
link-local**. Both `proxy_ndp` and ndppd emit their NA with the host's
link-local source address (`fe80::…`). The router therefore discards the reply,
the container appears unreachable from outside, and — because this happens on
the data plane with no error surfaced anywhere — it fails **silently**:
containers reach *out* fine, but inbound connectivity to the container's
public address returns nothing.

This was confirmed on a real provider with a controlled experiment: binding
the same address directly on `eth0` (so the kernel answers with the *global*
address as the NA source) made the container reachable from outside
immediately.

## The chosen fix

vpsmgr now ships its own minimal NDP responder in `internal/ndp` that answers
Neighbor Solicitations on the external interface with an NA whose **source is
the advertised global address** — exactly what these providers expect. It runs
as a root systemd unit (`vps-ipv6.service`, prefix mode only).

### Wire-format correctness

The responder is small (< 300 lines) but the invariants that matter to a
router are all handled and locked down by unit tests:

- Correct ICMPv6 checksum over the IPv6 pseudo-header.
- Hop limit = 255 (a hard NDP requirement).
- NA flags `0xe0000000` (Router+Solicited+Override) for a normal reply;
  `0xc0000000` (Solicited cleared) for a DAD answer.
- Target Link-Layer Address option carrying the host `eth0` MAC.
- DAD solicitations (source `::`) answered as a multicast `ff02::1` NA.

### Why not just patch ndppd?

The ideas of "one project-owned daemon" vs "reuse a mature daemon" were weighed
(the latter is the original report's weaker point). The decision to keep an
in-tree responder was deliberate: ndppd **cannot be configured** to source its
NA from a global address, so no config-level workaround exists; patching it
would mean maintaining a private fork of a C daemon and re-integrating upstream
forever. A ~300-line, unit-tested Go responder that we own and can reason about
was judged the better long-term trade-off. The cost is maintenance ownership.

## Reading the rules without restarting

`ndppd` needed a daemon restart on every config change. The in-tree responder
re-reads `/etc/vpsmgr/ndppd.conf` while it runs:

- At most once per second while idle.
- On-time on the arrival of an NS (by checking the file's mtime), so the very
  first solicitation after an `add`/`del` is answered instead of dropped.

As a result `vps add` / `vps del` never restart `vps-ipv6.service`.

## Design hardening (beyond the original report)

Two issues found while reviewing became part of the accepted design.

### A. Confining the root responder's trust surface

`vps-ipv6.service` runs as **root** (it needs a raw `AF_PACKET` socket). Its
input, `/etc/vpsmgr/ndppd.conf`, lives in a directory the unprivileged panel
user writes (the panel updates it on every add/del). Without additional
checks, a compromised panel process could write an arbitrary IPv6 rule and
turn the root responder into an NDP spoofer for any external address.

The responder therefore takes the operator's **routed prefix** as an `allowed`
set and silently drops any rule not contained in it (checked at both reload
points). A compromised rules-file writer can no longer make the host answer for
addresses outside vpsmgr's own prefix.

### B. No hot-looping on a broken interface

`Restart=always` + a responder that exits on error would retry every second
forever. The common trigger is an `ext_if` with no 6-byte Ethernet MAC (a
tunnel, a bare tun, some cloud virtual NICs, a bond without a slave). We added
an `ethernetMAC` pre-check at **install time**: if prefix mode is selected but
the interface cannot answer NDP, the responder is not enabled and `vps install`
warns clearly, instead of letting the service spin in a log loop. The command
itself also exits cleanly (no-op) when IPv6 is disabled or the mode is not prefix.

## What changed on the branch

| File | Change |
|---|---|
| `internal/ndp/proxy.go` | the responder (raw socket, NS→NA, `allowed` confinement) |
| `internal/ndp/proxy_test.go` | wire-format + `filterRules` tests |
| `main.go` | `vps ipv6-proxy` command, prefix `vps-ipv6.service` unit, `ethernetMAC` install check; removed ndppd daemon sudoers grants |
| `internal/mgr/ipv6.go` | `writeNDPPD` rewritten **atomically** (unique temp file + `os.Rename`); all ndppd daemon management (restart/stop/link/pid/alive) removed |
| `internal/mgr/mgr.go` | `Add`/`Reinstall`: configure the guest's IPv6 **before** publishing the NDP rule, so the first SYN never races a half-configured guest |
| `internal/mgr/routed_ipv6.go` | guest script: pin the `fe80::1` gateway neighbor, RA `UseGateway=false`, a **local** /112 route, and a `vpsmgr-ipv6.service` in-container helper that re-applies the local route after a network restart |
| `install.sh` | removed the `ndppd` install/enable block (no longer used in prefix mode) |
| `50-image.sh`, `80-debian-dev-image.sh` | bake `UseGateway=false` into the image's `[IPv6AcceptRA]` |

The real **data-plane** change is confined to prefix mode and to the NDP
handling around it. Login, snapshots, quotas, pool mode, IPv4 and the panel are
untouched.

## Notes on a few deliberate decisions

- **File name kept as `ndppd.conf`.** The format (bare `rule <cidr> {}`) is
  ndppd-compatible and unchanged; the name is retained so the panel's Writable
  directory layout and the config it renders stay stable. The documentation
  makes clear the daemon itself is gone.
- **Atomic write / privilege.** The old buggy author was a *permission* one:
  the panel failed to write `ndppd.conf` when an older `vps install` (running
  as root) had left it root-owned. The atomic-rename rewrite only depends on the
  writable parent directory, never on overwriting a possibly root-owned target.
- **`UseGateway=false`.** Guards against an RA installing a competing dynamic
  default route in the guest. It is baked into the images *and* restored
  idempotently by the runtime script.
- **Gateway neighbor pinned.** `security.ipv6_filtering` drops the guest's
  link-local NDP lookup for the bridge's `fe80::1`; pinning the neighbor to the
  bridge MAC restores the guest's ability to send without weakening isolation.
- **Local /112 route.** A normal /112 route only says where packets *go*;
  a `local` route makes every address in the block bindable, so a service on
  `block::4` is reachable without adding each address separately.

## What to verify in wide-scale testing

The unit tests (build, vet, and `go test ./...`) all pass. Prefix-mode IPv6
data flow still needs real-router validation because a test box without an
upstream IPv6 prefix cannot exercise the NDP path end-to-end. On hosts with a
routed prefix:

1. **Inbound reachability is global-source.** From an external probe (e.g.
   Globalping, same as the existing `check-ipv6-support.sh`), verify the
   container's public address and at least one non-primary address in its /112
   (e.g. `block::4`) reply. This is the exact thing that broke before.
2. **Outbound still works** — `curl -6` from inside the container.
3. **First-connection race.** Immediately after `vps add`, ping the new
   container from outside; the first NS must be answered (the on-time mtime
   reload), i.e. no "first ping lost".
4. **add/del churn without a service restart** — `journalctl -u vps-ipv6.service`
   shows no restarts across several `vps add`/`del`.
5. **Self-healing on boot** — reboot the host; `vps-ipv6.service` starts,
   re-runs `ipv6-reapply`, and inbound still works.
6. **Broken `ext_if` fails loudly** — on a host whose ext_if has no MAC,
   `vps install` (prefix mode) must *warn and not enable* the responder,
   never spin in a start loop.
7. **No competing daemon** — `ndppd.service` is inactive (it is disabled at
   install); a competing link-local NA would break the whole fix.
8. **Guest gateway neighbor + local route** — inside a container,
   `ip neigh` shows `fe80::1` pinned to the bridge MAC and
   `ip -6 route show table local` contains the /112.

## References

- Base behaviour and prefix/pool modes: [ipv6.md](ipv6.md).
- This document's rationale: the original provider bug report plus the
  `dev-ndp` branch review (issues A and B).