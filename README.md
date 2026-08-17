# Vpsmgr Lite

轻量级 Incus 容器托管面板，适合小型 VPS 和低配主机。

> **安全警告**：这里的“虚拟机”是 LXC 容器，不是真正的虚拟机。容器隔离性弱于 KVM/QEMU。容器逃逸或内核漏洞可能影响宿主机和其他租户。不要在共享主机上运行不受信任或高安全要求的工作负载。

[English](README.en.md) · [文档](docs/README.md)

## 功能

- 每个用户一个 Debian 13 容器
- Web 面板管理：启动、停止、重启、重装
- IPv4 NAT、SSH 和端口转发
- Traefik 代理 HTTP/HTTPS 域名
- 可选 IPv6 直通：前缀模式或地址池模式，无 NAT
- CPU、内存、磁盘和月度流量配额
- 管理员面板、域名管理、IPv6 地址池和审计日志
- 单个 Go 二进制，容器镜像保持精简

## 安装

推荐使用 Ubuntu 22.04、24.04 或 26.04。也支持 Debian 12/13，但 Debian 安装 ZFS 时通常需要额外编译 DKMS 模块，安装时间可能明显更长。

最低建议：1 核、1.5 GB 内存、10 GB 可用磁盘空间，以及 root 权限。amd64 和 arm64 均支持，主要在 amd64 上测试。

```sh
git clone https://github.com/bluebluesoda/vpsmgr.git
cd vpsmgr
sudo ./install.sh                  # 下载最新预编译版本
# sudo ./install.sh --local-build  # 强制从当前源码编译
# sudo ./install.sh --update       # 更新现有安装的预编译版本
```

默认安装会先下载 GitHub Release 中的预编译二进制；下载失败时会回退到本地编译。`--local-build` 总是重新编译，`--update` 下载失败时保留现有二进制。

安装器会配置 Zabbly Incus 7 LTS 软件源，并默认使用 ZFS 存储。测试环境可显式使用 `VPSMGR_STORAGE=dir`，但该模式没有 ZFS 的配额、快照和克隆能力：

```sh
sudo VPSMGR_STORAGE=dir ./install.sh
```

### IPv6

IPv6 直通默认关闭。交互式安装会询问是否启用，并根据网络条件提供以下模式：

- **前缀模式**：服务商将完整 IPv6 前缀路由到宿主机
- **地址池模式**：服务商提供多个可用的独立 IPv6 地址，在管理员面板中添加
- **关闭**：仅使用 IPv4

需要检查 IPv6 时，可先运行：

```sh
bash check-ipv6-support.sh
```

## 使用

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

默认配额为 1 核、1 GB 内存和 10 GB 磁盘。CPU 支持整数核心，或 `0.1` 到 `0.9` 的小数核心；磁盘只能扩容。密码只显示一次。

月度流量配额单位为 GiB，统计上传和下载总量。超额后容器双向限速为 1 Mbps，无需重启。

用户可以在面板设置初始化脚本。重装后脚本会在容器内以 root 运行，日志位于 `/var/log/vpsmgr-init.log`。

## 额外镜像

默认镜像是 Debian 13。需要 AlmaLinux 9 或 Rocky Linux 9 时，可手动构建一次：

```sh
sudo bash scripts/60-rhel-image.sh          # AlmaLinux 9
sudo bash scripts/60-rhel-image.sh rocky    # Rocky Linux 9
```

该脚本不会由 `install.sh` 自动执行，以减少小型主机的安装负担。

## 配置与卸载

配置文件为 `/etc/vpsmgr/config.yaml`。建议使用以下命令修改，而不是直接编辑文件：

```sh
vps config list
vps config help
vps config set <key> <value>
```

```sh
sudo ./uninstall.sh          # 删除软件，保留配置、数据库和容器
sudo ./uninstall.sh --purge  # 额外删除配置、数据库、容器、存储池和 Incus
```

更多内容见 [文档索引](docs/README.md)。

## 截图

![管理员面板](ScreenShot-AdminPanel.png)

![用户面板](ScreenShot-UserPanel.png)
