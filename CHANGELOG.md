# Changelog

## 0.3.4

### The feed refresh no longer rolls the installed release back

- Fixed a package upgrade being undone on the next service start. `refresh_release_metadata()` reads a usign-signed `Packages` index and wrote its `binary_url`/`binary_sha256` into UCI unconditionally. The default feed is the `rel.n4t.su` mirror, which still advertises 0.2.13. On a 25.12.6 mipsle router the sequence was: `apk add --upgrade` installed 0.3.3 and its `post-upgrade` pinned `binary_url` to the v0.3.3 asset; the next `service start` read the stale mirror index and wrote 0.2.13's URL and SHA back into UCI; the launcher then downloaded and ran 0.2.13 while `apk list -I` reported 0.3.3 installed. The refresh is now a fallback only: it returns without touching UCI when the installed pin is a well-formed 64-hex SHA-256 plus a URL. A missing or malformed pin is still filled in from the index, so a router whose UCI was reset or that was installed from a pre-multi-architecture release still gets a working URL. A router that wants a newer build upgrades the package, whose `post-upgrade` repins it.
- Added `openwrt/opensocks-minimal/tests/feed_pin_test.sh`, which extracts the shipped refresh function, stubs `uci`/`wget`/`usign`, and asserts a usable pin survives an index advertising an older build while a missing or malformed pin is filled in. It runs in CI.

### Packaging

- Bumped `opensocks`, `opensocks-minimal` and `luci-app-opensocks` to 0.3.4.

## 0.3.3

### Package upgrade scripts

- Fixed an upgraded router never running the package's setup script. apk runs `post-install` only for a fresh install and `post-upgrade` only when replacing an existing version (`src/database.c`: `(oldpkg == NULL) ? APK_SCRIPT_POST_INSTALL : APK_SCRIPT_POST_UPGRADE`), and both `opensocks` and `opensocks-minimal` registered only `post-install`. The consequence was observed on a 25.12.6 mipsle router: `apk add --upgrade` moved `opensocks-minimal` to 0.3.2-r1 while `uci get opensocks.settings.binary_url` still pointed at the v0.3.1 asset, so the launcher went on downloading the previous daemon and the upgrade changed nothing the user could see. Both packages now register `post-upgrade` with the same body as `post-install`, so the URL and SHA-256 are repinned and the service is restarted on upgrade as well as on install.
- The daemon package's `post-upgrade` also restarts the service: without it an upgrade replaced `/usr/lib/opensocks/launcher.sh` while procd kept the already-running daemon, so the new launcher only took effect at the next reboot.
- Added a release-time check that dumps every produced package with `apk adbdump` and fails when `opensocks` or `opensocks-minimal` does not carry both `post-install` and `post-upgrade`.

### Packaging

- Bumped `opensocks`, `opensocks-minimal` and `luci-app-opensocks` to 0.3.3.

## 0.3.2

### Traffic accounting and connection state

- Fixed the traffic panel staying at zero while the tunnel moved gigabytes. On nftables 1.1.6 (OpenWrt 25.12) a multi-line `nft -f -` script resolves only the first `list counter` line and fails the rest with "No such file or directory; did you mean obj ...", so the command exited non-zero and the sampler read an empty map. It now issues one `nft list counters inet opensocks`, which dumps every counter in a single call. Reproduced and verified on a 25.12 mipsle router: the old form failed there and the new one returns `proxy_up`, `proxy_down` and every `svc_*` counter.
- Fixed the UI reporting "Connected" while nothing could leave the router. `isRunning()` only proves the daemon owns its listening ports, and a bound listener survives an upstream that has stopped answering, so a router whose account had expired kept showing Connected forever. The engine now dials its upstream on the supervisor tick and caches the verdict for 25 s, and `status()` reports that verdict as `running`. The listener-only view is still available as `listenersBound`, so a diagnostic can tell "no listener" from "listener up, upstream dead". A dead upstream is logged so the failure is no longer silent.
- Made automatic recovery handle the dead-upstream case. `connect()` returns early while the engine is running, so the watchdog's old `isRunning()` check could never re-establish a tunnel whose listeners were still bound but whose server had gone away. It now compares the upstream verdict and tears the engine down before reconnecting.
- Tolerated one failed probe as a blip: the tunnel is declared down only after two consecutive failures (~60 s), so a single lost packet does not flap the UI to Disconnected.
- Cleared the cached upstream verdict and failure count on stop, so reconnecting to a different server cannot inherit the previous server's reachability.

### Minimal launcher

- Fixed an installed package never taking effect. The launcher executed `/tmp/opensocks/opensocks` as soon as the file existed, without comparing it to the SHA-256 the package ships. `/tmp` is a tmpfs that survives a service restart (it only resets on reboot), so after the first boot every later package upgrade kept running the binary from the first install: `apk list -I` reported the new version while the daemon did not change. The launcher now records the release it unpacked in `/tmp/opensocks/opensocks.sha256` and only takes the fast path when that stamp matches the configured `binary_sha256`; otherwise it re-downloads. A binary with no stamp is never trusted.
- Fixed "Text file busy" crash loops on restart. Both unpack paths wrote straight into the executable the previous daemon was still exiting from, so procd logged `exec: ... Text file busy` and counted crashes. The daemon is now unpacked to `opensocks.new` and renamed into place, and both rename failures are reported instead of ignored.
- Added `openwrt/opensocks-minimal/tests/launcher_cache_test.sh`, which drives the shipped fast-path condition against stamped, unstamped and stale caches and asserts the unpack never writes into the running executable. It runs in CI next to the architecture test.
- Documented the two URL layouts the launcher accepts and that the archive hash, not the URL, is the trust boundary.

### Packaging

- Bumped `opensocks`, `opensocks-minimal` and `luci-app-opensocks` to 0.3.2.

## 0.3.1

### OpenWrt 25.12 fixes for the minimal package

- Fixed `opensocks-minimal` never starting on a router that reports only `mips`. The byte-order probe used `od`, which OpenWrt's stock BusyBox does not ship (`od: not found`), so detection failed and the launcher exited with "OpenSocks does not support CPU architecture: mips". It now reads the same ELF byte with `hexdump`, which is present by default.
- Fixed the download-on-boot URL. It was hardcoded to the `rel.n4t.su` mirror, which still publishes 0.2.13, so every 0.3.0 URL returned 404 and the daemon was never fetched — LuCI reported "daemon unreachable". The URL is now generated at build time from `BINARY_BASE` plus `OPENSOCKS_BINARY_LAYOUT` (`flat` for a GitHub Release, `by-sha` for the mirror), and CI points it at the release that ships the archive.
- Stopped the launcher retrying forever. It now makes five attempts, logs why each one failed (HTTP error, checksum mismatch, not a gzip), and exits non-zero so procd can apply its own backoff instead of leaving the service looking active with the API closed.
- Accepted either URL layout in the launcher and pinned the file name to the current CPU, so a tampered `binary_url` cannot point at a binary for another architecture.
- Made the `usign` metadata refresh a no-op where opkg does not exist, instead of failing on the missing `/etc/opkg/keys` path.
- Added `openwrt/opensocks-minimal/tests/architecture_test.sh`: 27 cases across the launcher and both generated post-install scripts, run in CI with `od` removed and `hexdump` required.
- Added a release-time check that fetches the daemon URL baked into the published minimal package and fails the build if it does not resolve.

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
- Assembled the `mkpkg` and `mkndx` argument vectors with `set --` instead of inline `${VAR:+word}` expansion. In POSIX sh the quotes inside that expansion are literal, so a signing-key path containing whitespace was split into two arguments and reached apk as a malformed `--sign-key`.
- Made the apk signing key mandatory in `tools/build-apk-repo.sh` itself rather than only when `REQUIRE_SIGNATURE=1`: without a key the script used to continue and die on `cp: cannot stat ''` while publishing the public key. It now stops up front and names the variables to set.
- Made the repository's own apk key the publishing target. `keys/opensocks-apk.pem` is the committed public half (an EC P-256 key independent of any upstream key), GitHub Actions reads the matching secret from `APK_SIGNING_KEY_PEM`/`APK_PUBLIC_KEY_PEM`, and a build without it fails instead of publishing an uninstallable feed. CI also compares the published `opensocks.pem` against the committed anchor so a secret/anchor mismatch cannot ship.
- Made the opkg/IPK feed explicitly optional for this fork: it is still built and verified, but it is signed only when an `OPKG_SIGNING_KEY` secret exists. The apk feed is the one this repository publishes.

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
