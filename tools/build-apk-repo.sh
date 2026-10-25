#!/bin/sh
# Turn an OpenSocks release directory into an OpenWrt 25.12 apk repository.
#
#   usage: tools/build-apk-repo.sh <out-dir> <version> <release> <apk-binary> <key-dir-or-empty>
#
# <out-dir>       release directory holding the already-built binaries
#                 (opensocks-linux-<suffix>.gz, produced by build-release.sh).
# <version>       PKG_VERSION, e.g. 0.2.13
# <release>       PKG_RELEASE, e.g. 1 (the apk version becomes <version>-r<release>)
# <apk-binary>    apk-tools binary built with mkpkg/mkndx/adbsign
#                 (see tools/build-apk-tool.sh)
# <key-dir>       directory with the EC P-256 key pair.
#                 Recognised names: private.pem, public.pem.
#
# A signing key is mandatory, not optional: mkpkg embeds a signature made with
# the private key, and mkndx refuses to index a package it cannot verify, so an
# unsigned apk repository cannot be represented in apk-tools 3.
#
# Environment:
#   APK_SIGNING_KEY_FILE   explicit private key path (default <key-dir>/private.pem)
#   APK_PUBLIC_KEY_FILE    explicit public key path  (default <key-dir>/public.pem)
#   REQUIRE_SIGNATURE=1    fail (rather than skip) when no key is configured
#   FEED_BASE              base URL for the download-on-boot binaries
#   OPENSOCKS_TARGETS      same syntax as build-release.sh
#   APK_SKIP_VERIFY=1      skip the apk self checks (debugging only)
#
# Facts this script depends on, each verified against apk-tools
# b5a31c0d86 (the commit OpenWrt 25.12 pins) and openwrt/openwrt openwrt-25.12:
#
#   doc/apk-mkpkg.8.scd  mkpkg takes --info/--files/--output/--script/--xattrs.
#                        There is no --name/--version/--arch/--output-name.
#   src/app_mkpkg.c      parse_info() rejects --info file-size and
#                        --info installed-size (goto inval). mkpkg computes
#                        installed-size itself by summing the staged files, so
#                        passing it is a hard error, not an improvement.
#   src/apk_adb.c        the --info keys are the ADB schema field names
#                        (name, version, description, arch, license, origin,
#                        url, maintainer, depends, provides, ...)
#   src/ctype.c          a dependency list is separated by SPACE and newline
#                        (APK_CTYPE_DEPENDENCY_SEPARATOR); ',' IS a valid
#                        dependency-name character. So a comma separated list
#                        is one long name and mkpkg rejects it. This is why
#                        every DEPENDS list below is space separated.
#   src/package.c        apk_dep_parse() has no OR syntax, and OpenWrt adds no
#                        patch for it, so "a | b" cannot be expressed in apk.
#   include/package-pack.mk
#                        Architecture: all in opkg is --info arch:noarch in apk.
#   src/app_mkndx.c      mkndx requires --output. It has no signing option of
#                        its own: the index is signed by whatever private key
#                        is in the apk trust context, loaded through the global
#                        GENERATION option --sign-key. mkndx also *verifies*
#                        each package it indexes and refuses an unsigned one
#                        ("UNTRUSTED signature"), so --sign-key must be given to
#                        mkpkg as well, where adb_c_adb writes the signature.
#   doc/apk-repositories.5.scd
#                        the repositories URL may point directly at the .adb
#                        index; package paths then resolve relative to it.
#
# Design notes worth keeping:
#
#   * No dependency may contain '|'. apk cannot say "opensocks or
#     opensocks-minimal". The LuCI package depends on opensocks-minimal, which
#     downloads the same daemon the full package installs, so installing only
#     the LuCI app still produces a working setup. See LUCIDEPENDS.
#   * apk v3 has no per-package conffiles list and apk-tools never reads
#     lib/apk/packages/*.conffiles (OpenWrt's package-pack.mk writes it, but the
#     tool ignores it). Config protection is client-side: apk defaults to
#     "+etc", "@etc/init.d", "!etc/apk" in protected_paths, and writes
#     .apk-new instead of clobbering edited files. We therefore do not emit a
#     fake conffiles file.
#   * mkpkg resolves uid/gid to *names* through the host passwd
#     (apk_id_cache_resolve_user/group), so the staging tree is forced to 0:0;
#     otherwise a CI runner's uid would leak into the package ACLs.
#   * Packages and the index go in one flat directory. mkndx records a
#     pkgname-spec so apk looks for ${name}-${version}-${arch}.apk next to
#     the index instead of <base>/<arch>/${name}-${version}.apk.
set -eu

USAGE='usage: build-apk-repo.sh <out-dir> <version> <release> <apk-binary> <key-dir-or-empty>'

OUT="${1:-}"
VERSION="${2:-}"
RELEASE="${3:-}"
APK_BIN="${4:-}"
KEY_DIR="${5:-}"

[ -n "$OUT" ] && [ -n "$VERSION" ] && [ -n "$RELEASE" ] && [ -n "$APK_BIN" ] || {
	printf '%s\n' "$USAGE" >&2
	exit 1
}

ROOT="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
FEED_BASE="${FEED_BASE:-https://rel.n4t.su/opkg}"
# OpenWrt's PKG_RELEASE maps to the apk -r<number> build component. Using
# <version>-r<release> keeps the two feeds describing the same build, and is a
# valid apk version (number{.number}{letter}{_suffix}{-r#}).
APK_VERSION="${VERSION}-r${RELEASE}"

[ -d "$OUT" ] || {
	printf 'release directory not found: %s\n' "$OUT" >&2
	exit 1
}
OUT="$(CDPATH= cd -- "$OUT" && pwd)"
[ -f "$APK_BIN" ] || {
	printf 'apk binary not found: %s\n' "$APK_BIN" >&2
	exit 1
}
APK_DIR="$(CDPATH= cd -- "$(dirname "$APK_BIN")" && pwd)"
APK_BIN="$APK_DIR/$(basename "$APK_BIN")"
[ -x "$APK_BIN" ] || {
	printf 'apk binary is not executable: %s\n' "$APK_BIN" >&2
	exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

# Resolve the signing key before anything is packaged. mkpkg signs each package
# with the private key in its trust context, and mkndx later refuses to index a
# package whose signature it cannot verify, so both need this up front; defining
# it after the packaging loop would leave every earlier package unsigned.
SIGNING_KEY="${APK_SIGNING_KEY_FILE:-}"
PUBLIC_KEY="${APK_PUBLIC_KEY_FILE:-}"
if [ -n "$KEY_DIR" ]; then
	[ -n "$SIGNING_KEY" ] || SIGNING_KEY="$KEY_DIR/private.pem"
	[ -n "$PUBLIC_KEY" ] || PUBLIC_KEY="$KEY_DIR/public.pem"
fi
if [ -n "$SIGNING_KEY" ] && [ ! -f "$SIGNING_KEY" ]; then
	printf 'apk signing key not found: %s\n' "$SIGNING_KEY" >&2
	exit 1
fi
if [ -n "$PUBLIC_KEY" ] && [ ! -f "$PUBLIC_KEY" ]; then
	printf 'apk public key not found: %s\n' "$PUBLIC_KEY" >&2
	exit 1
fi
if [ -n "$SIGNING_KEY" ] && [ -z "$PUBLIC_KEY" ]; then
	printf 'apk public key not set; use APK_PUBLIC_KEY_FILE or <key-dir>/public.pem\n' >&2
	exit 1
fi
if [ -z "$SIGNING_KEY" ] && [ "${REQUIRE_SIGNATURE:-0}" = "1" ]; then
	printf 'APK_SIGNING_KEY_FILE is required; refusing to build an unsigned apk repository.\n' >&2
	exit 1
fi

# The rest of the release tooling calls shasum (Perl); CI's ubuntu image has it,
# but a minimal container or a macOS/Windows dev box may only have sha256sum.
# Pick whichever exists so the script is not silently Linux-only.
if command -v shasum >/dev/null 2>&1; then
	sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
elif command -v sha256sum >/dev/null 2>&1; then
	sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v openssl >/dev/null 2>&1; then
	sha256_of() { openssl dgst -sha256 "$1" | awk '{print $NF}'; }
else
	printf 'no SHA-256 tool found (need shasum, sha256sum or openssl)\n' >&2
	exit 1
fi

# A -Dminimal=true build has no mkpkg/mkndx/adbsign, and apk reports that as an
# unknown command rather than failing early. Check once, up front.
for applet in mkpkg mkndx adbsign; do
	if ( cd "$WORK" && "$APK_BIN" "$applet" ) 2>&1 |
		grep -q 'is not an apk command'; then
		printf 'apk binary lacks the %s applet (built with -Dminimal=true?)\n' \
			"$applet" >&2
		exit 1
	fi
done

# Same target grammar as tools/build-release.sh:
#   GOARCH[:GOARM|:GOMIPS] binary_suffix opkg_arch [opkg_arch...];...
# The extra aliases (aarch64_cortex-a53_neon-vfpv4) only exist so vendor
# firmwares resolve the opkg package; they are not real apk architectures, so
# they are intentionally NOT emitted as .apk files. Mapped below.
TARGETS="${OPENSOCKS_TARGETS:-mipsle:softfloat mipsle mipsel_24kc;arm64 arm64 aarch64_cortex-a53 aarch64_cortex-a53_neon-vfpv4}"

printf '%s' "$TARGETS" | tr '\n' ';' | sed 's/;;*/;/g; s/^;//; s/;$//' |
	tr ';' '\n' > "$WORK/targets.txt"

# Map an opkg architecture string onto the apk architecture string that
# OpenWrt 25.12 uses in its feeds. Vendor suffixes such as _neon-vfpv4 are
# dropped: they are opkg-only aliases for the same CPU.
apk_arch_for() {
	case "$1" in
		mipsel_24kc|mipsel_24kc_24kf) printf '%s\n' mipsel_24kc ;;
		aarch64_cortex-a53|aarch64_cortex-a53_neon-vfpv4) printf '%s\n' aarch64_cortex-a53 ;;
		all) printf '%s\n' noarch ;;
		*) printf '%s\n' "$1" ;;
	esac
}

# ---------------------------------------------------------------------------
# Staging helpers
# ---------------------------------------------------------------------------

# Normalise a staging tree so the package metadata is host independent.
normalise_stage() {
	stage="$1"
	[ -d "$stage" ] || return 0
	find "$stage" -type d -exec chmod 0755 {} +
	find "$stage" -type f ! -perm -u+x -exec chmod 0644 {} +
	find "$stage" -type f -perm -u+x -exec chmod 0755 {} +
	# A state dir is not shipped, but keep the mode right if one ever is.
	if [ -d "$stage/etc/opensocks" ]; then
		chmod 0700 "$stage/etc/opensocks"
	fi
	# Only root can chown; a non-root local run keeps its own uid, which is
	# fine because the names then resolve to that user deliberately.
	if [ "$(id -u)" = "0" ]; then
		chown -R 0:0 "$stage"
	fi
}

install_file() {
	src="$1"
	dst="$2"
	mode="$3"
	mkdir -p "$(dirname "$dst")"
	cp "$src" "$dst"
	chmod "$mode" "$dst"
}

# Shared file set for the daemon package, byte-identical to the .ipk staging in
# tools/build-release.sh so both feeds describe the same install.
#
# Two directories are intentionally NOT shipped:
#   /etc/opensocks  the persistent state dir (credentials.enc, session,
#                   history.json, device_id, mobile-token, traffic.json, ...).
#                   The init script creates it with mode 0700 on every start, so
#                   pre-creating it here would only bake an empty dir into the
#                   image and fight the init script's chmod.
#   /tmp/opensocks  tmpfs scratch (OPENSOCKS_DIR), created by the daemon.
# The engine config files once shipped under /etc/opensocks (engine.yaml,
# config.yaml) are gone: the Shadowsocks engine is in-process now.
stage_daemon_files() {
	stage="$1"
	suffix="$2"
	install_file "$ROOT/openwrt/opensocks/files/etc/init.d/opensocks" \
		"$stage/etc/init.d/opensocks" 0755
	install_file "$ROOT/openwrt/opensocks/files/etc/config/opensocks" \
		"$stage/etc/config/opensocks" 0644
	mkdir -p "$stage/usr/lib/opensocks"
	cp "$OUT/opensocks-linux-$suffix.gz" "$stage/usr/lib/opensocks/opensocks.gz"
	chmod 0644 "$stage/usr/lib/opensocks/opensocks.gz"
}

# apk runs package scripts with a sanitised environment (PATH plus APK_PACKAGE
# and APK_SCRIPT), chrooted into --root when one is given. There is no
# IPKG_INSTROOT equivalent. The original guards existed to avoid touching a
# foreign rootfs, so the replacement is a probe for a booted OpenWrt userspace:
# /etc/rc.common only exists inside a real OpenWrt root. `apk --root` image
# builds pass --no-scripts and never reach these scripts at all.
write_daemon_scripts() {
	dir="$1"
	cat > "$dir/post-install" <<'EOF'
#!/bin/sh
if [ -x /etc/rc.common ]; then
	/etc/init.d/opensocks enable >/dev/null 2>&1 || true
	( sleep 2; /etc/init.d/opensocks start >/dev/null 2>&1 || true ) &
fi
exit 0
EOF
	cat > "$dir/pre-deinstall" <<'EOF'
#!/bin/sh
if [ -x /etc/rc.common ]; then
	/etc/init.d/opensocks stop >/dev/null 2>&1 || true
	/etc/init.d/opensocks disable >/dev/null 2>&1 || true
fi
exit 0
EOF
	chmod 0755 "$dir/post-install" "$dir/pre-deinstall"
}

# Minimal launcher post-install: pick the CPU suffix, then pin the matching
# binary URL and SHA-256 into UCI. Mirrors tools/build-release.sh.
write_minimal_postinstall() {
	dir="$1"
	arch_case="$2"
	cat > "$dir/post-install" <<EOF
#!/bin/sh
[ -x /etc/rc.common ] || exit 0

detect_suffix() {
	case "\$(uname -m)" in
		mipsel) printf '%s\n' mipsle ;;
		mips)
			elf_data="\$(od -An -j 5 -N 1 -tu1 /bin/busybox 2>/dev/null | tr -d ' ')"
			case "\$elf_data" in
				1) printf '%s\n' mipsle ;;
				2) printf '%s\n' mips ;;
				*) return 1 ;;
			esac
			;;
		mips64el) printf '%s\n' mips64le ;;
		mips64) printf '%s\n' mips64 ;;
		aarch64|arm64) printf '%s\n' arm64 ;;
		armv7l|arm) printf '%s\n' arm ;;
		armv6l) printf '%s\n' arm6 ;;
		x86_64|amd64) printf '%s\n' amd64 ;;
		i386|i486|i586|i686) printf '%s\n' 386 ;;
		*) return 1 ;;
	esac
}

_suffix="\$(detect_suffix 2>/dev/null)" || {
	echo "OpenSocks does not support CPU architecture: \$(uname -m)" >&2
	exit 0
}
_sha=''
_url=''
case "\${_suffix}" in${arch_case}
esac
[ -n "\${_sha}" ] && [ -n "\${_url}" ] || {
	echo "This OpenSocks release has no binary for CPU architecture: \$(uname -m)" >&2
	exit 0
}

uci set opensocks.settings.binary_url="\${_url}"
uci set opensocks.settings.binary_sha256="\${_sha}"
uci commit opensocks
/etc/init.d/opensocks enable >/dev/null 2>&1 || true
( sleep 2; /etc/init.d/opensocks start >/dev/null 2>&1 || true ) &
exit 0
EOF
	chmod 0755 "$dir/post-install"
}

# build_apk <output> <name> <arch> <origin> <description> <depends> \
#           <stage> [--script TYPE:FILE ...]
# Dependency lists are SPACE separated. apk's ADB dependency parser
# (src/sch_apk_adb.c dependencies_fromstring) splits on
# APK_CTYPE_DEPENDENCY_SEPARATOR, which ctype.c defines as space and newline
# only; a comma is an allowed *name* character. A comma-separated list is
# therefore read as one enormous package name, and mkpkg rejects it with
# "info field 'depends' has invalid value". OpenWrt's include/package-pack.mk
# converts its own comma lists to spaces for exactly this reason.
build_apk() {
	out="$1"
	name="$2"
	arch="$3"
	origin="$4"
	description="$5"
	depends="$6"
	stage="$7"
	shift 7
	rm -f "$out"

	# --sign-key is a GENERATION option that loads a private key into the trust
	# context, and mkpkg signs the ADB with whatever private keys it finds
	# (adb_c_adb -> adb_trust_write_signatures). It must be passed HERE, not to
	# mkndx: mkndx only *verifies* the packages it indexes, so it needs the
	# packages to already carry a signature it can check.
	#
	# The caller's remaining arguments (--script TYPE:FILE) stay in "$@" and are
	# appended after the generated ones.
	"$APK_BIN" mkpkg \
		${SIGNING_KEY:+--sign-key="$SIGNING_KEY"} \
		--info "name:$name" \
		--info "version:$APK_VERSION" \
		--info "arch:$arch" \
		--info "license:GPL-3.0-or-later" \
		--info "origin:$origin" \
		--info "url:https://github.com/NarYuki/OpenSocks" \
		--info "maintainer:OpenSocks Developers" \
		--info "description:$description" \
		--info "depends:$depends" \
		${PROVIDES:+--info "provides:$PROVIDES"} \
		"$@" \
		--files "$stage" \
		--output "$out" \
		--xattrs=no
	[ -s "$out" ] || {
		printf 'apk mkpkg produced no package: %s\n' "$out" >&2
		exit 1
	}
}

# ---------------------------------------------------------------------------
# Daemon package
# ---------------------------------------------------------------------------

# The daemon embeds the Shadowsocks engine (TCP REDIRECT + UDP TPROXY +
# SOCKS5), so shadowsocks-libev-ss-redir, shadowsocks-libev-ss-local, mihomo,
# sing-box and redsocks are all gone from the dependency set.
# kmod-nft-tproxy is still required for UDP TPROXY. It lives in the per-target
# kmods feed rather than the generic arch feed, so a client that has not
# registered its target's kmods feed cannot resolve it. That is normal for
# OpenWrt packages that need a kernel module.
DAEMON_DEPENDS='libc nftables-json kmod-nft-tproxy ca-bundle uci'

# apk has no OR dependencies, so the "opensocks | opensocks-minimal" that the
# opkg LuCI package used has to be modelled with a virtual name. A *versioned*
# provide makes apk treat the two packages as owning one name, which gives the
# semantics we want: either one satisfies a dependency on the virtual name, and
# they conflict so only one is ever installed. provider-priority is not needed
# here because nothing installs the virtual name implicitly; the LuCI package
# depends on it explicitly.
#
# The value is a full dependency expression ("name=version"), which is what the
# ADB provides field wants as well: apk parses both depends and provides with
# apk_dep_parse, so an unversioned name would still work but would not conflict
# with the other implementation.
ENGINE_PROVIDE='opensocks-engine='"$VERSION"

: > "$WORK/suffixes"
arch_case=""
while IFS= read -r target; do
	[ -n "$target" ] || continue
	set -- $target
	go_spec="$1"
	suffix="$2"
	shift 2
	[ "$#" -ge 1 ] || {
		printf 'invalid OPENSOCKS_TARGETS entry: %s\n' "$target" >&2
		exit 1
	}
	gz="opensocks-linux-$suffix.gz"
	if [ ! -s "$OUT/$gz" ]; then
		printf 'missing release binary for suffix %s: %s/%s\n' \
			"$suffix" "$OUT" "$gz" >&2
		exit 1
	fi
	if ! grep -qx "$suffix" "$WORK/suffixes"; then
		printf '%s\n' "$suffix" >> "$WORK/suffixes"
		sha="$(sha256_of "$OUT/$gz")"
		arch_case="${arch_case}
	${suffix})
		_sha='${sha}'
		_url='${FEED_BASE}/by-sha/${sha}/${gz}'
		;;"
		sha_file="$WORK/sha-$suffix"
		printf '%s\n' "$sha" > "$sha_file"
	fi
	sha="$(cat "$WORK/sha-$suffix")"

	for opkg_arch in "$@"; do
		arch="$(apk_arch_for "$opkg_arch")"
		# Vendor aliases collapse onto the stock arch; build each apk arch once.
		[ -f "$OUT/opensocks-$APK_VERSION-$arch.apk" ] && continue
		stage="$WORK/stage-daemon-$arch"
		scripts="$WORK/scripts-daemon-$arch"
		rm -rf "$stage" "$scripts"
		mkdir -p "$stage" "$scripts"
		stage_daemon_files "$stage" "$suffix"
		normalise_stage "$stage"
		write_daemon_scripts "$scripts"
		PROVIDES="$ENGINE_PROVIDE"
		build_apk "$OUT/opensocks-$APK_VERSION-$arch.apk" \
			opensocks "$arch" opensocks \
			'Low-memory Transocks daemon with TCP REDIRECT and UDP TPROXY' \
			"$DAEMON_DEPENDS" "$stage" \
			--script "post-install:$scripts/post-install" \
			--script "pre-deinstall:$scripts/pre-deinstall" || exit 1
		PROVIDES=""
		printf 'built opensocks-%s-%s.apk\n' "$APK_VERSION" "$arch"
	done
done < "$WORK/targets.txt"

[ -s "$WORK/suffixes" ] || {
	printf 'no usable OPENSOCKS_TARGETS entries\n' >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Minimal launcher (noarch)
# ---------------------------------------------------------------------------

# Keeps the download tools the .ipk used (wget-ssl provides wget, usign verifies
# the signed index); drops the proxy engines that no longer exist in 25.12.
MINIMAL_DEPENDS='libc nftables-json kmod-nft-tproxy ca-bundle uci wget-ssl usign'

default_suffix="mipsle"
if ! grep -qx "$default_suffix" "$WORK/suffixes"; then
	default_suffix="$(head -n 1 "$WORK/suffixes")"
fi
default_gz="opensocks-linux-$default_suffix.gz"

minimal_stage="$WORK/stage-minimal"
minimal_scripts="$WORK/scripts-minimal"
rm -rf "$minimal_stage" "$minimal_scripts"
mkdir -p "$minimal_stage" "$minimal_scripts"
install_file "$ROOT/openwrt/opensocks-minimal/files/etc/init.d/opensocks" \
	"$minimal_stage/etc/init.d/opensocks" 0755
install_file "$ROOT/openwrt/opensocks-minimal/files/usr/lib/opensocks/launcher.sh" \
	"$minimal_stage/usr/lib/opensocks/launcher.sh" 0755
# The UCI defaults carry @BINARY_SHA256@/@BINARY_NAME@ placeholders.
mkdir -p "$minimal_stage/etc/config"
default_sha="$(cat "$WORK/sha-$default_suffix")"
sed \
	-e "s|@BINARY_SHA256@|${default_sha}|g" \
	-e "s|@BINARY_NAME@|${default_gz}|g" \
	"$ROOT/openwrt/opensocks-minimal/files/etc/config/opensocks" \
	> "$minimal_stage/etc/config/opensocks"
chmod 0644 "$minimal_stage/etc/config/opensocks"
normalise_stage "$minimal_stage"
write_minimal_postinstall "$minimal_scripts" "$arch_case"
# opkg "Architecture: all" becomes apk "arch:noarch"; 25.12 clients treat noarch
# as installable everywhere.
PROVIDES="$ENGINE_PROVIDE"
build_apk "$OUT/opensocks-minimal-$APK_VERSION-noarch.apk" \
	opensocks-minimal noarch opensocks-minimal \
	'Minimal verified download-on-boot launcher for OpenSocks' \
	"$MINIMAL_DEPENDS" "$minimal_stage" \
	--script "post-install:$minimal_scripts/post-install"
PROVIDES=""
printf 'built opensocks-minimal-%s-noarch.apk\n' "$APK_VERSION"

# ---------------------------------------------------------------------------
# LuCI package (noarch)
# ---------------------------------------------------------------------------

# What the shipped app actually needs, verified against the 25.12 feeds:
#
#   luci-base        the LuCI core the admin pages run on.
#   luci-compat +    the controller is still Lua MVC (luasrc/controller/*.lua),
#   luci-lua-runtime so it needs the Lua dispatcher and the CBI/view shims.
#                    luci-compat depends on luci-lua-runtime, but both are named
#                    explicitly so a future luci-compat dependency change cannot
#                    silently drop the runtime.
#   curl             the controller proxies to the daemon by shelling out to
#                    /usr/bin/curl (luasrc/controller/opensocks.lua), so curl is
#                    a runtime dependency, not a build convenience.
#
# All four are present in the 25.12 feed for every architecture.
#
# apk cannot express "opensocks | opensocks-minimal" ('|' is not a dependency
# character and apk_dep_parse has no OR syntax). Both engine packages instead
# provide the versioned virtual name stored in ENGINE_PROVIDE, which the app
# depends on: either implementation satisfies it, and because the provide is
# versioned apk treats them as owners of one name, so they conflict and exactly
# one gets installed.
LUCIDEPENDS="libc $ENGINE_PROVIDE luci-base luci-compat luci-lua-runtime curl"

luci_stage="$WORK/stage-luci"
rm -rf "$luci_stage"
mkdir -p "$luci_stage"

# ucode application: /usr/share/ucode/luci is where LuCI 25.12 loads ucode apps
# from. root/ is copied verbatim (rpcd ACLs, menu.d, uci-defaults).
if [ -d "$ROOT/openwrt/luci-app-opensocks/ucode" ]; then
	mkdir -p "$luci_stage/usr/share/ucode/luci"
	cp -R "$ROOT/openwrt/luci-app-opensocks/ucode/." \
		"$luci_stage/usr/share/ucode/luci/"
fi
if [ -d "$ROOT/openwrt/luci-app-opensocks/root" ]; then
	cp -R "$ROOT/openwrt/luci-app-opensocks/root/." "$luci_stage/"
fi

# Lua MVC sources are tolerated while the port is in flight: shipped when
# present, skipped when luasrc/ is deleted.
if [ -d "$ROOT/openwrt/luci-app-opensocks/luasrc" ]; then
	if [ -f "$ROOT/openwrt/luci-app-opensocks/luasrc/controller/opensocks.lua" ]; then
		install_file \
			"$ROOT/openwrt/luci-app-opensocks/luasrc/controller/opensocks.lua" \
			"$luci_stage/usr/lib/lua/luci/controller/opensocks.lua" 0644
	fi
	if [ -f "$ROOT/openwrt/luci-app-opensocks/luasrc/view/opensocks/status.htm" ]; then
		install_file \
			"$ROOT/openwrt/luci-app-opensocks/luasrc/view/opensocks/status.htm" \
			"$luci_stage/usr/lib/lua/luci/view/opensocks/status.htm" 0644
	fi
fi

normalise_stage "$luci_stage"
if [ -z "$(find "$luci_stage" -type f -print -quit)" ]; then
	printf 'luci-app-opensocks stages no files; refusing to package it\n' >&2
	exit 1
fi
build_apk "$OUT/luci-app-opensocks-$APK_VERSION-noarch.apk" \
	luci-app-opensocks noarch luci-app-opensocks \
	'LuCI interface for OpenSocks' \
	"$LUCIDEPENDS" "$luci_stage"
printf 'built luci-app-opensocks-%s-noarch.apk\n' "$APK_VERSION"

# ---------------------------------------------------------------------------
# Repository index
# ---------------------------------------------------------------------------

# SIGNING_KEY/PUBLIC_KEY were resolved at the top; mkpkg already used the private
# key, so the packages carry signatures mkndx can verify.

# Every .apk lives in one flat directory, so the index must tell apk how to map
# a package onto its filename. --pkgname-spec is stored in the index and
# overrides the default <base>/<arch>/<name>-<version>.apk layout.
#
# The spec is expanded through adb_s_field_subst (src/adb.c), which resolves
# each ${var} against the *package's own* ADB fields. ${arch} therefore
# resolves per package, so the spec must spell out the architecture: otherwise
# apk requests opensocks-0.3.0-r1.apk while the file on disk is
# opensocks-0.3.0-r1-mipsel_24kc.apk. mkndx warns when a spec does not
# reproduce its input filename, which is how this was caught.
PKGNAME_SPEC='${name}-${version}-${arch}.apk'

set -- "$OUT"/*.apk
[ "$#" -ge 1 ] && [ -e "$1" ] || {
	printf 'no .apk files were produced in %s\n' "$OUT" >&2
	exit 1
}

mkdir -p "$OUT/repo"
# Move the packages into repo/ so the index and the packages share a directory
# and the pkgname-spec resolves relative to packages.adb.
for apk in "$@"; do
	mv "$apk" "$OUT/repo/"
done
set -- "$OUT"/repo/*.apk

# mkndx re-reads every package through apk_extract and refuses one whose
# signature it cannot verify ("UNTRUSTED signature"), so two things must line up:
#
#   * mkpkg must have signed each package (it does; SIGNING_KEY is passed above).
#   * mkndx must TRUST the matching public key. --sign-key loads the private key
#     into trust->private_key_list (for creating signatures); verification
#     instead walks trust->trusted_key_list, which is populated ONLY from
#     --keys-dir (or the default etc/apk/keys and lib/apk/keys directories,
#     see apk_ctx_get_trust in src/context.c).
#
# So mkndx gets --keys-dir pointing at a directory holding the PUBLIC half.
keys_dir="$WORK/trusted"
mkdir -p "$keys_dir"
cp "$PUBLIC_KEY" "$keys_dir/opensocks.pem"

run_mkndx() {
	# mkndx writes diagnostics to stderr; keep them for the failure path and for
	# the pkgname-spec check below, and echo them when it fails so the log says
	# why instead of only "exit code 1".
	if "$APK_BIN" \
		--keys-dir "$keys_dir" \
		${SIGNING_KEY:+--sign-key="$SIGNING_KEY"} \
		mkndx \
		--output "$OUT/repo/packages.adb" \
		--description "OpenSocks $VERSION" \
		--pkgname-spec "$PKGNAME_SPEC" \
		"$@" 2> "$WORK/mkndx.log"; then
		return 0
	fi
	printf 'apk mkndx failed; it said:\n' >&2
	[ -s "$WORK/mkndx.log" ] && cat "$WORK/mkndx.log" >&2
	return 1
}

# A key is mandatory (checked at the top): mkpkg signs each package with it and
# mkndx verifies that signature while indexing. There is no unsigned feed.
run_mkndx "$@"
cp "$PUBLIC_KEY" "$OUT/repo/opensocks.pem"
# The v2-style fingerprint name is kept as an extra alias: opkg's usign key is
# named by fingerprint, so an operator copying keys between the two feeds finds a
# matching name. apk itself matches the key by identity, not name.
printf 'apk index signed; public key published as opensocks.pem\n'
[ -s "$OUT/repo/packages.adb" ] || {
	printf 'apk mkndx produced no index\n' >&2
	# mkndx's stderr went to the log file; surface it, otherwise the failure is
	# just an exit status with no explanation.
	if [ -s "$WORK/mkndx.log" ]; then
		printf 'mkndx said:\n' >&2
		cat "$WORK/mkndx.log" >&2
	fi
	exit 1
}
gzip -9c "$OUT/repo/packages.adb" > "$OUT/repo/packages.adb.gz"

# mkndx only *warns* when --pkgname-spec does not reproduce an input filename
# (src/app_mkndx.c: "not matching package name specification"). A warning here
# means every client would 404 while resolving that package, so turn it into a
# hard failure. The warning text is matched loosely so an apk wording change
# fails open rather than breaking the build; the exit status of mkndx is checked
# separately and is what enforces success.
if grep -qi 'not matching package name specification' "$WORK/mkndx.log"; then
	printf 'apk mkndx reported a package name spec mismatch:\n' >&2
	grep -i 'not matching package name specification' "$WORK/mkndx.log" >&2
	printf 'check PKGNAME_SPEC against the generated filenames.\n' >&2
	exit 1
fi

# ---------------------------------------------------------------------------
# Self verification
# ---------------------------------------------------------------------------

if [ "${APK_SKIP_VERIFY:-0}" = "1" ]; then
	printf 'apk self verification skipped (APK_SKIP_VERIFY=1)\n' >&2
else
	# 1. Integrity and signature of the index and of every package.
	"$APK_BIN" verify "$OUT/repo/packages.adb"
	for apk in "$OUT"/repo/*.apk; do
		"$APK_BIN" verify "$apk"
	done

	# 2. Install smoke test in a throwaway root. --arch is mandatory; without
	#    it apk falls back to the build host's architecture and every package
	#    looks uninstallable. kmod-nft-tproxy is stubbed because this root has
	#    no target kmods feed; the point is to prove our own metadata, file
	#    layout and signature work.
	real_arch=""
	while IFS= read -r target; do
		[ -n "$target" ] || continue
		set -- $target
		[ "$#" -ge 3 ] || continue
		shift 2
		real_arch="$(apk_arch_for "$1")"
		break
	done < "$WORK/targets.txt"
	[ -n "$real_arch" ] || real_arch="$default_suffix"

	smoke_stub="$WORK/stub"
	rm -rf "$smoke_stub"
	mkdir -p "$smoke_stub/lib/modules"
	: > "$smoke_stub/lib/modules/.keep"
	normalise_stage "$smoke_stub"

	run_smoke() {
		smoke_arch="$1"
		root="$WORK/root-$smoke_arch"
		rm -rf "$root"
		mkdir -p "$root/etc/apk/keys" "$root/var/cache/apk"
		if [ -n "$PUBLIC_KEY" ]; then
			cp "$PUBLIC_KEY" "$root/etc/apk/keys/opensocks.pem"
		fi
		# The registered URL is the FULL PATH to packages.adb, which is what
		# 25.12 expects in /etc/apk/repositories.d/*.list.
		#
		# This file must live OUTSIDE the root: apk opens --repositories-file
		# relative to the process cwd (add_repos_from_file with AT_FDCWD),
		# whereas --keys-dir and --cache-dir are resolved relative to --root.
		# Pointing at $root/etc/apk/... would silently find nothing.
		printf 'file://%s/repo/packages.adb\n' "$OUT" \
			> "$WORK/repositories-$smoke_arch.list"

		stub_apk="$WORK/kmod-nft-tproxy-$smoke_arch.apk"
		rm -f "$stub_apk"
		"$APK_BIN" mkpkg \
			--info name:kmod-nft-tproxy \
			--info version:0 \
			--info arch:"$smoke_arch" \
			--info description:"smoke test stub for the kernel module" \
			--files "$smoke_stub" \
			--output "$stub_apk" \
			--xattrs=no

		# --allow-untrusted covers the stub package, which is not in any signed
		# index. The real packages are still checked against the index hash.
		"$APK_BIN" --root "$root" \
			--arch "$smoke_arch" --arch noarch \
			--keys-dir /etc/apk/keys \
			--repositories-file "$WORK/repositories-$smoke_arch.list" \
			--cache-dir /var/cache/apk \
			add --initdb --no-scripts --allow-untrusted "$stub_apk" \
			opensocks opensocks-minimal luci-app-opensocks >/dev/null

		for pkg in opensocks opensocks-minimal luci-app-opensocks; do
			"$APK_BIN" --root "$root" --arch "$smoke_arch" --arch noarch \
				--keys-dir /etc/apk/keys info -e "$pkg" >/dev/null || {
				printf 'smoke test failed: %s missing after install for %s\n' \
					"$pkg" "$smoke_arch" >&2
				exit 1
			}
		done
		[ -f "$root/etc/init.d/opensocks" ] || {
			printf 'smoke test failed: init script missing for %s\n' "$smoke_arch" >&2
			exit 1
		}
		[ -f "$root/usr/lib/opensocks/opensocks.gz" ] || {
			printf 'smoke test failed: daemon payload missing for %s\n' "$smoke_arch" >&2
			exit 1
		}
		printf 'smoke test passed for %s\n' "$smoke_arch"
	}

	run_smoke noarch
	run_smoke "$real_arch"
fi

printf 'apk repository written to %s\n' "$OUT/repo"
if [ -n "$SIGNING_KEY" ]; then
	printf '  packages.adb (signed), *.apk, opensocks.pem\n'
	printf '  client: wget -O /etc/apk/keys/opensocks.pem <feed>/opensocks.pem\n'
	printf '  client: echo <feed>/packages.adb >> /etc/apk/repositories.d/customfeeds.list\n'
else
	printf '  packages.adb (UNSIGNED) - do not distribute without a signing key\n'
fi
