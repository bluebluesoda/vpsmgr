# Vpsmgr Lite
**警告 ⚠️ 本项目创建的"虚拟机"是 LXC 容器，并非真正的虚拟机，其隔离性与安全性远低于 KVM/QEMU 等其他虚拟化方式。一旦发生内核级或容器逃逸，将影响宿主机及所有租户。安全风险自理——请勿在共享宿主机上运行不受信任或高安全要求的工作负载。**

[English](README.md) · [文档](docs/README.md)

面向小型机器（≤ 4G 内存、小型 VPS）的玩具级 LXC 托管面板：每用户一台 Debian 13，用户通过 Web 面板管理机器（开机/关机/重启/重装），自动 NAT4 端口转发，80/443 按域名由 Traefik 转发。可选 IPv6 直通（无 NAT）。面板是一个很小的 Go 单二进制，容器镜像保持精简——存储和内存都视作稀缺资源。

安装提示：共享 IPv4 入站默认开启，不再询问（可在之后用 `vps config set net.v4_forward true|false` 切换）；安装时唯一可自定义的网络项是容器子网的第二段八位组。容器 25 端口（SMTP）双向永久封禁（反垃圾邮件，无开关）。

## 安装

**最低要求：Ubuntu 24.04（物理机或 KVM）1 核心 1.5G 内存 10G 磁盘空闲 root**

amd64/arm64 均可，主要在amd64上进行了测试

```
git clone https://github.com/bluebluesoda/lxc-hosting.git && cd lxc-hosting
sudo ./install.sh                  # 安装稳定版预编译二进制
#sudo ./install.sh --local-build   # 强制本地编译
```

**如需启用IPv6直通，请确保宿主机获得整段Route**，可以询问服务商，或者使用仓库中的检查脚本进行不严谨的测试。
**务必在测试v6整段可用后再尝试以v6支持进行安装**
```
bash check-ipv6-support.sh #v6测试脚本
```


装完运行 `vps panel-url` 查看完整面板地址——`https://<IP>:<端口>/<随机路径>`(端口为 2000-9999 中随机选取的空闲端口)。该随机路径是面板唯一入口。

## 可选：额外系统镜像

默认系统为 Debian 13。想让用户重装容器时选 RHEL 系系统，可以（以 root）运行一次可选的镜像构建脚本——它不会在 `install.sh` 里自动执行，小型机器保持精简：

```
sudo bash scripts/60-rhel-image.sh          # Alma 9
sudo bash scripts/60-rhel-image.sh rocky     # Rocky 9
```

之后重装弹窗会列出这些镜像供选择（即使只有默认镜像也会让用户选）。镜像构建与 Debian 一致：精简缓存、发布后删除基础镜像。

## 使用

```
vps add <name> [--cpu N] [--mem NG] [--disk NG]   # 默认 1核/1G/10G；cpu 可为整数核（≥1）或 0.1~0.9 小数；密码自动生成，仅显示一次
vps quota <name> [--cpu N] [--mem N] [--disk NG]  # 磁盘只许扩
vps passwd <name>                                 # 重发用户面板密码（仅显示一次）
vps list [name]                                   # 全部用户，或单个详情
vps power <name> start|stop|restart
vps del <name>
vps panel-url
vps config set net.v4_forward true|false   # 共享 IPv4 入站开关：false = 容器仅 IPv6
```

用户可在面板中设置自定义**初始化脚本**——重装后在容器内以 root 自动运行（输出在容器内 /var/log/vpsmgr-init.log），用于云厂商式的首次引导自动化。

管理员可设置每用户的**月度流量配额**（GiB，上传+下载）；超限后容器上下行各限速 **1Mbps**。限速由 Incus 实时应用（tc qdisc），无需重启容器。

域名可选用 **PROXY protocol v2**（443 TLS 直通向后端汇报访客 IP，后端需适配；HTTP/80 保持常规 X-Forwarded-For header）。管理员有**域名管理**页面：列出所有域名及其所属用户、最后修改时间（UTC，按浏览器时区显示），可修改该设置或删除域名。

**审计日志**记录耗资源的用户操作——开关机、重装、重置 root 密码、域名配置变更。操作者标记为操作用户名；管理员对某用户资源操作时标记为 `000+<用户名>`。管理员审计页分片加载（每片 500 条、滚动到底自动加载），保留最近 5000 条。

## 配置

`/etc/vpsmgr/config.yaml`（安装时自动生成）——**默认配置不建议修改**。正规入口是 `vps config list/set/help`，会校验每次改动并拒绝不可变字段。参考：[docs/configuration.md](docs/configuration.md)。

## 卸载

```
sudo ./uninstall.sh          # 卸载软件，保留 config/db/容器
sudo ./uninstall.sh --purge  # 连 config/db、容器、存储池、Incus 一起删
```

## 文档

技术细节在 `docs/`（英文）：[索引](docs/README.md)；另有面向 AI 编程代理的 [`AGENTS.md`](AGENTS.md)。

