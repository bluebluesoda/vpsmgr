# IPv6 pass-through

Two modes, chosen at install:

- **Prefix mode** (classic): no NAT, each container owns a global /112 block
  cut from the provider-routed prefix for its account (see below).
- **Pool mode**: no routable whole prefix, but the host has multiple global
  addresses on its NIC (the "discrete whitelist" providers). Each container is
  assigned one address from a confirmed pool; the host keeps the first address
  for itself.

## Prefix mode (classic)

Optional, **no NAT**: each container owns a global /112 block and the outside
can reach any address it binds directly. Enabled by setting
`net.ipv6_subnet` (asked at install; see [configuration.md](configuration.md)).

## Per-container /112 block

A block is built from a **32-bit index stored with the account** (`users.
ipv6_index`), stable across reinstalls and independent of the username:

```
block = [configured prefix][32-bit index][16 host bits]
                            bits 80-111    bits 112-127
```

- The index is **random**, picked when the account is created (and retried
  internally if the value is somehow taken — the `UNIQUE` index on the column is
  the backstop). It used to be `sha256(username)[0:4]`, which made the suffix of
  every address a global constant for a given name: the same `alice` produced
  the same block on every host running the panel, so an address identified its
  owner to anyone who knew the scheme and could enumerate names. Accounts that
  predate the column were seeded with the value they had been using, so their
  addresses never moved.
- Example (`2602:fada:6::/64`, index `0x2bd806c9`): block
  `2602:fada:6::2bd8:6c9:0/112`, primary address `2602:fada:6::2bd8:6c9:1`.
- The primary address is **byte-identical to the pre-/112 scheme** for an
  account seeded with its old value, so upgrading never changes an existing
  container's address.
- The block index is per-host: it is **not** carried across machines by
  `vps transfer`, because the receiving host numbers its own containers from its
  own prefix anyway. Deleting an account and creating one with the same name
  yields a fresh random index.
- A block never contains the bridge gateway address: an index whose block does
  is skipped when picking (the gateway lives in the all-zero block, which is why
  `0` also means "no index" — every pool-mode and V4-only account).

## Supported prefixes

`/48` .. `/80` (an explicit length is required in `ipv6_subnet`).

| Prefix | Bridge uses | Notes |
|---|---|---|
| `/48` `/56` `/60` | **first /64 of the prefix** | Incus's dnsmasq rejects non-/64 networks, and every block falls inside the first /64 anyway (bits `[prefixlen:79]` are zero-filled). |
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
  container's DHCPv6 lease for its primary address for up to an hour, so
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

## Whole /64 blocks from an extra prefix (optional)

A provider that delegates a prefix **shorter than /64** (a /60, /56 or /48) makes
one thing possible that a plain /64 host cannot do: hand a container a *whole
/64*. The /112 block a prefix-mode account normally owns has 16 host bits, so
nothing inside the container can use SLAAC (which needs exactly 64 bits) or be
delegated any further.

The feature is **off by default and stays off on every upgrade**: it exists only
while `net.ipv6_extra_prefix` is set (see [configuration.md](configuration.md)).
Nothing is allocated and no panel UI appears otherwise, and the installer never
asks for it — an operator turns it on deliberately.

### What a container gets

- Its `/112` primary address and everything around it, unchanged.
- A whole `/64` out of the extra prefix (e.g. `2001:1c00:b1b:7f0::/64`), bound
  inside the container as `<block>::1/64` — with its real length, so the prefix
  is on-link there, any address in it is usable, and the customer can carve
  sub-prefixes out of it for internal networks. It is deliberately **not** a
  local route: that would claim every address in the block for the container
  itself and make exactly that delegation impossible.
- The host routes the block to the container, and one ndppd rule covers it (like
  the `/112`s) for an upstream that resolves prefixes by NDP.

### How the route is wired

The block is declared on the container's own NIC — `ipv6.routes = <account's
/112>,<block>/64` — and `ipv6.address` is **removed** from that device. Both
halves are load-bearing:

- Incus builds `security.ipv6_filtering` from the addresses and routes a NIC
  declares, and that filter drops everything else. A block that is not declared
  is a block the bridge throws away: the container binds its addresses and gets
  replies from the host, but every packet it sources from the block dies at the
  veth.
- Incus programs a declared route as `via <ipv6.address>`. The kernel refuses
  such a route when the gateway is itself covered by a *gateway* route — which
  the account's own `/112` route is (`RTNETLINK answers: No route to host`) —
  so the declared routes have to be direct (`dev <bridge>`), which is exactly
  what omitting `ipv6.address` produces. Nothing is lost: the container binds
  its primary address itself, from the provider script, so the device option was
  only ever the DHCPv6 reservation and the routes' gateway.

Because the wiring lives in the container's own network configuration, Incus
restores it on every start — a container restart or a host reboot brings the
block back with no panel-side route plumbing. What the guest needs (the address
binding) is written by the provider script, so a power start, the boot unit and
`vps ipv6-reapply` re-apply it idempotently; `EnsureExtraBlockRoutes` also
re-declares the NIC of every account that owns a block, healing a container that
was recreated or downgraded out of band.

### The host's own /64 is never handed out

The pool excludes every `/64` the host itself uses: the one holding
`net.ipv6_subnet` (bridge address, gateway, every `/112`) and any `/64` the host
holds a global address in. Routing one of those to a container would collide
with the host's own on-link route and cut its upstream connectivity — which is
why an operator may safely point the key at a prefix that also contains the
host's own /64: it is carved out, not handed out.

### Capacity and degradation

`2^(64-len)` blocks, minus the reserved ones: a `/60` yields 15 or 16, a `/56`
255 or 256, a `/48` many thousands. Blocks are handed out lowest-first, so the
assigned set reads in order. An exhausted pool is **not** an error: the
container is created without a block, the create form greys its checkbox out,
and the label shows how many are left.

An assignment belongs to the account for life. Editing a container's quotas can
never take the block back — only deleting the account releases it, after which
the block is free for the next container.

### Where it shows up

- **Admin → IPv6** (prefix mode): the prefix editor, the capacity readout
  (`total / kept for this host / assigned / free`) and the list of assigned
  blocks with their owners (only the assigned ones — a /48 has 65536).
- **Admin → create user**: a ticked-by-default checkbox `Assign a whole /64
  (N left)`, disabled once the pool is empty; the batch dialog has the same one.
  `vps add --extra64` is the CLI equivalent.
- **Admin → quota**: `Assign a whole /64` for a container that has none; a
  container that already owns one shows it read-only. `vps extra64 <name>` is
  the CLI equivalent.
- **User panel**: the block is listed next to the container's IPv6 address
  (`IPv6 prefix: <block>/64`), presented plainly — nothing advertises it as
  something to ask for.
