# Architecture

vpsmgr is a lightweight LXC hosting panel: one Debian 13 container per user,
managed from a web panel and a CLI. Everything ships as a single Go binary
(`vps` = CLI + embedded web panel); Incus, nftables and Traefik provide the
plumbing.

## Design goals

vpsmgr is a toy for small machines (≤ 4 GB RAM, small VPS). Storage and memory
are treated as scarce, which drives every choice in this document:

- the panel is a single static Go binary — the only new service vpsmgr adds
  besides Incus and Traefik;
- the storage pool is sparse (a loop file that only grows as it fills) and the
  published image is slimmed and the base image deleted, so disk is only
  consumed by what containers actually use, and clones share the image's
  blocks;
- container tooling stays minimal (`git` / `python3` are deliberately absent);
- live stats are batched into a handful of REST calls per refresh, and traffic
  sampling runs every 60 s, keeping idle CPU/RAM use low.

"Lightweight" refers to the panel and this storage/memory discipline, not to a
zero-overhead platform: Incus, nftables and Traefik are the minimal plumbing
that makes the panel possible.

## Components

```
install.sh / uninstall.sh / build.sh   # lifecycle + local build
scripts/  00-check 10-incus 20-network 30-traefik 40-panel 50-image
configs/  reference configs (traefik / systemd / sudoers)
src/      Go source (single binary: CLI + panel)
```

- **Go binary** — CLI commands (`vps add/show/del/...`) and the HTTPS panel
  (`vps.service`). The web templates are embedded (`//go:embed`).
- **Incus** (Zabbly LTS 7.0, Debian package) — runs the containers. Storage
  pool `vpsmgr` (ZFS), bridge `incusbr0` (10.<n>.0.0/24, octet chosen at
  install, default 115). The panel talks to the daemon over its
  **Unix-socket REST API** (`internal/lx`, one reusable HTTP connection, no
  `incus` process spawn per call). **Every** operation including `exec`
  (provisioning scripts, the readiness probe, the per-container `df` probe)
  goes over the API websocket transport — no CLI calls at all. Fractional CPU
  quotas (0.1..0.9) are enforced as `limits.cpu=1` plus
  `limits.cpu.allowance=<n>ms/100ms` — a one-core pin with a time slice.
- **nftables** — one table `inet vpsmgr`: DNAT (prerouting+output) for port
  ranges, MASQUERADE for NAT4. Reload applies the whole ruleset in a single
  `nft -f` batch (the generated file starts with a delete-table line so a load
  error leaves the previous ruleset in place). Restored on boot by
  `vps-nft.service`. The reload runs through the sudoers whitelist (the panel
  daemon is unprivileged).
- **UFW** — vpsmgr manages its firewall through its own nftables table, so the
  installer **disables UFW** when it is active: UFW's default-DROP policy runs
  before Incus's `table inet incus` rules and silently kills container IPv4
  (no DHCP, no DNS, no forwarded traffic), which makes both the image build
  and every container's network fail. If you keep UFW, you must add these
  rules yourself (see `00-check.sh`):

  ```sh
  ufw allow in on incusbr0 to any port 67 proto udp   # DHCP
  ufw allow in on incusbr0 to any port 53             # DNS
  ufw route allow in on incusbr0 from 10.<n>.0.0/24    # container forwarding
  ```
- **Traefik** — file provider, hot-reloads `/etc/traefik/dynamic`. Port 80
  proxies per domain; 443 SNI passthrough (TLS is managed inside the container).
- **SQLite** — users, domains, sessions, traffic counters. Located at
  `/etc/vpsmgr/vpsmgr.db`.

## Unprivileged panel

The panel daemon runs as the dedicated unprivileged `vps` system user
(`User=vps` in `vps.service`), never as root:

- **Incus access** comes from group membership: the Incus Unix socket is
  `group incus-admin` rw, and `vps install` adds `vps` to `incus-admin`. The
  full management API is therefore available without any privilege elevation.
- **The only root commands** the panel may run are pinned in a sudoers
  whitelist (`/etc/sudoers.d/vps`, validated with `visudo -c` before
  install): `nft` reloads, `systemctl` control of traefik / the panel itself,
  IPv6 route/neighbor/addr changes, the IPv6-forwarding sysctl and ndppd
  control. `internal/su` runs them via `sudo -n`; anything else fails instead
  of prompting.
- `vps install` (the install-time setup) still runs as root because it creates
  the user, chowns the writable dirs, writes systemd units and loads kernel
  modules. The long-running `vps serve` is fully unprivileged.

## Users and resources

- User `i` (1..200, capacity cap): container IP `10.<n>.0.(i+1)` (subnet
  `10.<n>.0.0/24`, second octet chosen at install, default 115), a random SSH
  port in `30000-31999` (DNAT to container 22), and a whole-hundred block of
  100 user ports `10000 + (i-1)*100 .. +99` (DNAT to the container, TCP+UDP).
- **IPv4 inbound policy (`v4_forward`)**: the SSH/port-block DNAT above only
  exists while `net.v4_forward` is true. When false (IPv6-only box), no DNAT
  rules are written and traefik is stopped (domains kept but not served); the
  NAT4 masquerade stays so containers still reach IPv4 outbound. Toggle:
  `vps config set net.v4_forward true|false` (applied immediately). The SSH/port
  values stay recorded in the DB.
- **Port 25 is always blocked**: the ruleset's forward chain drops port 25
  (TCP+UDP, both directions) for all forwarded traffic — permanent anti-spam,
  only a full uninstall clears it.
- Add/Del/Reinstall are serialized by a per-process mutex, and `mgr.Add` rolls
  back the container, IPv6 route, nft rules and DB record on any post-launch
  failure. The DB write is a **single transaction** (`db.CreateUserFull`):
  user row + initial traffic row + optional quota commit atomically, so a
  crash can never leave a half-created user. `mgr.Del` refuses to drop the DB
  row when the container cannot actually be removed (it would orphan the
  container and let `NextFreeIdx` reuse its IP for a new user); a fresh add
  also refuses a name/IP already claimed by a live Incus instance, so orphaned
  containers cannot cause bridge IP conflicts.
- Quotas: CPU (whole cores ≥ 1, or a fraction 0.1..0.9 of one core), memory
  (MiB), disk (GiB). Disk maps onto the ZFS quota and can only grow, never
  shrink.
- **Bandwidth quota**: each user can carry a monthly bandwidth quota in GiB
  (`users.traffic_quota_gb`, 0 = unlimited; counts upload + download of the
  current month). The 60s traffic sampler enforces it: over-quota containers
  get their eth0 rate-limited to 1Mbps each direction, back-under (e.g. monthly
  rollover) is unthrottled. The NIC limits are applied LIVE by Incus via tc
  (htb qdisc on the host veth) — no container restart — and the manager keeps
  an in-memory throttle state so Incus is only touched on state changes.
- **Domains**: the `domains` table (owner via `user_id`, PROXY flag, UTC
  created/updated timestamps) is the single source of truth; the traefik
  dynamic directory holds **one self-contained YAML per domain**
  (`/etc/traefik/dynamic/<domain>.yaml`), so toggling PROXY protocol or
  deleting a domain only rewrites/removes that one file. PROXY protocol v2 is a
  **TCP-service** feature: a flagged domain's TLS-passthrough service carries
  `proxyProtocol.version: 2` (HTTP/80 cannot — traefik injects
  `X-Forwarded-For` there). Router/service names are `sanitizeDomain(domain)`
  (dots → underscores), which is collision-free across the `[a-z0-9.-]` domain
  charset and globally unique. **DB and YAML are updated sequentially, not
  atomically**: every domain mutation writes the DB first, then the file, and
  rolls the DB back if the file write fails; a crash or full disk between the
  two can leave them out of sync. `SyncAllDomains` (run on `vps install` and
  `vps config set net.v4_forward true`) regenerates every file from the DB and
  deletes orphans, repairing any drift. All timestamps are stored as UTC; the admin
  domain page renders them in the browser's timezone.
- **Audit log**: resource-heavy user actions (power start/stop/restart,
  reinstall, reset root password, domain config changes) are recorded in the
  `audit_log` table (actor / action / UTC timestamp). The actor encodes who
  acted: a plain username for a user's own action, or `000+<username>` when an
  admin acts on that user's resources (usernames can't contain `+` or start
  with a digit, so the marker never collides). The admin audit page renders
  **client-side in 500-row chunks** fetched from `/audit/api` with infinite
  scroll, so the page never renders thousands of rows at once; the latest 5000
  rows are kept (pruned on insert, ~1 MB).
- **Init script**: each user can store a custom shell script (≤ 64 KiB) in
  their panel. On a successful `reinstall` it is written to the container over
  exec stdin (never the host command line — no injection surface) and run
  **detached** as root inside the container, logging to
  `/var/log/vpsmgr-init.log` there. Shebangs are honored; a hanging script
  cannot block the reinstall because it is backgrounded. Delivery failure only
  warns — the reinstall still succeeds.

## Storage

- The pool is ZFS. On first install, `10-incus.sh` either adopts an existing
  pool, uses a spare whole-disk block device, or (no spare disk) creates a
  **sparse loop-file pool** sized to a share of the free space on `/`:
  80% by default, 90% when ≥ 20 GiB free. The loop file only allocates blocks
  as the pool actually fills. On very small hosts, cap the ZFS ARC
  (`zfs.arc_max`) so container memory keeps priority over the pool's cache.
- Containers are ZFS clones of the image: the image's blocks are shared
  (copy-on-write), so a well-provisioned image costs one copy no matter how
  many containers. Because Incus's `refquota` counts inherited blocks, image
  bloat also eats into every container's disk quota — images must stay slim.

## Image (`vpsmgr/debian-sshd`)

Built by `50-image.sh` from `images:debian/13` (fallback `images:debian/trixie`):

- Installs `openssh-server` plus universal user tooling: `curl`, `wget`,
  `ca-certificates`, `less`, `bind9-dnsutils`, `openssh-client`, `unzip`,
  `nano`. (`ca-certificates` is essential — without it all HTTPS fails.)
- Slims the image before publishing: `apt-get clean`, removes
  `/var/lib/apt/lists/*`, logs and tmp. Without this the image balloons
  ~50 MiB+ in apt lists alone.
- Deletes the Debian base image afterwards — it is only a build intermediate;
  the runtime fallback for containers is the remote `images:debian/13`.
- `git` / `python3` are deliberately **not** baked in (heavy / opinionated).

## Optional RHEL-family image (`vpsmgr/alma-sshd`)

`60-rhel-image.sh` (NOT part of `install.sh`, run by the admin only when
wanted) builds an Alma 9 image — `rocky` builds Rocky 9 instead — with the same
hygiene: sshd + universal tooling via `dnf`, `dnf clean all` + cache/log
removal, base image deleted after publishing.

The reinstall dialog in the user panel enumerates every local `vpsmgr/*-sshd`
image (always offering Debian 13 first) and the user picks one; `mgr.Reinstall`
validates that a picked non-default image still exists. Containers run from
either base with the same light provisioning (random hostname, root password,
sshd enabled — the service is `sshd` on RHEL and `ssh` on Debian).

## Per-container provisioning

`mgr.Provision` runs inside every new or reinstalled container and:

1. Sets a **random hostname** (`vps-<8 hex>`, crypto/rand) — never the
   username, so users cannot identify each other from prompts/logs/banners on
   the internal network. Re-rolled on every reinstall.
2. Disables cloud-init hostname resets (`preserve_hostname: true`).
3. Sets the root password and ensures sshd is enabled/running.

## Container isolation on the bridge

All containers share the single `incusbr0` L2 segment `10.<n>.0.0/24` (the second
octet is chosen at install; default `10.115.0.0/24`). To make
sure a scan does **not** reveal usernames:

- Incus's dnsmasq must NOT serve instance-name DNS: by default it publishes
  `<instance>.lxd` records (instance name = username) that turn into
  `username.lxd` PTR answers — this is independent of the randomized in-guest
  hostname, so it would leak usernames anyway. `10-incus.sh` therefore sets
  `dns.mode=none` on `incusbr0` (DHCP and upstream forwarding still work; the
  `search lxd` suffix is dropped and reverse lookups fall back to the random
  guest hostname or the upstream resolver).
- The in-guest hostname is already randomized (see above), so nothing
  username-derived is ever advertised on the wire.

### L2 isolation per container

Every container's `eth0` is created (and migrated) with three Incus NIC
security options (`lx.nicIsolation`):

- `security.port_isolation=true` — the veth is an isolated bridge port
  (`isolated on`), so **no frames** (unicast, multicast, broadcast) flow
  between containers at L2. This blocks ARP/NDP spoofing, L2 sniffing, and
  rogue DHCP/DHCPv6/DNS servers.
- `security.ipv4_filtering=true` + `security.ipv6_filtering=true` — Incus
  installs bridge `input`/`forward` nftables rules per NIC that only accept
  ARP/NDP/NA claiming the container's own addresses or MAC, and drop
  router-advertisements from containers. This protects the **host's own
  ARP/NDP caches** from container-side poisoning (a container cannot rewrite
  the host's neighbor entries).

Observed side effects compared with the flat-bridge baseline:

- Containers **cannot reach each other** on the private bridge anymore (IPv4
  and IPv6 alike) — by design. Any inter-container traffic would have to go
  through a public address + DNAT; the host does not proxy the private
  subnet.
- ARP spoofing a neighbor or the gateway, NDP/NA spoofing, and RA
  router-announcement attacks are all blocked both against other containers
  and against the host.
- Outbound (IPv4/IPv6), host→container, DNAT port forwards, DHCP lease and
  IPv6 pass-through (`/112` block routed to the container + ndppd on the
  external interface) are unaffected.

### `br_netfilter` requirement

`security.ipv6_filtering` only works while the `br_netfilter` kernel module is
loaded, and Incus does **not** load it itself — a container with the option
**refuses to boot** without it, so after a host reboot every isolated container
would fail to start. `vps install` therefore writes
`/etc/modules-load.d/br_netfilter.conf` and loads the module, so it is present
before Incus starts any container. Harmless no-op where the module is built
into the kernel.

## Security model

- Panel lives behind a random secret `url_path`; everything off-path returns a
  bare, headerless 404 (no fingerprint, no auth cost).
- Mutating actions are POST-only; sessions are 3-day HttpOnly+Secure+
  SameSite=Lax cookies; a per-IP login rate limiter.
- The panel daemon is **unprivileged** (see "Unprivileged panel" above): Incus
  access via group membership, only whitelisted commands via sudo.
- Containers are Incus-unprivileged with `security.nesting=true`.

## Bandwidth accounting

Per-container NIC counters come from Incus. A background goroutine in the panel
samples every 60 s and accumulates deltas into SQLite (`traffic` table). The
panel reads totals from the DB — it never blocks on an exec for bandwidth.

## Install / uninstall lifecycle

- `uninstall.sh` without `--purge` removes the software but **keeps**
  `/etc/vpsmgr` (config/db/certs) and `/etc/traefik`, so a reinstall adopts
  the previous users/domains/settings. `--purge` removes those plus
  containers, the storage pool and the Incus package.
- `install.sh` detects an existing `/etc/vpsmgr/config.yaml` and adopts it
  (users/domains survive). `00-ip-ask.sh` reuses a previously configured
  `ipv6_subnet` / `subnet` instead of re-asking.
- `install.sh --local-build` prints the current git branch and waits 10 s
  (Ctrl-C to abort) before starting, and always rebuilds rather than reusing
  an installed stable binary.

See [ipv6.md](ipv6.md) for the optional IPv6 pass-through feature.
