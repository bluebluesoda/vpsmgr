# Vpsmgr Lite

A lightweight Incus container hosting panel for small VPS instances and low-memory hosts.

> **Security warning:** The “VMs” created by this project are LXC containers, not virtual machines. Container isolation is weaker than KVM/QEMU. A container escape or kernel vulnerability could affect the host and other tenants. Do not run untrusted or high-security workloads on a shared host.

[简体中文](README.md) · [Documentation](docs/README.md)

## Feature Details

- Admin panel: users, quotas, domains, IPv6 pool, SSH public keys, and audit logs; supports logging in as a user
- User panel: power control, self-service Debian 13/AlmaLinux 9/openSUSE Leap 16/Arch Linux reinstall, domain and SSH key management, encrypted sticky notes (exportable)
- The admin can assign each user a dedicated accent color (click the username in the user list): that user's "log in" button and user-panel background use it, so impersonating operators can tell users apart at a glance
- CPU, memory, and disk oversubscription; quota changes apply live without a container restart
- Strict container network isolation; prefix mode assigns each container a dedicated `/112` IPv6 block that it can subdivide
- Traffic is counted in both directions; over-quota containers are rate-limited to 1 Mbps both ways
- Compressed ZFS storage by default; the fixed instance limit is 200 containers
- Ports 80/443 forward to container ports 80/443; port 80 uses shared HTTP proxying and port 443 shared SNI proxying

## Installation

Ubuntu 24.04 is recommended; Ubuntu 22+ and Debian 12+ also work. Debian requires a kernel-module build and takes longer to install.

Unless you have checked the existing services and network configuration, treat the installer as a fresh, dedicated-host installer. Do not run it directly on a personal host that already serves other workloads. It installs and configures Incus, ZFS (by default), nftables, Traefik, systemd units, and a container bridge; with IPv4 inbound forwarding enabled it also requires ports `80/443`, `10000-29999`, and `30000-31999` to be available, and may disable conflicting UFW configuration. The installer does not uninstall your applications, but its ports, bridge, and firewall policy can conflict with existing services.

Recommended minimum: 1 CPU core, 1 GB RAM, 15 GB of free disk space, and root access. Both amd64 and arm64 are supported; testing has focused mainly on amd64.

```sh
git clone https://github.com/bluebluesoda/vpsmgr.git
cd vpsmgr
sudo ./install.sh                  # download the latest prebuilt binary
# sudo ./install.sh --local-build  # always build from this checkout
# sudo ./install.sh --update       # update an existing install from the release
```

The installer first downloads the prebuilt binary from GitHub Releases. If that fails, it falls back to a local build. `--local-build` always rebuilds from source. If `--update` cannot download the release, the existing binary is left untouched.

If the host already runs other public services and you only need IPv6 inbound access, without IPv4 SSH/port forwarding, use:

```sh
sudo ./install.sh --disable-v4forward
```

This option asks for confirmation at the beginning. Once confirmed, it writes `net.v4_forward=false`, skips vpsmgr's reserved-port check, and only needs one randomly selected panel entry port. Traefik is still installed but remains stopped. IPv4 inbound forwarding can later be restored with `vps config set net.v4_forward true`.

ZFS is the default storage backend. For disposable test machines, you can explicitly choose the `dir` backend. It does not provide ZFS quotas, snapshots, or cloning:

```sh
sudo VPSMGR_STORAGE=dir ./install.sh
```

### IPv6

IPv6 pass-through is disabled by default. Interactive installation offers three choices:

- **Prefix mode:** the provider routes an entire IPv6 prefix to the host
- **Address-pool mode:** the provider supplies separate IPv6 addresses, which an administrator adds in the admin panel
- **Disabled:** IPv4 only

To check IPv6 reachability before installation:

```sh
bash check-ipv6-support.sh
```

## Usage

```sh
vps add <name> [--cpu N] [--mem NG] [--disk NG] [--bandwidth N]
vps quota <name> [--cpu N] [--mem NG] [--disk NG] [--bandwidth N]
vps list [name]
vps power <name> start|stop|restart
vps passwd <name>
vps del <name>
vps panel-url
vps admin-passwd
vps config list|set|help
```

The default allocation is 1 CPU core, 1 GB of RAM, and 10 GB of disk. CPU values may be whole cores or a fraction from `0.1` to `0.9`; disk capacity can only be increased. Generated passwords are shown once.

Bandwidth quotas are monthly GiB totals for upload and download combined. Containers over quota are rate-limited to 1 Mbps in both directions without a restart.

Container swap is controlled by `incus.swap_ratio` (default 0.5 — a 1 GiB memory container may use up to 512 MiB of host swap). Setting the value applies it to all existing containers immediately (no restart); after upgrading to a version with swap support, run `vps install` to apply the swap allowance to containers created before it.

Users can define an init script in the panel. It runs as root inside the container after reinstall, with output written to `/var/log/vpsmgr-init.log`.

In the admin user list, **clicking a username** (a deliberately low-key entry point) opens a picker where the operator can assign that user a dedicated accent color, or clear it back to the default. The color is applied to that user's "log in" button and tints their user-panel background (adapted for both light and dark mode). Users cannot change it themselves.

## Additional Images

Debian 13 is the default image. To also offer AlmaLinux 9, openSUSE Leap 16, Arch Linux, or a Debian dev image during reinstall, build them once:

```sh
sudo bash scripts/60-rhel-image.sh          # AlmaLinux 9
sudo bash scripts/70-opensuse-image.sh      # openSUSE Leap 16
sudo bash scripts/80-debian-dev-image.sh    # Debian 13 dev image (full dev toolchain)
sudo bash scripts/90-arch-image.sh          # Arch Linux (rolling — rebuilds to the latest snapshot on every run)
```

The image builder is not run by `install.sh`, keeping the default installation small.

## Configuration and Removal

The configuration file is `/etc/vpsmgr/config.yaml`. Prefer the validated CLI over editing it directly:

```sh
vps config list
vps config help
vps config set <key> <value>
```

```sh
sudo ./uninstall.sh          # remove software, keep config, database, and containers
sudo ./uninstall.sh --purge  # also remove config, database, containers, storage, and Incus
```

See the [documentation index](docs/README.md) for technical details.

## Screenshots

![Admin panel](ScreenShot-AdminPanel.png)

![User panel](ScreenShot-UserPanel.png)
