<p align="center">
  <img src="assets/opensocks-icon-monochrome.png" alt="OpenSocks" width="192">
</p>

<h1 align="center">OpenSocks for OpenWrt</h1>

<p align="center">
  <a href="README.md">中文</a> | <a href="README.en.md">English</a> | <a href="README.ja.md">日本語</a>
</p>

OpenSocksはTransocks（穿梭）の中国向け回国回線サービスをOpenWrtから利用するためのオープンソースクライアントで、LuCI管理画面とスマホ操作アプリを提供します。サーバー回線の利用にはTransocksアカウントが必要です。Shadowsocksエンジン（TCP REDIRECT・UDP TPROXY・SOCKS5）はGoデーモンに組み込まれており、nftablesと連携して動作します。`shadowsocks-libev`の追加インストールは不要で、大規模な統合プロキシコアも使いません。本プロジェクトは独立実装であり、Transocks公式クライアントではありません。

## 主な機能

- スマート：中国サービスはTransocks中国回線、X・YouTube・Google等は元のWAN回線
- フル中国回線：LAN/Wi-Fiの公開先TCP/UDPをすべてTransocks中国回線へ転送
- TCP REDIRECTとUDP TPROXY
- 選択サーバー固定、セッション更新、経路の自動復元
- Ping順サーバー一覧、接続履歴、再接続
- 中国サービス別通信量、リアルタイム速度、累積通信量
- 中国国内Ookla／SpeedTest.cn測定
- LuCI／Android／iOSの中国語・英語・日本語対応
- **容量の少ない端末向け:** 約1.8KBのminimalランチャーが実行ファイルを`/tmp`へ自動展開し、再起動後も自動復旧
- Shadowsocksエンジンを内蔵し、依存を削減（`libc`、`nftables-json`、`kmod-nft-tproxy`、`ca-bundle`、`uci`）

## インストール

ソフトウェア源は**恒久 URL**です。`releases/latest` は常に最新リリースを指すため、登録した1行は新しいバージョンが出ても書き換える必要がありません。

### OpenWrt 25.12 以降（apk）

OpenWrt 25.12 からパッケージマネージャが opkg から apk に変わり、パッケージ形式は `.apk` になります。

```sh
# 署名鍵を取得（opensocks.pem は全リリースで同一の鍵です）
wget -O /etc/apk/keys/opensocks.pem \
  https://github.com/rticks/OpenSocks/releases/latest/download/opensocks.pem

# 恒久フィードを登録
echo 'https://github.com/rticks/OpenSocks/releases/latest/download/packages.adb' \
  >> /etc/apk/repositories.d/customfeeds.list

apk update
apk add opensocks-minimal luci-app-opensocks
```

更新は同じ行のまま `apk add --upgrade opensocks-minimal luci-app-opensocks` で行えます。`apk del` は依存パッケージも一緒に削除するため、削除する場合は必要なパッケージを入れ直してください。

### OpenWrt 22.03–24.10（opkg）

`.ipk` も同じ恒久 URL から取得できます。署名は行わず、`Packages` をそのまま使います。

```sh
SRC=https://github.com/rticks/OpenSocks/releases/latest/download
echo "src/gz opensocks $SRC" >> /etc/opkg/customfeeds.conf
opkg update
opkg install opensocks-minimal luci-app-opensocks
```

> opkg は `Packages` の `Filename:` を基準に `.ipk` を解決するため、フィードの場所と `.ipk` の場所が同じである必要があります。上記の URL はその条件を満たしています。

apk リポジトリでは `opensocks-minimal` と `luci-app-opensocks` は `noarch`、`opensocks` 完全版は実際の CPU アーキテクチャ（例: `mipsel_24kc`、`aarch64_cortex-a53`）向けに構築されます。

`kmod-nft-tproxy` は汎用 arch リポジトリではなく、ターゲット別の kmods リポジトリにあります。対象ターゲットのリポジトリを未登録の場合、apk はこの依存を解決できません（カーネルモジュールに依存するOpenWrt 公式パッケージと同じ挙動です）。

[GitHub Releases](https://github.com/NarYuki/OpenSocks/releases/latest)からIPKを直接ダウンロードしてインストールすることもできます。インストール、ログイン、機能、スマホ連携の詳しい説明:

完全版パッケージは複数 CPU アーキテクチャ向けに公開されます（例: `mipsel_24kc`、`aarch64_cortex-a53`、`aarch64_cortex-a53_neon-vfpv4`）。`opensocks-minimal` は IPK では `Architecture: all`、apk では `noarch` となり、`uname -m` に応じた daemon バイナリを選択します。

- [日本語 Wiki](https://github.com/NarYuki/OpenSocks/wiki/Home-ja)
- [English Wiki](https://github.com/NarYuki/OpenSocks/wiki/Home-en)
- [中文 Wiki](https://github.com/NarYuki/OpenSocks/wiki)

スマホアプリは、Android版を[最新Release](https://github.com/NarYuki/OpenSocks/releases/latest)から、iOS版を[TestFlight](https://testflight.apple.com/join/eT82PtM1)からインストールできます。

## 開発時の確認

```sh
cd openwrt/opensocks/src
go test ./...
go vet ./...

cd ../../../mobile
flutter analyze
flutter test
```

ライセンスはGPL-3.0-or-laterです。詳細は[LICENSE](LICENSE)を参照してください。
