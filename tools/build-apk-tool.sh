#!/bin/sh
# Build the pinned apk-tools release that OpenWrt 25.12 ships, so that .apk
# packages and the packages.adb repository index can be produced without an
# OpenWrt buildroot checkout.
#
# Everything below is verified against openwrt/openwrt openwrt-25.12:
#   package/system/apk/Makefile  PKG_VERSION=3.0.5
#                                PKG_SOURCE_VERSION=b5a31c0d86...
#                                MESON_HOST_ARGS=-Dcrypto_backend=openssl
#                                  -Ddefault_library=static ...
#   apk-tools meson_options.txt   crypto_backend, url_backend, docs, help, lua,
#                                 python, tests, zstd, minimal, arch, ...
#   apk-tools meson.build         version: run_command('./get-version.sh')
#   apk-tools src/meson.build     executable('apk', ...) -> <build>/src/apk
#
# The version is *not* a Meson option: get-version.sh reads the VERSION
# environment variable first, which is why OpenWrt passes MESON_VARS=VERSION.
# Upstream apk-tools lives on GitLab; the GitHub mirror is used here because it
# is reachable from every CI runner and supports shallow commit fetches.
#
# Deliberate differences from the OpenWrt target build:
#   * -Dhelp=disabled instead of enabled: the help database needs a host Lua
#     interpreter. mkpkg, mkndx and adbsign do not read it.
#   * -Durl_backend=libfetch instead of wget: no external wget process is
#     needed on the runner. Packaging never fetches anything anyway.
#   * -Dminimal keeps its default of false; the minimal applet set excludes
#     mkpkg, mkndx and adbsign, which the release tooling needs.
#   * -Ddefault_library=static is kept, so libapk and libfetch are linked into
#     the tool. OpenSSL and zlib stay dynamic and come from the build host.
#   * --buildtype=release instead of OpenWrt's plain/debug toggle; this only
#     changes optimisation flags, not the produced package format.
#
# Prerequisites: git, meson >= 0.64, ninja, a C compiler, pkg-config, and the
# OpenSSL and zlib development headers (libssl-dev, zlib1g-dev on Debian).
#
# Usage: tools/build-apk-tool.sh <install-dir>
#   APK_TOOLS_SRC   reuse an existing checkout at the pinned commit instead of
#                   cloning (offline or repeated local builds).
#   APK_TOOLS_REPO  git remote to fetch from.
#   APK_TOOLS_COMMIT / APK_TOOLS_VERSION  override the pinned release.
set -eu

APK_TOOLS_REPO="${APK_TOOLS_REPO:-https://github.com/alpinelinux/apk-tools.git}"
# Matches PKG_SOURCE_VERSION in openwrt/openwrt:package/system/apk/Makefile for
# the openwrt-25.12 branch. This commit is the "apk-tools-3.0.5" release tag.
APK_TOOLS_COMMIT="${APK_TOOLS_COMMIT:-b5a31c0d865342ad80be10d68f1bb3d3ad9b0866}"
APK_TOOLS_VERSION="${APK_TOOLS_VERSION:-3.0.5}"
DEST="${1:-${APK_TOOLS_DIR:-}}"
[ -n "$DEST" ] || {
	printf 'usage: %s <install-dir>\n' "$0" >&2
	exit 1
}

for tool in git grep meson ninja; do
	command -v "$tool" >/dev/null 2>&1 || {
		printf 'required build tool not found: %s\n' "$tool" >&2
		exit 1
	}
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

if [ -n "${APK_TOOLS_SRC:-}" ]; then
	SRC="$APK_TOOLS_SRC"
	[ -d "$SRC" ] || {
		printf 'APK_TOOLS_SRC is not a directory: %s\n' "$SRC" >&2
		exit 1
	}
	if [ -d "$SRC/.git" ]; then
		actual="$(git -C "$SRC" rev-parse HEAD 2>/dev/null || true)"
		if [ "$actual" != "$APK_TOOLS_COMMIT" ]; then
			printf 'apk-tools checkout %s is at %s, expected %s\n' \
				"$SRC" "${actual:-<unknown>}" "$APK_TOOLS_COMMIT" >&2
			exit 1
		fi
	fi
else
	SRC="$WORK/apk-tools"
	git init -q "$SRC"
	git -C "$SRC" remote add origin "$APK_TOOLS_REPO"
	git -C "$SRC" fetch -q --depth 1 origin "$APK_TOOLS_COMMIT"
	git -C "$SRC" -c advice.detachedHead=false checkout -q FETCH_HEAD
fi

# get-version.sh resolves the release string from this environment variable, so
# the tool reports the same version as the OpenWrt package it stands in for.
VERSION="$APK_TOOLS_VERSION" meson setup "$WORK/build" "$SRC" \
	--buildtype=release \
	--prefix=/usr \
	-Dcrypto_backend=openssl \
	-Durl_backend=libfetch \
	-Ddefault_library=static \
	-Ddocs=disabled \
	-Dhelp=disabled \
	-Dlua=disabled \
	-Dpython=disabled \
	-Dtests=disabled \
	-Dzstd=disabled >&2
meson compile -C "$WORK/build" >&2

[ -x "$WORK/build/src/apk" ] || {
	printf 'apk-tools build produced no apk binary at %s\n' \
		"$WORK/build/src/apk" >&2
	exit 1
}
mkdir -p "$DEST"
cp "$WORK/build/src/apk" "$DEST/apk"
chmod 0755 "$DEST/apk"

version_output="$("$DEST/apk" --version 2>&1)" || {
	printf 'built apk binary failed to run: %s\n' "$version_output" >&2
	exit 1
}
case "$version_output" in
	*"apk-tools $APK_TOOLS_VERSION"*) ;;
	*)
		printf 'unexpected apk --version output: %s\n' "$version_output" >&2
		exit 1
		;;
esac

# The repository tooling needs the v3 generation applets, which -Dminimal=true
# would omit. An unknown applet makes apk print "is not an apk command" on
# stderr; a known applet only complains about its own missing arguments.
mkdir -p "$WORK/probe"
for applet in mkpkg mkndx adbsign; do
	# Run from a scratch directory so a probe can never leave a file behind.
	if ( cd "$WORK/probe" && "$DEST/apk" "$applet" ) 2>&1 |
		grep -q 'is not an apk command'; then
		printf 'apk-tools was built without the %s applet\n' "$applet" >&2
		exit 1
	fi
done

# Round-trip a throwaway package through mkpkg and adbdump. This catches a
# broken link against zlib/OpenSSL (mkpkg compresses the ADB with deflate) which
# an applet-existence check alone would not notice. No signing key is involved.
mkdir -p "$WORK/probe/pkgroot/usr/bin"
printf '#!/bin/sh\nexit 0\n' > "$WORK/probe/pkgroot/usr/bin/opensocks-probe"
chmod 0755 "$WORK/probe/pkgroot/usr/bin/opensocks-probe"
( cd "$WORK/probe" &&
	"$DEST/apk" mkpkg \
		--info name:opensocks-probe \
		--info version:0.0.1-r0 \
		--info arch:noarch \
		--info description:"build probe" \
		--files pkgroot \
		--output probe.apk ) >&2
if ! ( cd "$WORK/probe" && "$DEST/apk" adbdump probe.apk ) \
	> "$WORK/probe/probe.yaml" 2>&1; then
	printf 'built apk mkpkg/adbdump round trip failed\n' >&2
	cat "$WORK/probe/probe.yaml" >&2
	exit 1
fi
for expected in opensocks-probe 0.0.1-r0 noarch; do
	if ! grep -q "$expected" "$WORK/probe/probe.yaml"; then
		printf 'probe package is missing %s in its metadata\n' "$expected" >&2
		exit 1
	fi
done

printf '%s\n' "$version_output"
printf 'apk-tools installed to %s\n' "$DEST/apk"
