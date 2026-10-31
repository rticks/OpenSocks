<p align="center">
  <img src="assets/opensocks-icon-monochrome.png" alt="OpenSocks" width="192">
</p>

<h1 align="center">OpenSocks for OpenWrt</h1>

<p align="center">
  <a href="README.md">中文</a> | <a href="README.en.md">English</a> | <a href="README.ja.md">日本語</a>
</p>

OpenSocks is an open-source OpenWrt client for the Transocks China return-route service, with a LuCI dashboard and mobile controller. A Transocks account is required to use its server lines. The Shadowsocks engine (TCP REDIRECT, UDP TPROXY, SOCKS5) is built into the Go daemon and works with nftables, so `shadowsocks-libev` is no longer needed and no large all-in-one proxy core is involved. This is an independent implementation, not an official Transocks client.

## Highlights

- Smart routing sends Chinese services through the Transocks China line while X, YouTube, Google, and other non-Chinese services use the original WAN
- Full China routing sends all public TCP/UDP from LAN and Wi-Fi through the Transocks China line
- TCP REDIRECT and UDP TPROXY
- Persistent server selection, session recovery, and automatic route restoration
- Latency-sorted servers, connection history, and reconnect actions
- Per-service China traffic, live rates, and persistent totals
- Ookla China and SpeedTest.cn testing
- Chinese, English, and Japanese LuCI/mobile interfaces
- **Low-storage option:** an approximately 1.8KB minimal launcher expands the daemon into `/tmp` and restores it automatically after reboots
- Built-in Shadowsocks engine with a small dependency set (`libc`, `nftables-json`, `kmod-nft-tproxy`, `ca-bundle`, `uci`)

## Installation

The feed is a **permanent URL**: `releases/latest` always resolves to the newest
release, so the line you register keeps working after every version bump.

### OpenWrt 25.12 and later (apk)

OpenWrt 25.12 replaced opkg with apk and the package format with `.apk`.

```sh
# Trust the signing key (opensocks.pem is the same key in every release).
wget -O /etc/apk/keys/opensocks.pem \
  https://github.com/rticks/OpenSocks/releases/latest/download/opensocks.pem

# Register the permanent feed.
echo 'https://github.com/rticks/OpenSocks/releases/latest/download/packages.adb' \
  >> /etc/apk/repositories.d/customfeeds.list

apk update
apk add opensocks-minimal luci-app-opensocks
```

Upgrade with the same registration: `apk add --upgrade opensocks-minimal luci-app-opensocks`.
Note that `apk del` also removes dependencies that nothing else needs; reinstall
them if you remove the package.

### OpenWrt 22.03–24.10 (opkg)

The `.ipk` files are served from the same permanent URL. The feed is unsigned,
which opkg accepts.

```sh
SRC=https://github.com/rticks/OpenSocks/releases/latest/download
echo "src/gz opensocks $SRC" >> /etc/opkg/customfeeds.conf
opkg update
opkg install opensocks-minimal luci-app-opensocks
```

> opkg resolves each `.ipk` relative to the location of `Packages`, so the feed
> URL and the `.ipk` assets must share a directory. The URL above does.

In the apk feed `opensocks-minimal` and `luci-app-opensocks` are `noarch`; the full `opensocks` package is built for the real CPU architecture (for example `mipsel_24kc`, `aarch64_cortex-a53`).

`kmod-nft-tproxy` ships in the per-target kmods feed rather than the generic arch feed. If your device has not registered its target's feed, apk cannot resolve that dependency — the same behaviour as any OpenWrt package that needs a kernel module.

You can also download IPKs from [GitHub Releases](https://github.com/NarYuki/OpenSocks/releases/latest) and install them directly. Full installation, login, feature, and mobile-pairing documentation:

Full packages are published for multiple CPU architectures (for example `mipsel_24kc`, `aarch64_cortex-a53`, and `aarch64_cortex-a53_neon-vfpv4`). `opensocks-minimal` is `Architecture: all` in the IPK feed and `noarch` in the apk feed; it selects the matching daemon binary from `uname -m`.

- [English Wiki](https://github.com/NarYuki/OpenSocks/wiki/Home-en)
- [中文 Wiki](https://github.com/NarYuki/OpenSocks/wiki)
- [日本語 Wiki](https://github.com/NarYuki/OpenSocks/wiki/Home-ja)

Mobile app: download the Android APK from the [latest release](https://github.com/NarYuki/OpenSocks/releases/latest), or install the iOS app through [TestFlight](https://testflight.apple.com/join/eT82PtM1).

## Development checks

```sh
cd openwrt/opensocks/src
go test ./...
go vet ./...

cd ../../../mobile
flutter analyze
flutter test
```

Licensed under GPL-3.0-or-later. See [LICENSE](LICENSE).
