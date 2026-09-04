# IPv6 pass-through

Two modes, chosen at install:

- **Prefix mode** (classic): no NAT, each container owns a global /112 block
  derived deterministically from its username inside a provider-routed prefix.
- **Pool mode**: no routable whole prefix, but the host has multiple global
  addresses on its NIC (the "discrete whitelist" providers). Each container is
  assigned one address from a confirmed pool; the host keeps the first address
  for itself.

## Prefix mode (classic)

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
| `/48` `/56` `/60` | **first /64 of the prefix** | Incus's dnsmasq rejects non-/64 networks, and every deterministic block falls inside the first /64 anyway (bits `[prefixlen:79]` are zero-filled). |
| `/64` | the /64 itself | |
| `/80` | the /80 itself | Common provider slice (e.g. AWS ENI /80). |

The bridge prefix length is clamped with `min(ones, 64)`-equivalent logic
(`bridgePrefixLen`); `SetupIPv6Bridge` (run by `vps install` and on boot)
applies it.

## Bridge setup (`SetupIPv6Bridge`)

The bridge (`incusbr0`) gets:

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

The conflicting prefix route Incus auto-creates on the bridge is deleted (eth0
keeps the authoritative route), and IPv6 forwarding is enabled.

## Per-container wiring

For each container:

- The `eth0` device sets `ipv6.address=<block>::1` and
  `ipv6.routes=<block>::/112`, so Incus routes the whole block to the container.
  Any address inside the /112 that the container binds is delivered to it.
- The primary `/128` is **bound statically** inside the container (a networkd
  `[Address]` section on Debian, a boot-time service on RHEL) — it does not
  depend on DHCPv6. This matters on reinstall: Incus's dnsmasq keeps the deleted
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
by `vps-ipv6.service` / `vps ipv6-reapply` and by `vps install`, so
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
- `10-incus.sh` — creates `incusbr0` without an IPv6 address (the address is
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
`security.ipv6_filtering` whitelists the whole routed /112 (observed on Incus
5.21), so a container may source packets from any address in its block, and
nothing else. Containers cannot reach each other on the private bridge —
v6 included — so inter-container traffic must go via public addresses; the
host does not proxy the private subnet.

## Uninstall cleanup

`uninstall.sh` reads the prefix from the config before removal, then: stops
and disables `ndppd` / `npd6` and removes `/etc/ndppd.conf`, removes any leftover
kernel `proxy_ndp` entries and `/128` routes matching the prefix, resets
`incusbr0` IPv6 to disabled, and restores forwarding sysctls.

## Pool mode (per-address pool)

For providers that hand out **discrete global addresses** (a whitelist in the
control panel) instead of a routable whole prefix — e.g. 15 addresses in one
/64 that is itself NOT routed as a whole (verified externally: a random
address inside the /64 gets no reply, while every whitelisted address does).

### Installer flow

`00-ip-ask.sh` runs the unchanged `check-ipv6-support.sh`:

1. Whole prefix **VERIFIED** → classic prefix mode (as before).
2. Not verified → the installer asks: **pool mode** (empty pool; addresses are
   added later in the admin panel), **manual prefix** (user types a prefix
   they know is routed — trusted as-is), or **disable** (pure IPv4).
   Whitelist providers typically only bind one address on the NIC at boot, so
   there is nothing to auto-collect at install time — the pool starts empty by
   design and the admin fills it from the provider's control-panel list.

The mode is fixed at install (`net.ipv6_mode`, immutable like `net.subnet`);
the **pool itself is editable** (`net.ipv6_pool`, via the admin panel's IPv6
Pool page or `vps config set`).

### Configuration

```yaml
net:
  ipv6_mode: pool
  ipv6_pool:
    - "2001:db8:1::9c4"
    - "2001:db8:1::9c5"
    ...
```

`ipv6_pool` entries are global addresses, bare or with an explicit `/128`
(any other prefix length is rejected). The host keeps its own address for
itself (not in the pool).

### Admin panel pool management

The admin panel's **IPv6 Pool** page (visible in pool mode) lets the operator:

- **Batch-add** addresses from a multi-line textarea (one per line, bare or
  `/128`). Invalid entries (bad prefix, ULA, private, duplicates) reject the
  whole batch.
- **List** the pool with per-address state: **free** or **used** (by which
  user).
- **Remove** a free address (an address assigned to a user is refused — the
  user keeps it for life).

Adding addresses re-applies the host plumbing (any address the provider
bound on the external interface is detached), so newly added addresses are
immediately usable.

### Assignment

- Each container is assigned **one address from the pool**, stored in the
  `users.ipv6_address` column (UNIQUE index = one address can never be given
  to two users; the reservation and the user row are written in one
  transaction).
- The address **belongs to the user for life** (reinstalls keep it); it is
  released only when the user is deleted (the row dies, the address is free
  again).
- The admin panel's create form has a dropdown: auto (first free), a specific
  free address, or **no IPv6** (a V4-only container). `vps add` without extra
  flags always auto-assigns the first free address; when the pool is
  exhausted, further creates simply have no IPv6 (never an error).

### Routing (empirically verified)

Each pool container gets **two NICs**:

- **eth0** — `nictype: routed`, `parent: <ext_if>`, `ipv6.address=<pool /128>`.
  Incus creates a veth pair, sets the host side to `fe80::1` (the container's
  default gateway), adds a `/128` route to the veth for the address, and
  installs a `proxy_ndp` entry on the external interface. This is the same
  mechanism `ipvlan`/`routed` NICs use to join an external network without a
  bridge.
- **eth1** — `nictype: bridged` on `incusbr0`, `ipv4.address=<private v4>`.
  Carries the shared IPv4 (SSH DNAT, user ports, NAT4 outbound) exactly like
  prefix mode.

The container's systemd-networkd binds the `/128` statically on eth0 with
`fe80::1` as the default route (no RA, no DHCPv6), and runs DHCPv4 on eth1.

**Critical prerequisite — the pool address must NOT be bound on the host's
external interface.** The whitelist provider assigns every address to the host
at boot; while the host holds an address, the kernel treats it as a LOCAL
address and drops the container's packets that use it as a source
(source-address validation) — outbound routing fails while inbound may work
by luck. Pool mode therefore removes each pool address from the external
interface (`vps ip6 addr-del`), on add/reinstall and on every
`ipv6-reapply`/boot (self-healing: a reboot that re-binds the addresses on
eth0 is corrected on the next reapply). The host keeps its own first address.

Verified on a whitelist provider host (address NOT bound on eth0, container
on a routed NIC): external Globalping probes reach the container's /128 with
**0% loss** (5/5 probes), and the container reaches out over IPv6 (curl -6)
and IPv4 (eth1 NAT) simultaneously. No ndppd, no manual per-address routes —
Incus programs everything.

### Interaction with IPv4

Pool mode works with either `v4_forward` setting. When the pool is exhausted
(or the admin picks "no IPv6"), containers are plain V4-only boxes — same as
the pre-IPv6 behavior.
