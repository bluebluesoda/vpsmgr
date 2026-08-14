# IPv6 pass-through

Optional, **no NAT**: each container owns a global /112 block and the outside
can reach any address it binds directly. Enabled by setting
`net.ipv6_subnet` (asked at install; see [configuration.md](configuration.md)).

## Deterministic per-container /112 block

A block is **computed on the fly from the username** — never stored, never
queried, stable across reinstalls:

```
block = [configured prefix][32-bit sha256(username)][16 host bits]
                            bits 80-111            bits 112-127
```

- Example (`2602:fada:6::/64`, user `alice`): block
  `2602:fada:6::2bd8:6c9:0/112`, primary address `2602:fada:6::2bd8:6c9:1`.
- The primary address is **byte-identical to the pre-/112 scheme**, so
  upgrading never changes an existing container's address.
- Because the 32-bit hash space is small, `vps add` refuses a name whose
  block collides with an existing user (hash collision) or would contain the
  bridge gateway address.

## Supported prefixes

`/48` .. `/80` (an explicit length is required in `ipv6_subnet`).

| Prefix | Bridge uses | Notes |
|---|---|---|
| `/48` `/56` `/60` | **first /64 of the prefix** | LXD's dnsmasq rejects non-/64 networks, and every deterministic block falls inside the first /64 anyway (bits `[prefixlen:79]` are zero-filled). |
| `/64` | the /64 itself | |
| `/80` | the /80 itself | Common provider slice (e.g. AWS ENI /80). |

The bridge prefix length is clamped with `min(ones, 64)`-equivalent logic
(`bridgePrefixLen`); `SetupIPv6Bridge` (run by `vps install` and on boot)
applies it.

## Bridge setup (`SetupIPv6Bridge`)

The bridge (`lxdbr0`) gets:

```
ipv6.address = <gw>/<len>     # len = bridgePrefixLen(ones)
ipv6.nat = false
ipv6.routing = true
ipv6.dhcp.stateful = true
```

The gateway is the first free address in the prefix (`net+1`, `net+2`, ...)
that the host can see is already taken:

1. addresses assigned to the host's external interface,
2. the upstream default gateway(s),
3. any address in the NDP neighbor table on the external interface.

The conflicting prefix route LXD auto-creates on the bridge is deleted (eth0
keeps the authoritative route), and IPv6 forwarding is enabled.

## Per-container wiring

For each container:

- The `eth0` device sets `ipv6.address=<block>::1` and
  `ipv6.routes=<block>::/112`, so LXD routes the whole block to the container.
  Any address inside the /112 that the container binds is delivered to it.
- The primary `/128` is **bound statically** inside the container (a networkd
  `[Address]` section on Debian, a boot-time service on RHEL) — it does not
  depend on DHCPv6. This matters on reinstall: LXD's dnsmasq keeps the deleted
  container's DHCPv6 lease for the deterministic address for up to an hour, so
  DHCPv6 would hand the recreated container a *dynamic* address instead, which
  falls outside the routed /112 and is dropped by `ipv6_filtering`. Binding the
  /128 directly makes IPv6 survive reinstalls.
- DHCPv6 is turned off on Debian (`DHCP=ipv4` plus `[IPv6AcceptRA] DHCPv6Client=no`
  — the RA's Managed flag would otherwise start the DHCPv6 client regardless of
  `DHCP=`), and the RA is told to generate no SLAAC address, so the container
  never ends up with a stray address outside its /112. RA
  `UseOnLinkPrefix=false` / `UseRoutePrefix=false` keep the parent prefix
  off-link, so a container reaches a peer through the host (its default
  gateway) instead of direct L2 neighbour discovery.
- `ndppd` proxies Neighbor Discovery on the **external** interface for every
  /112: an upstream neighbor solicitation for an address in a block is relayed
  to the bridge, the container answers, and ndppd relays the NA back. Kernel
  `proxy_ndp` is not used for prefixes — it only answers single addresses
  (route-covered or prefix queries are ignored).

vpsmgr renders `/etc/ndppd.conf` (one `rule <block>::/112` per container) and
restarts the daemon on `add`/`del`; the config is rebuilt from the DB at boot
by `vpsmgr-ipv6.service` / `vps ipv6-reapply` and by `vps install`, so
rules survive reboots. `vps ipv6-reapply` also re-applies the per-container
routed-IPv6 config (self-healing: containers created before the host-routed
scheme, or whose networkd config was corrupted, are repaired on every boot).

## Installer flow

- `00-ip-ask.sh` — the install-time network asks: whether to enable IPv6
  (captures the prefix) and the container subnet's second octet (default 115).
  The prefix length is **required** (no silent `/64` default). On reinstall it
  reuses an existing config's `ipv6_subnet` / `subnet` instead of re-asking.
- `install.sh` — installs `ndppd` (only when IPv6 is enabled; it is not part
  of the default small install otherwise).
- `10-lxd.sh` — creates `lxdbr0` without an IPv6 address (the address is
  chosen clash-free by `SetupIPv6Bridge` at `vps install`).
- `20-network.sh` — enables IPv6 forwarding.
- `50-image.sh` — bakes the Debian networkd IPv6 config (`DHCP=ipv4`,
  `[IPv6AcceptRA]` off-link/no-SLAAC/no-DHCPv6) into the published image.
- `60-rhel-image.sh` — bakes the RHEL kernel-managed IPv6 plumbing (sysctls for
  the RA default route without the on-link prefix, plus the `vpsmgr-ipv6`
  helper and boot unit). The runtime script installs these idempotently if an
  older image lacks them, so a pre-fix image still gets working IPv6.
- `vps install` / `add` / `reinstall` — apply the per-container config
  (`ConfigureContainerIPv6`); `ipv6-reapply` covers existing containers.
- `check-ipv6-support.sh` — probe before install: reports the host's global
  addresses, derives a candidate prefix from the on-link routed block when the
  kernel route table shows one (e.g. an AWS /80), falling back to the
  address's own configured length, and verifies from the outside (Globalping,
  free) that the provider actually routes the prefix to the host.

## Isolation interplay

Container isolation (see [architecture.md](architecture.md)) is unaffected:
`security.ipv6_filtering` whitelists the whole routed /112 (observed on LXD
5.21), so a container may source packets from any address in its block, and
nothing else. Containers cannot reach each other on the private bridge —
v6 included — so inter-container traffic must go via public addresses; the
host does not proxy the private subnet.

## Uninstall cleanup

`uninstall.sh` reads the prefix from the config before removal, then: stops
and disables `ndppd` and removes `/etc/ndppd.conf`, removes any leftover
kernel `proxy_ndp` entries and `/128` routes matching the prefix, resets
`lxdbr0` IPv6 to disabled, and restores forwarding sysctls.
