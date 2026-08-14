# AGENTS.md

Guide for AI coding agents working in this repo.

## Project

vpsmgr — a lightweight LXC hosting panel. Debian 13/Ubuntu host + Incus
containers, one Debian 13 container per user, managed via a web panel and a CLI
(single Go binary). Hosting-panel concepts, not an ordinary web app:
installers, containers, networking, storage.

## Commands (always set CGO_ENABLED=0)

The project is pure Go — no cgo, never enable it. `net` (stdlib) pulls in
`runtime/cgo` whenever a C toolchain is present, which fails on machines with
broken gcc.

```sh
cd src
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
```

Shell scripts: check syntax with `bash -n scripts/*.sh`. Note: `install.sh`
and `00-ip-ask.sh` (the install-time network asks — IPv6 prefix + container
subnet octet) are `source`d, so they must use `return`, not `exit`.

## Incus (not LXD)

The container runtime is **Incus 7 LTS**, installed from the Zabbly package
repo (`10-incus.sh`) — never snap, never the `lxc` CLI.

- Daemon socket: `/var/lib/incus/unix.socket` (group `incus-admin`). The panel
  daemon (`vps.service`) runs as the unprivileged `vps` user, which is a member
  of `incus-admin`; all Incus access is via the REST API over that socket.
- `internal/lx` is the ONLY package that talks to Incus. Exec is done over the
  API websocket transport (`Exec`/`ExecSH`/`RunInitScript`) — there must be no
  `incus`/`lxc` CLI calls in the panel runtime path.
- Storage pool `vpsmgr` (ZFS preferred), bridge `lxdbr0`.
- A remote-qualified fallback image (`images:debian/13`) must go through
  `lx.EnsureImage` before `lx.Launch` — the API cannot auto-fetch it inside the
  create call (unlike the old `lxc launch` CLI).

## Privilege model

- `vps install` runs as root (setup: user creation, units, kernel modules).
- `vps serve` runs as the unprivileged `vps` user. Root commands are pinned in
  `/etc/sudoers.d/vps` (installed by `ensureSudoers`, validated with
  `visudo -c`); the panel escalates only those exact commands via `internal/su`
  (`sudo -n`). Never add a new `exec.Command("...")` that needs root without
  also adding it to the whitelist.

## Documentation

- `docs/README.md` — index
- `docs/architecture.md` — system design (storage/network/security/traffic)
- `docs/configuration.md` — `/etc/vpsmgr/config.yaml` reference
- `docs/ipv6.md` — IPv6 pass-through design (large dev-branch feature)
- `docs/development.md` — build/test/release/conventions

Keep the top-level READMEs concise; technical detail belongs in `docs/`
(English).

## Key invariants — do not break

- `uninstall.sh` **without** `--purge` must keep `/etc/vpsmgr` (config/db)
  and `/etc/traefik` so reinstall adopts them; only `--purge` deletes them.
- IPv6 bridge prefix is clamped to ≥ /64 (`bridgePrefixLen`): Incus's dnsmasq
  only serves /64 networks. Container addresses always live in the first /64
  of the configured prefix.
- IPv6 prefix length is required in `ipv6_subnet` — a bare address is
  rejected, never silently treated as /64.
- Container hostnames are random (`vps-<8hex>`) and never equal the username.
- `install.sh --local-build` must always rebuild (never reuse an installed
  binary) and warn about the branch.
- Image builds (`50-image.sh`, `60-rhel-image.sh`) must stay slim (apt/dnf
  clean) and delete the base image after publishing. `60-rhel-image.sh` is
  optional and must never be part of `install.sh` (small boxes stay lean).
- Never add cgo or force C compilation.
- The panel service is `vps.service` (restart via `systemctl restart vps`).
  Do not reintroduce `vpsmgr-*.service` names on the host. (The in-container
  `vpsmgr-ipv6.service` helper is a different thing — it lives inside
  container images.)

## Conventions

- One commit per small bug/feature; short subject + bullet body.
- Run `git log --oneline -10` for the current commit style before writing a
  message.
- The test environment has ~2 GiB RAM; keep CI/local test workloads light.
