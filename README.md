# Vpsmgr Lite

轻量级 Incus 容器托管面板，适合小型 VPS 和低配主机。

> **安全警告**：这里的“虚拟机”是 LXC 容器，不是真正的虚拟机。容器隔离性弱于 KVM/QEMU。容器逃逸或内核漏洞可能影响宿主机和其他租户。不要在共享主机上运行不受信任或高安全要求的工作负载。

[English](README.en.md) · [文档](docs/README.md)

## 功能细节

- 管理员面板：用户、配额、域名、IPv6 地址池、SSH 公钥与审计日志；支持"以用户身份登录"接入用户面板
- 用户面板：电源控制、自助重装 Debian 13/AlmaLinux 9/openSUSE Leap 16/Arch Linux、域名与 SSH 密钥管理、加密便签（可导出）
- 管理员可为每个用户分配专属颜色（在用户列表中点击用户名）：该用户的「登录面板」按钮与其用户面板背景会采用此颜色，方便代客登录时区分不同用户
- CPU、内存、磁盘支持超售；配额修改实时生效，无需重启容器
- 容器网络严格隔离；IPv6 前缀模式下每个容器获得独立 `/112`，可继续划分子网
- 流量按上下行合计，超额后双向限速至 1 Mbps
- 默认使用压缩 ZFS 存储池；容器数量上限为 200
- 80/443 转发到容器的 80/443；80 使用共享 HTTP 转发，443 使用共享 SNI 转发

## 安装

推荐使用 Ubuntu 24.04。Ubuntu 22+ / Debian 12+ 均可使用；Debian 需要编译内核模块，安装时间较长。

最低建议：1 核、1 GB 内存、15 GB 可用磁盘空间，以及 root 权限。amd64 和 arm64 均支持，主要在 amd64 上测试。

**从预编译安装**
```bash
bash <(curl -fsSL https://raw.githubusercontent.com/bluebluesoda/vpsmgr/refs/heads/main/oneclick.sh)
```
**从预编译更新**
```bash
bash <(curl -fsSL https://raw.githubusercontent.com/bluebluesoda/vpsmgr/refs/heads/main/oneclick.sh) --update
```

**从源代码编译安装**
```sh
git clone --depth 1 https://github.com/bluebluesoda/vpsmgr.git && cd vpsmgr
sudo ./install.sh --local-build 
```

如果这台主机已有其他公网服务，且你只需要 IPv6 入站、不需要 v4 SSH/端口转发，可以使用：

```sh
sudo ./install.sh --disable-v4forward
```

该参数会在安装开始时要求确认；确认后写入 `net.v4_forward=false`，跳过 vpsmgr 保留端口检查，只随机选择一个面板入口端口。Traefik 仍会安装但不会启动。以后可用 `vps config set net.v4_forward true` 恢复 IPv4 入站转发。Traefik 也可通过 `vps config set net.traefik false` 独立关闭；关闭后不会运行或自启，且不能添加新域名。

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

容器 swap 由 `incus.swap_ratio` 控制（默认 0.5，即 1 GiB 内存容器最多用 512 MiB 宿主 swap）。设置该配置后立即应用到所有容器（无需重启）；升级到包含 swap 支持的版本后，运行 `vps install` 会为老容器补齐 swap 配额。

用户可以在面板设置初始化脚本。重装后脚本会在容器内以 root 运行，日志位于 `/var/log/vpsmgr-init.log`。

管理员可在用户列表中**点击用户名**（一个小隐藏功能）为该用户挑选专属颜色，或点「清除颜色」恢复默认。该颜色会应用到该用户的「登录面板」按钮和其用户面板背景（明亮/暗色模式均适配）；用户无法自行修改。

## 额外镜像

默认镜像是 Debian 13。需要 AlmaLinux 9、openSUSE Leap 16、Arch Linux 或 Debian 开发镜像时，可手动构建一次：

```sh
sudo bash scripts/60-rhel-image.sh          # AlmaLinux 9
sudo bash scripts/70-opensuse-image.sh      # openSUSE Leap 16
sudo bash scripts/80-debian-dev-image.sh    # Debian 13 开发镜像（含完整开发工具链）
sudo bash scripts/90-arch-image.sh          # Arch Linux（滚动发行版，每次运行都会重建为最新快照）
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
