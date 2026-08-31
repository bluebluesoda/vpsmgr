# Configuration

The config file is at `/etc/vpsmgr/config.yaml` (auto-generated at install,
root-only read/write). The `VPSMGR_CONFIG` env var points the `vps` binary at
another path; the shell install/uninstall scripts always use
`/etc/vpsmgr/config.yaml`.

The installer is intended for a fresh, dedicated host unless existing services,
ports, bridges, UFW, and firewall rules have been checked. For a host that must
keep other public services and only needs IPv6 inbound access, install with
`./install.sh --disable-v4forward`; this requires an explicit confirmation,
skips vpsmgr's reserved-port check, writes `net.v4_forward=false`, and leaves
Traefik installed but stopped.

**Editing config.yaml by hand is discouraged.** The sanctioned interface is
`vps config list` / `vps config set` / `vps config help`, which validate every
change and refuse immutable fields. A raw YAML edit still works but is not
checked — get the rules from `vps config help`.

## Field matrix (kind + how a change applies)

Every field is classified exactly one way — there is no ambiguity about who
may change it or when the change takes effect. `vps config help` prints the
same table; `vps config list` shows the live values with this annotation.

| Key | Kind | Change applies | Notes |
|---|---|---|---|
| `panel.listen` | operator | `systemctl restart vps` | a FRESH install picks a random free port in 2000-9999 |
| `panel.cert` | operator | restart panel | TLS certificate path |
| `panel.key` | operator | restart panel | TLS private key path |
| `panel.db` | operator | re-run `vps install` | SQLite database path |
| `panel.public_ip` | operator | re-run `vps install` | NIC IPv4 used by firewall/routing; cert is regenerated |
| `panel.display_ip` | operator | restart panel | address shown to users (panel URL / SSH hints); any string without spaces (IP or domain), or empty = fall back to `public_ip` |
| `panel.session_days` | operator | restart panel | login session lifetime (days) |
| `panel.url_path` | **fixed at install** | — | secret prefix of the user panel; settable only while empty (re-enable) |
| `panel.admin_url_path` | operator | restart panel | secret prefix of the admin panel; an **empty value disables the admin panel** (shown as `disabled`) |
| `panel.admin_pass_hash` | managed elsewhere | — | bcrypt hash of the admin password; stored in the **DB**, set via `vps admin-passwd` / web UI |
| `net.subnet` | **fixed at install** | — | container subnet `10.<n>.0.0/24`; changing breaks existing containers |
| `net.gateway` | **fixed at install** | — | bridge gateway (derived from subnet) |
| `net.slot_range` | operator | next `vps add` / reinstall | container slot range (read the v4 last octet a new container may take), e.g. `2-201` = 200 containers; shrinkable to any sub-range of `2-201`; affects **new containers only** — existing ones keep their slot/ports |
| `net.v4_forward` | runtime toggle | **applied immediately** | false = IPv6-only containers (no SSH/port DNAT, traefik disabled, NAT4 outbound kept) |
| `net.traefik` | runtime toggle | **applied immediately** | false = stop and disable Traefik; existing domains are retained, but new domains cannot be added |
| `net.ext_if` | operator | re-run `vps install` | external NIC (auto-detected from default route) |
| `net.ipv6_subnet` | operator | re-run `vps install` | global IPv6 prefix for pass-through, e.g. `2602:fada:6::/64`; empty = disabled (does not remove IPv6 state already applied, see note below) |
| `net.ipv6_mode` | **fixed at install** | — | IPv6 allocation mode: `none` / `prefix` (/112 blocks) / `pool` (per-container address) |
| `net.ipv6_pool` | operator | `vps config set` needs `--apply`; admin-panel changes apply immediately | pool-mode address list (bare global addresses or `/128`); editable via the admin panel's IPv6 Pool page |
| `incus.image` | operator | next `vps add` / reinstall | container image alias |
| `incus.image_fallback` | operator | next `vps add` / reinstall | fallback remote image |
| `incus.pool` | **fixed at install** | — | storage pool |
| `incus.bridge` | **fixed at install** | — | managed bridge |
| `incus.socket` | operator | restart panel | Incus daemon Unix socket |
| `incus.swap_ratio` | operator | **applied immediately** | swap granted to each container as a multiple of its memory limit (`limits.memory.swap = limits.memory × ratio`); `0` disables container swap. Setting it re-applies the allowance to **all existing containers** (no restart) |
| `snapshots.limit` | operator | restart panel | max checkpoints a user may keep per container (`0` = disable new snapshots). Restoring to an older checkpoint auto-deletes the ones created after it (see below) |

### How "fixed at install" is enforced

Fields marked **fixed at install** (`net.subnet`, `net.gateway`, `incus.pool`,
`incus.bridge`, `panel.url_path`, `net.ipv6_mode`) are snapshotted into the DB settings table on
the first `vps install`. Every later `vps install` and `vps serve` compares the
live config against that snapshot and **refuses to run** if any of them drifted
— `vps config set` also refuses them up front. The user panel path
(`panel.url_path`) is immutable on purpose: moving it strands every user who
bookmarked it. The admin path is NOT immutable — it can be rotated and emptied
to disable the admin panel.

## Managed files

Files generated by the panel carry a `Managed by vpsmgr — generated, do not
edit by hand` banner and are **overwritten on the next write**:

| File | Written by |
|---|---|
| `/etc/vpsmgr/nftables.conf` | `vps install` |
| `/etc/vpsmgr/nftables.d/user-<name>.nft` | `vps add` / quota / firewall updates |
| `/etc/traefik/traefik.yaml` | install (from `configs/traefik.yaml`); only written when absent — existing installs keep theirs |
| `/etc/traefik/dynamic/<domain>.yaml` | domain add/update/delete |
| `/etc/ndppd.conf` | IPv6 pass-through updates |
| `/etc/sysctl.d/99-vpsmgr.conf` | `vps install` |
| systemd units (`vps`/`vps-nft`/`vps-ipv6`, `traefik`) | `vps install` |

## Example

```yaml
panel:
  listen: ":5231"              # panel listen address — a FRESH install picks a random free port in 2000-9999
  cert: /etc/vpsmgr/panel.crt  # HTTPS certificate
  key: /etc/vpsmgr/panel.key   # private key
  db: /etc/vpsmgr/vpsmgr.db    # SQLite database
  public_ip: AUTO              # NIC IPv4 used by the firewall / routing; on NAT-ing clouds (AWS/Alibaba) this is a private address
  display_ip: AUTO             # address shown to users (panel URL, SSH hints); any string without spaces (IP or domain); auto-fetched from ipv4.ip.sb when public_ip is private; empty = fall back to public_ip
  session_days: 3              # login session lifetime (days)
  url_path: AUTO               # random secret path, the only panel entrance; do not change after first install
  admin_url_path: AUTO         # random secret path of the admin panel; do not change after first install
  admin_pass_hash: AUTO        # bcrypt hash of the admin password — stored in the DB, not in this file

net:
  subnet: "10.115.0.0/24"      # container subnet 10.<n>.0.0/24 — only the second octet is settable, at install
  gateway: "10.115.0.1"
  v4_forward: true             # IPv4 inbound policy (false = IPv6-only containers)
  traefik: true                # domain reverse proxy (false = stopped and not enabled at boot)
  ext_if: AUTO                 # external NIC, auto-detected from the default route
  ipv6_mode: ""                # IPv6 allocation mode: none / prefix / pool (fixed at install)
  ipv6_subnet: ""              # optional: global prefix for IPv6 pass-through, e.g. "2602:fada:6::/64"
                               # (/64 or shorter uses the first /64; provider /80 slices like
                               # "2406:da14:1dd2:a807:753a::/80"); empty = disabled (default),
                               # but does not remove IPv6 state already applied
  ipv6_pool: []                # optional (pool mode): the /128 addresses containers are
                               # assigned from, e.g. "2001:db8:1::9c4" (bare global
                               # addresses, no prefix length)

incus:
  image: "vpsmgr/debian-sshd"
  image_fallback: "images:debian/13"
  pool: vpsmgr
  bridge: incusbr0
  socket: "/var/lib/incus/unix.socket"   # Incus daemon Unix socket (REST API)
  swap_ratio: 0.5                        # swap per container as a multiple of its
                                         # memory limit (0 = no swap; 0.5 = a 1 GiB
                                         # container may use 512 MiB host swap);
                                         # applied to all containers immediately on set

snapshots:
  limit: 1                                # 0 disables new snapshots
```

## Port scheme
The port layout per container is fixed at install and not individually tunable,
but its **span** follows the container slot range (`net.slot_range`):

- **Panel port**: random free port in `2000-9999`, chosen on a fresh install
  and stored in `panel.listen`. Change it in the config only if you know why.
- **SSH port**: each container gets one random port in `30000-31999` (TCP, DNAT
  to container `:22`). Independent of `idx`; always reserved regardless of the
  slot range. Shown as `ssh -p <port>`.
- **User ports**: each container owns a whole-hundred block of 100 ports,
  assigned deterministically (`UserPortBase+(idx-1)*100` .. `+99`, where
  `idx = v4-last-octet - 1`), DNAT to the container (TCP and UDP). Displayed
  compactly as e.g. `107xx`.

### Slot range (`net.slot_range`)

The slot range bounds which `idx` a **new** container may take, and therefore
how much of the user-port span (`10000-29999`) a host reserves. It is an
inclusive pair of v4 last octets, e.g. `2-201` (= idx `1..200`). Set it at
install (`00-ip-ask.sh` asks right after the subnet octet) or change it later:

```sh
vps config set net.slot_range 6-201
```

Rules:

- The value must be `A-B` with each end an integer in `2..201` and `A <= B` —
  i.e. always a **sub-range of the default**, so it can only increase the lower
  edge and/or decrease the upper edge ("shrink"). Re-expanding within `2..201`
  is allowed.
- Capacity follows the range: the default `2-201` allows **200** containers;
  `6-201` allows **196**, `20-100` allows **81**, and so on.
- A shrunken install reserves far fewer host user ports, so its port-occupancy
  check scans only the range's span (`10000+(lo-2)*100` .. `10000+(hi-2)*100+99`)
  — not the whole `10000-29999`. `80/443` and SSH `30000-31999` are reserved
  **regardless** of the range.
- It affects **new containers only**: shrinking never renumbers or removes an
  existing container (one that falls outside the narrowed range keeps its
  slot/ports). The admin panel's capacity readout reflects the range.
## IPv4 inbound policy (`v4_forward`)

`net.v4_forward` controls whether containers receive **shared IPv4 inbound**.
Always enabled by default — the installer does not ask (with IPv6 off, IPv4
forwarding is mandatory, as containers would otherwise be unreachable).

- `true` (default): containers get the random SSH port + user port block (DNAT),
  and the domain proxy (traefik) is available.
- `false`: containers are **IPv6-only**. No SSH DNAT, no port-block DNAT, and
  traefik is stopped (domains are kept but not served; adding a domain is
  rejected until re-enabled). Containers still reach IPv4 outbound via the NAT4
  masquerade.

Toggle at runtime with `vps config set net.v4_forward true|false` — the rules
are refreshed and traefik started/stopped immediately (its boot autostart is
disabled along with it, so it cannot come back on reboot). The SSH/user ports
stay recorded in the DB, so turning it back on restores everything. The user
panel hides IPv4 inbound info and shows "v4 SSH unavailable" while off, and
**domain-add is blocked while off** — the add form is hidden and the handler
rejects it (the panel reads the toggle live from the DB, no restart needed).

## Traefik (`net.traefik`)

`net.traefik` independently controls the Traefik domain reverse proxy. It is
enabled by default. Set it with `vps config set net.traefik false` to stop
Traefik and disable its boot autostart. Existing domain records and files are
kept, but users cannot add new domains while it is disabled. Re-enabling the
setting starts Traefik, restores autostart, and synchronizes the domain files.

**Install-time auto-off:** if port `80` or `443` is already bound by a
non-vpsmgr process during `install.sh`, the installer does **not** fail. Instead
it proceeds with Traefik installed but `net.traefik: false` (not started, no
boot autostart), so a host that already serves 80/443 stays usable. The
`00-check.sh` port-occupancy scan reports it and installs Traefik disabled;
re-enable later with `vps config set net.traefik true`.

## Port 25 (SMTP) is always blocked

The host nftables ruleset drops **port 25 for all forwarded traffic, both
directions, TCP and UDP** (containers ⇄ internet, container ⇄ container). This
is a permanent anti-spam measure: there is no user toggle, and the rule only
goes away when the panel is uninstalled.

## Field notes

- `panel.listen`, `panel.public_ip`, `net.ext_if` only affect display/on how
  the panel listens.
- `url_path`, `net.subnet`, `incus.pool` etc. are bound to existing data — do not
  change them after first install.
- `net.ipv6_subnet` must be a **global** (non-ULA) IPv6 CIDR with an explicit
  prefix length — a bare address is rejected, never silently assumed `/64`.
  The validator accepts a global prefix of `/80` or shorter; `/48`..`/80` are
  the documented operational range (see [ipv6.md](ipv6.md)).
- `net.ipv6_mode` is fixed at install and immutable (like `net.subnet`):
  `none` (pure IPv4), `prefix` (classic /112 blocks), `pool` (per-container
  address from `net.ipv6_pool`). Switching modes would renumber containers.
- Clearing `net.ipv6_subnet` (empty) only stops vpsmgr from applying IPv6 on
  the next `vps install`; it does not remove IPv6 already in place (bridge
  address, ndppd, routes). Full IPv6 cleanup happens in `uninstall.sh`.
