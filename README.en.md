# Vpsmgr Lite

A lightweight Incus container hosting panel for small VPS instances and low-memory hosts.

> **Security warning:** The “VMs” created by this project are LXC containers, not virtual machines. Container isolation is weaker than KVM/QEMU. A container escape or kernel vulnerability could affect the host and other tenants. Do not run untrusted or high-security workloads on a shared host.

[简体中文](README.md) · [Documentation](docs/README.md)

## Feature Details

- Admin panel: users, quotas, domains, IPv6 pool, and audit logs
- User panel: power control, self-service Debian 13/AlmaLinux 9/openSUSE Leap 16 reinstall, and domain configuration
- CPU, memory, and disk oversubscription; quota changes apply live without a container restart
- Strict container network isolation; prefix mode assigns each container a dedicated `/112` IPv6 block that it can subdivide
- Traffic is counted in both directions; over-quota containers are rate-limited to 1 Mbps both ways
- Compressed ZFS storage by default; the fixed instance limit is 200 containers
- Ports 80/443 forward to container ports 80/443; port 80 uses shared HTTP proxying and port 443 shared SNI proxying

## Installation

Ubuntu 22.04, 24.04, or 26.04 is recommended. Debian 12 and 13 are also supported; Debian requires a kernel-module build and takes longer to install.

Recommended minimum: 1 CPU core, 1.5 GB RAM, 15 GB of free disk space, and root access. Both amd64 and arm64 are supported; testing has focused mainly on amd64.

```sh
git clone https://github.com/bluebluesoda/vpsmgr.git
cd vpsmgr
sudo ./install.sh                  # download the latest prebuilt binary
# sudo ./install.sh --local-build  # always build from this checkout
# sudo ./install.sh --update       # update an existing install from the release
```

The installer first downloads the prebuilt binary from GitHub Releases. If that fails, it falls back to a local build. `--local-build` always rebuilds from source. If `--update` cannot download the release, the existing binary is left untouched.

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

## Additional Images

Debian 13 is the default image. To offer AlmaLinux 9 or openSUSE Leap 16 during reinstall, build it once:

```sh
sudo bash scripts/60-rhel-image.sh          # AlmaLinux 9
sudo bash scripts/70-opensuse-image.sh      # openSUSE Leap 16
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
