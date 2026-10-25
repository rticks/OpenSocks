# Changelog

## 0.3.0

### OpenWrt 25.12 / apk

- Added OpenWrt 25.12 support, where apk replaced opkg and packages use the `.apk` (APKv3) format.
- Published a signed apk repository (`packages.adb`) from the same release build that produces the opkg feed, so 22.03-24.10 and 25.12+ stay in lockstep.
- Signed the index and packages with an EC P-256 key, using the same scheme OpenWrt itself uses.
- Reported `noarch` for the minimal launcher and the LuCI app, and the real architecture (`mipsel_24kc`, `aarch64_cortex-a53`, ...) for the daemon.
- Kept `kmod-nft-tproxy` as a dependency for the UDP TPROXY path; it is resolved from the per-target kmods feed like any OpenWrt kernel module dependency.
- Verified every build by installing the feed into a throwaway root for one `noarch` and one real architecture before publishing.
- Fixed the release job dropping the entire apk feed: `gh release create "$RELEASE_DIR"/*` only matches top-level files, so `repo/` was never uploaded. The apk feed is now staged into the flat release asset set (renaming its own `SHA256SUMS` to `apk-SHA256SUMS` to avoid the opkg manifest of the same name).
- Extended `tools/sync-opkg-release.sh` to publish the apk feed under `<publish-root>/opkg/apk/`, next to the opkg index, so 25.12 clients get it from the same host.
- Aligned the apk feed URL printed by `tools/build-release.sh` with the published location (`<feed-base>/apk`), which previously pointed at the feed root.
- Fixed the aarch64 packages never being built: both feed scripts wrote their target list without a trailing newline, and `while IFS= read -r` silently drops a final line that has none, so the last target (`arm64`) was skipped and only `mipsel_24kc` was produced.
- Fixed dependency lists being comma separated. apk splits dependencies on spaces and treats `,` as a legal name character, so `libc,nftables-json,...` parsed as one impossible package name; the daemon silently got a garbage dependency and the LuCI package failed outright once a version constraint appeared mid-string.
- Fixed the apk feed never being installable: package signing belongs to `mkpkg` (it is what writes the ADB signature), and `mkndx`/`apk verify` only trust keys loaded through `--keys-dir`, so the public half is now staged for them.
- Install-tested the feed in a throwaway root on every build, one engine per root because the full and minimal packages conflict by design, with every external dependency stubbed.

### Shadowsocks engine

- Fixed `upstreamFromBoot` leaving whitespace-padded `Method` values and empty methods untouched. It now trims `Method` and falls back to the default cipher, so a padded line from the API no longer fails cipher resolution.
- Removed the `shadowsocks-libev` dependency: 25.12 no longer ships `ss-redir`/`ss-local`, so the engine is now compiled into the single Go daemon.
- Implemented TCP REDIRECT, UDP TPROXY, and SOCKS5 in-process, with AEAD and stream ciphers.
- Reduced the dependency set to `libc`, `nftables-json`, `kmod-nft-tproxy`, `ca-bundle`, and `uci`.
- Kept runtime state in `/etc/opensocks` and scratch data in `/tmp/opensocks`; the former `engine.yaml` configuration file is gone.
- Terminated `ss-redir`/`ss-local` children left behind by an upgrade so no stale helper keeps a port open.

### LuCI

- Kept the LuCI application on the Lua MVC (`luasrc/`); it is not ported to ucode yet, so `luci-compat` and `luci-lua-runtime` remain runtime dependencies.
- Declared `curl` as a dependency in both feeds: the controller proxies to the daemon by shelling out to `/usr/bin/curl`, so it was already required but previously undeclared.
- Confirmed `luci-base`, `luci-compat`, `luci-lua-runtime`, and `curl` are all present in the 25.12 LuCI feed.

## 0.2.13

### OpenWrt platforms

- Merged multi-architecture OpenWrt package support contributed by [@haotian-lu-prog](https://github.com/haotian-lu-prog) in [#2](https://github.com/NarYuki/OpenSocks/pull/2).
- Fixed minimal-package CPU detection on little-endian MIPS routers whose kernel reports the generic `mips` architecture name.
- Continued to reject binaries whose CPU architecture or byte order does not match the router.

## 0.2.12

### OpenWrt platforms

- Added signed daemon and full-package builds for mipsel and arm64 routers.
- Added package architecture aliases for stock OpenWrt and GL.iNet arm64 firmware.
- Made the minimal package select only a verified binary matching the router CPU.
- Kept existing mipsel minimal installations compatible with the previous release metadata.

### Release delivery

- Added multi-architecture assets to the signed opkg feed synchronization.
- Skipped synchronization when the same release is already active to prevent duplicate downloads and storage growth.

## 0.2.11

### Session servers

- Added independent server selection for the second and third sessions in LuCI and the mobile client.
- Kept every additional session linked to the main server by default while allowing either session to be pinned separately.
- Preserved per-session server selections across service and router restarts.
- Excluded and accounted for every active VPN server address when sessions use different endpoints.

## 0.2.10

### Smart routing

- Captured LAN DNS through the router so changing service subdomains are learned automatically.
- Added all `.cn` domains and every known Chinese-service subdomain to the live Smart routing set.
- Prevented encrypted-DNS and IPv6 paths from bypassing the observable IPv4 Smart route.
- Routed and classified Honor of Kings battle servers that are supplied as direct overseas IP addresses.

### Controls

- Applied mobile routing and session selections immediately when an item is tapped and removed the extra Apply button.
- Preserved unsaved LuCI settings while status information refreshes in the background.

## 0.2.9

### Routing controls

- Fixed the mobile routing sheet dropping Smart/Full China and Single/Dual/Triple changes while restoring the native navigation bar.
- Kept the routing operation active after the sheet closes so authentication, network configuration, interface processing, completion, and errors remain visible on the main screen.
- Fixed the LuCI proxy omitting `session_count` from settings requests.
- Prevented LuCI status polling from replacing edited routing values while a settings request is running.
- Kept the selected routing and session modes after service and full-router restarts.

## 0.2.8

### Multi-session UDP routing

- Enabled UDP forwarding on every active session in Dual and Triple modes.
- Added symmetric flow hashing so each UDP flow remains pinned to one session while separate flows are distributed across the available sessions.
- Kept the same policy-routing mark on every UDP slot so all selected flows use the existing TPROXY route safely.

## 0.2.7

### Mobile interface

- Unified Smart/Full China routing and Single/Dual/Triple session selection in one routing-mode sheet.
- Displayed the selected routing mode and session mode together on the home routing card.
- Applied both selections in one network reconfiguration and removed the duplicate session selector from Settings.

## 0.2.6

### Multi-session routing

- Added Single, Dual, and Triple session modes as supported operating modes in LuCI and the mobile client.
- Kept Single mode as the default for new installations while preserving the selected mode across service and router restarts.
- Connected every active session to the selected Transocks line and distributed new China-bound TCP flows across two or three isolated routes with nftables.
- Added active-session reporting to the control API and mode-aware status indicators to LuCI and the mobile client.
- Preserved compatibility with installations that previously enabled the dual-session setting.

### Speed testing

- Automatically selected the speed-test stream count for each session mode.
- Added three-session speed-test pooling with six internal transfer streams in Triple mode.
- Improved the China-route benchmark connection pool, idle-connection cleanup, progress update frequency, and memory reclamation on low-memory routers.
- Expanded Ookla endpoint selection and improved failure handling for SpeedTest.cn and external mainland test servers.

### Traffic and routing

- Expanded China-service classification and corrected service names in the traffic ranking.
- Improved DNS observation and dynamic service-address tracking for subdomains and short-lived endpoints.
- Added balanced two-way and three-way nftables TCP dispatch while retaining the primary session for UDP routing.
- Paused background traffic sampling and route recovery during speed tests to reduce CPU and memory contention.

### Reliability

- Added isolated persistent state for each active session and made multi-session login and connection setup failure-safe.
- Improved connection retry behavior for transient Transocks authentication failures.

## 0.2.2

### Security

- Added request-body, header-size, connection-concurrency, and HTTP timeout limits to the control servers.
- Serialized shared Transocks API state and connection operations to prevent concurrent session and configuration corruption.
- Pinned GitHub Actions, the LuCI build source, and the China IPv4 route list to verified immutable revisions or SHA-256 hashes.
- Added response-size limits and persistent connection management to the mobile client.
- Updated `flutter_secure_storage` and raised the supported Android API floor required by the newer secure-storage backend.

### Performance

- Replaced repeated per-setting UCI process launches with one snapshot read and one commit per settings update.
- Attached a classic BPF filter to the DNS observer so unrelated LAN frames are discarded in the kernel.
- Added 30-minute expiry to learned domain and service IPs, plus bounded daemon-side caches.
- Reduced persistent traffic writes from every five minutes to every fifteen minutes while preserving one-second in-memory statistics.
- Stopped hidden mobile tabs from polling traffic and prevented overlapping polling requests.
- Limited LuCI log reads to the latest 128 KiB and added 512 KiB log rotation.

### Reliability

- Added HTTP client/server timeouts and bounded concurrent request handling.
- Fixed races in API authentication state, settings refresh, and server switching.
- Corrected JSON error response headers.
- Rejected invalid China route data before applying nftables rules.
- Fixed immutable `by-sha` opkg URLs on HTTP servers that reject symbolic links.

### Distribution

- Published signed full, minimal, and LuCI packages through GitHub Releases and `rel.n4t.su`.
