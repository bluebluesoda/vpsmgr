# AGENTS.md

Guide for AI coding agents working in this repo.

## Project

vpsmgr — a lightweight LXC hosting panel. Ubuntu host + LXD containers, one
Debian 13 container per user, managed via a web panel and a CLI (single Go
binary). Hosting-panel concepts, not an ordinary web app: installers,
containers, networking, storage.

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
- IPv6 bridge prefix is clamped to ≥ /64 (`bridgePrefixLen`): LXD's dnsmasq
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

## Conventions

- One commit per small bug/feature; short subject + bullet body.
- Run `git log --oneline -10` for the current commit style before writing a
  message.
- The test environment has ~2 GiB RAM; keep CI/local test workloads light.
