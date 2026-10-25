<p align="center">
  <img src="assets/opensocks-icon-monochrome.png" alt="OpenSocks" width="192">
</p>

<h1 align="center">OpenSocks for OpenWrt</h1>

<p align="center">
  <a href="README.md">中文</a> | <a href="README.en.md">English</a> | <a href="README.ja.md">日本語</a>
</p>

OpenSocks 是用于在 OpenWrt 上使用 Transocks（穿梭）中国回国线路服务的开源客户端，可通过 LuCI 管理界面或手机应用操作。使用服务器线路需要 Transocks 账户。Shadowsocks 引擎（TCP REDIRECT、UDP TPROXY、SOCKS5）已内置在 Go 守护进程中，配合 nftables 工作，无需额外安装 `shadowsocks-libev`，也不需要大型一体化代理核心，适合存储和内存有限的路由器。本项目是独立实现，并非 Transocks 官方客户端。

## 主要功能

- 智能中国路由：中国服务经 Transocks 中国线路，X、YouTube、Google 等经原有 WAN
- 全中国线路路由：LAN/Wi-Fi 的公网 TCP/UDP 全部经 Transocks 中国线路
- TCP REDIRECT 与 UDP TPROXY
- 自动恢复路由、登录会话和固定选择的服务器
- 服务器延迟排序、历史记录和一键重连
- 中国服务分类流量统计、实时速率和累计流量
- Ookla 中国节点与 SpeedTest.cn 测速
- LuCI、Android 和 iOS 支持中文、英文、日文
- **低容量设备方案**：约 1.8KB 的 minimal 启动器将程序自动展开到 `/tmp`，重启后自动恢复
- 内置 Shadowsocks 引擎，依赖精简（`libc`、`nftables-json`、`kmod-nft-tproxy`、`ca-bundle`、`uci`）

## 安装

推荐使用软件源：

```sh
wget -O /etc/opkg/keys/d24a5e234001294c https://rel.n4t.su/opkg/d24a5e234001294c
echo 'src/gz opensocks https://rel.n4t.su/opkg' >> /etc/opkg/customfeeds.conf
opkg update
opkg install opensocks-minimal luci-app-opensocks
```

### OpenWrt 25.12 及以上（apk）

OpenWrt 25.12 起包管理器由 opkg 换成 apk，包格式为 `.apk`。两个软件源可同时存在：22.03–24.10 用 opkg，25.12+ 用 apk。

```sh
# OpenWrt 25.12+ (apk): register the feed, trust the key, install.
wget -O /etc/apk/keys/opensocks.pem https://rel.n4t.su/opkg/apk/opensocks.pem
echo 'https://rel.n4t.su/opkg/apk/packages.adb' >> /etc/apk/repositories.d/customfeeds.list
apk update
apk add opensocks-minimal luci-app-opensocks
```

`opensocks-minimal` 与 `luci-app-opensocks` 在 apk 源中的架构为 `noarch`；`opensocks` 完整包按真实 CPU 架构构建（例如 `mipsel_24kc`、`aarch64_cortex-a53`）。

`kmod-nft-tproxy` 来自目标平台的 kmods 源，而非通用 arch 源。若你的设备尚未注册对应的目标源，apk 会提示无法解析该依赖 —— 这与 OpenWrt 官方依赖内核模块的软件包行为一致。

也可以从 [GitHub Releases](https://github.com/NarYuki/OpenSocks/releases/latest) 下载 IPK 后直接安装。完整的安装、登录、功能和手机配对说明请参阅：

完整包现同时提供多种 CPU 架构（例如 `mipsel_24kc`、`aarch64_cortex-a53`、`aarch64_cortex-a53_neon-vfpv4`）。`opensocks-minimal` 在 IPK 中为 `Architecture: all`、在 apk 中为 `noarch`，会按 `uname -m` 自动选择对应的 daemon 二进制。

- [中文 Wiki](https://github.com/NarYuki/OpenSocks/wiki)
- [English Wiki](https://github.com/NarYuki/OpenSocks/wiki/Home-en)
- [日本語 Wiki](https://github.com/NarYuki/OpenSocks/wiki/Home-ja)

手机应用：Android APK可从[最新Release](https://github.com/NarYuki/OpenSocks/releases/latest)下载；iOS版通过[TestFlight](https://testflight.apple.com/join/eT82PtM1)安装。

## 项目结构

```text
openwrt/opensocks/          Go 服务和完整软件包
openwrt/opensocks-minimal/  下载到 tmpfs 的最小启动器
openwrt/luci-app-opensocks/ LuCI 界面和本地化
mobile/                     Flutter Android/iOS 客户端
tools/build-release.sh      IPK、opkg 索引、apk 索引和移动端发布构建
tools/build-apk-tool.sh     构建与 OpenWrt 25.12 一致的 apk-tools
tools/build-apk-repo.sh     生成 .apk 包和已签名的 packages.adb
```

## 开发测试

```sh
cd openwrt/opensocks/src
go test ./...
go vet ./...

cd ../../../mobile
flutter analyze
flutter test
```

## 许可证

GPL-3.0-or-later，详见 [LICENSE](LICENSE)。
