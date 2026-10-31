#!/bin/sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
VERSION="${VERSION:-0.3.2}"
RELEASE="${RELEASE:-1}"
OUT="${RELEASE_DIR:-$ROOT/../release/$VERSION}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM
FEED_BASE="${FEED_BASE:-https://rel.n4t.su/opkg}"
# Where the download-on-boot daemon archives live.
#
#   flat   <BINARY_BASE>/opensocks-linux-<suffix>.gz
#          A GitHub Release serves exactly this layout.
#   by-sha <BINARY_BASE>/by-sha/<sha256>/opensocks-linux-<suffix>.gz
#          Content addressed, so a CDN may cache it forever.
#
# The default is flat because that is what a release download looks like, and
# the minimal package's launcher verifies the archive against the SHA-256 baked
# into UCI either way. Set OPENSOCKS_BINARY_LAYOUT=by-sha for the mirror.
BINARY_BASE="${BINARY_BASE:-$FEED_BASE}"
BINARY_LAYOUT="${OPENSOCKS_BINARY_LAYOUT:-flat}"

binary_url() {
	sha="$1"
	gz="$2"
	case "$BINARY_LAYOUT" in
	by-sha) printf '%s/by-sha/%s/%s\n' "$BINARY_BASE" "$sha" "$gz" ;;
	*) printf '%s/%s\n' "$BINARY_BASE" "$gz" ;;
	esac
}

mkdir -p "$OUT"
rm -f "$OUT"/*

build_ipk() {
	name="$1"
	arch="$2"
	control_dir="$3"
	data_dir="$4"
	package="$OUT/${name}_${VERSION}-${RELEASE}_${arch}.ipk"
	find "$data_dir" -type d -exec chmod 0755 {} +
	if [ -d "$data_dir/etc/opensocks" ]; then
		chmod 0700 "$data_dir/etc/opensocks"
	fi
	( cd "$control_dir" && tar --owner=0 --group=0 -czf "$WORK/control.tar.gz" . )
	( cd "$data_dir" && tar --owner=0 --group=0 -czf "$WORK/data.tar.gz" . )
	printf '2.0\n' > "$WORK/debian-binary"
	# OpenWrt 22.03 opkg uses the legacy gzip-compressed tar IPK container.
	( cd "$WORK" && tar -czf "$package" debian-binary control.tar.gz data.tar.gz )
	rm -f "$WORK/control.tar.gz" "$WORK/data.tar.gz" "$WORK/debian-binary"
}

build_go_binary() {
	go_spec="$1"
	output="$2"
	goarch="${go_spec%%:*}"
	goextra=""
	case "$go_spec" in
		*:*) goextra="${go_spec#*:}" ;;
	esac
	(
		cd "$ROOT/openwrt/opensocks/src"
		extra_env=""
		case "$goarch" in
			mipsle|mips)
				extra_env="GOMIPS=${goextra:-softfloat}"
				;;
			arm)
				extra_env="GOARM=${goextra:-7}"
				;;
		esac
		# shellcheck disable=SC2086
		env CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" $extra_env \
			go build -trimpath -ldflags='-s -w' -o "$output" .
	)
}

# GOARCH[:GOARM|:GOMIPS] binary_suffix ipk_arch [ipk_arch...]
# Multiple targets are separated by ';' .
# One Go binary can be packaged under multiple opkg Architecture names so
# vendor firmwares (for example GL.iNet aarch64_cortex-a53_neon-vfpv4) match.
TARGETS="${OPENSOCKS_TARGETS:-mipsle:softfloat mipsle mipsel_24kc;arm64 arm64 aarch64_cortex-a53 aarch64_cortex-a53_neon-vfpv4}"

TARGETS_NORMALIZED="$(printf '%s' "$TARGETS" | tr '\n' ';' | sed 's/;;*/;/g; s/^;//; s/;$//')"
# The trailing newline is load-bearing: "while IFS= read -r target" silently
# DROPS a final line that has no newline, which would drop the last target
# (arm64) and build only the first architecture.
printf '%s\n' "$TARGETS_NORMALIZED" | tr ';' '\n' > "$WORK/targets.txt"
if [ -n "$(tail -c 1 "$WORK/targets.txt")" ]; then
	printf '\n' >> "$WORK/targets.txt"
fi

: > "$WORK/suffixes"
while IFS= read -r target; do
	[ -n "$target" ] || continue
	set -- $target
	go_spec="$1"
	binary_suffix="$2"
	shift 2
	[ "$#" -ge 1 ] || {
		printf 'Invalid OPENSOCKS_TARGETS entry (need go_spec suffix ipk_arch...): %s\n' "$target" >&2
		exit 1
	}

	bin_path="$WORK/opensocks-$binary_suffix"
	gz_name="opensocks-linux-${binary_suffix}.gz"
	if [ ! -s "$bin_path" ]; then
		build_go_binary "$go_spec" "$bin_path"
		gzip -9c "$bin_path" > "$OUT/$gz_name"
		sha="$(shasum -a 256 "$OUT/$gz_name" | awk '{print $1}')"
		printf '%s\n' "$sha" > "$WORK/sha-$binary_suffix"
		printf '%s\n' "$binary_suffix" >> "$WORK/suffixes"
	fi

	for ipk_arch in "$@"; do
		daemon_control="$WORK/daemon-control-$ipk_arch"
		daemon_data="$WORK/daemon-data-$ipk_arch"
		rm -rf "$daemon_control" "$daemon_data"
		mkdir -p "$daemon_control" "$daemon_data/etc/init.d" "$daemon_data/etc/config" \
			"$daemon_data/etc/opensocks" "$daemon_data/usr/lib/opensocks"
		cat > "$daemon_control/control" <<EOF
Package: opensocks
Version: ${VERSION}-${RELEASE}
Depends: libc, nftables-json, kmod-nft-tproxy, ca-bundle, uci
Source: opensocks
Section: net
Architecture: ${ipk_arch}
Maintainer: OpenSocks Developers
Description: Low-memory China routing daemon with TCP REDIRECT and UDP TPROXY
EOF
		printf '/etc/config/opensocks\n' > "$daemon_control/conffiles"
		cat > "$daemon_control/postinst" <<'EOF'
#!/bin/sh
if [ -z "${IPKG_INSTROOT:-}" ]; then
	/etc/init.d/opensocks enable
	( sleep 2; /etc/init.d/opensocks start >/dev/null 2>&1 || true ) &
fi
exit 0
EOF
		cat > "$daemon_control/prerm" <<'EOF'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] || /etc/init.d/opensocks stop
exit 0
EOF
		chmod 0755 "$daemon_control/postinst" "$daemon_control/prerm"
		cp "$ROOT/openwrt/opensocks/files/etc/init.d/opensocks" "$daemon_data/etc/init.d/opensocks"
		cp "$ROOT/openwrt/opensocks/files/etc/config/opensocks" "$daemon_data/etc/config/opensocks"
		chmod 0755 "$daemon_data/etc/init.d/opensocks"
		cp "$OUT/$gz_name" "$daemon_data/usr/lib/opensocks/opensocks.gz"
		build_ipk opensocks "$ipk_arch" "$daemon_control" "$daemon_data"
	done
done < "$WORK/targets.txt"

# Default binary metadata remains mipsle for backward-compatible field names.
default_suffix="mipsle"
if [ ! -f "$WORK/sha-$default_suffix" ]; then
	default_suffix="$(head -n 1 "$WORK/suffixes")"
fi
default_sha="$(cat "$WORK/sha-$default_suffix")"
default_gz="opensocks-linux-${default_suffix}.gz"
default_url="${BINARY_URL:-$(binary_url "$default_sha" "$default_gz")}"

arch_control_extra=""
arch_postinst_case=""
while IFS= read -r suffix; do
	[ -n "$suffix" ] || continue
	sha="$(cat "$WORK/sha-$suffix")"
	gz="opensocks-linux-${suffix}.gz"
	url="$(binary_url "$sha" "$gz")"
	arch_control_extra="${arch_control_extra}X-OpenSocks-Binary-SHA256-${suffix}: ${sha}
X-OpenSocks-Binary-URL-${suffix}: ${url}
"
	arch_postinst_case="${arch_postinst_case}
	${suffix})
		_sha='${sha}'
		_url='${url}'
		;;"
done < "$WORK/suffixes"

minimal_control="$WORK/minimal-control"
minimal_data="$WORK/minimal-data"
mkdir -p "$minimal_control" "$minimal_data/etc/init.d" "$minimal_data/etc/config" \
	"$minimal_data/etc/opensocks" "$minimal_data/usr/lib/opensocks"
cat > "$minimal_control/control" <<EOF
Package: opensocks-minimal
Version: ${VERSION}-${RELEASE}
Depends: libc, nftables-json, kmod-nft-tproxy, ca-bundle, uci, wget-ssl, usign
X-OpenSocks-Binary-SHA256: $default_sha
X-OpenSocks-Binary-URL: $default_url
${arch_control_extra}Source: opensocks-minimal
Section: net
Architecture: all
Maintainer: OpenSocks Developers
Description: Minimal verified download-on-boot launcher for OpenSocks
EOF
printf '/etc/config/opensocks\n' > "$minimal_control/conffiles"
cat > "$minimal_control/postinst" <<EOF
#!/bin/sh
[ -n "\${IPKG_INSTROOT:-}" ] && exit 0

detect_suffix() {
	case "\$(uname -m)" in
		mipsel) printf '%s\n' mipsle ;;
		mips)
			# hexdump, not od: the stock BusyBox has CONFIG_OD unset, so od is
			# missing on a default OpenWrt install while hexdump is present.
			elf_data="\$(hexdump -s 5 -n 1 -e '1/1 "%u"' /bin/busybox 2>/dev/null)"
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
	exit 1
}
_sha=''
_url=''
case "\${_suffix}" in${arch_postinst_case}
esac
[ -n "\${_sha}" ] && [ -n "\${_url}" ] || {
	echo "This OpenSocks release has no binary for CPU architecture: \$(uname -m)" >&2
	exit 1
}

uci set opensocks.settings.binary_url="\${_url}"
uci set opensocks.settings.binary_sha256="\${_sha}"
uci commit opensocks
/etc/init.d/opensocks enable
( sleep 2; /etc/init.d/opensocks start >/dev/null 2>&1 || true ) &
exit 0
EOF
cp "$ROOT/openwrt/opensocks-minimal/files/etc/init.d/opensocks" "$minimal_data/etc/init.d/opensocks"
cp "$ROOT/openwrt/opensocks-minimal/files/usr/lib/opensocks/launcher.sh" "$minimal_data/usr/lib/opensocks/launcher.sh"
sed \
	-e "s|@BINARY_SHA256@|$default_sha|g" \
	-e "s|@BINARY_NAME@|$default_gz|g" \
	-e "s|@BINARY_URL@|$default_url|g" \
	"$ROOT/openwrt/opensocks-minimal/files/etc/config/opensocks" > "$minimal_data/etc/config/opensocks"
chmod 0755 "$minimal_control/postinst" "$minimal_data/etc/init.d/opensocks" "$minimal_data/usr/lib/opensocks/launcher.sh"
build_ipk opensocks-minimal all "$minimal_control" "$minimal_data"

luci_control="$WORK/luci-control"
luci_data="$WORK/luci-data"
mkdir -p "$luci_control" "$luci_data/usr/lib/lua/luci/controller" "$luci_data/usr/lib/lua/luci/view/opensocks" \
	"$luci_data/usr/lib/lua/luci/i18n" "$luci_data/usr/share/rpcd/acl.d"
cat > "$luci_control/control" <<EOF
Package: luci-app-opensocks
Version: ${VERSION}-${RELEASE}
Depends: libc, opensocks | opensocks-minimal, luci-base, luci-compat, curl
Source: luci-app-opensocks
Section: luci
Architecture: all
Maintainer: OpenSocks Developers
Description: LuCI interface for OpenSocks
EOF
cp "$ROOT/openwrt/luci-app-opensocks/luasrc/controller/opensocks.lua" "$luci_data/usr/lib/lua/luci/controller/opensocks.lua"
cp "$ROOT/openwrt/luci-app-opensocks/luasrc/view/opensocks/status.htm" "$luci_data/usr/lib/lua/luci/view/opensocks/status.htm"
cp "$ROOT/openwrt/luci-app-opensocks/root/usr/share/rpcd/acl.d/luci-app-opensocks.json" "$luci_data/usr/share/rpcd/acl.d/"
po2lmo="${PO2LMO:-}"
if [ -z "$po2lmo" ]; then
	git -C "$WORK" init -q luci
	git -C "$WORK/luci" remote add origin https://github.com/openwrt/luci.git
	git -C "$WORK/luci" fetch -q --depth 1 origin 7a420e0c069d2257213136151c9c21565feced49
	git -C "$WORK/luci" checkout -q FETCH_HEAD
	make -s -C "$WORK/luci/modules/luci-base/src" po2lmo
	po2lmo="$WORK/luci/modules/luci-base/src/po2lmo"
fi
"$po2lmo" "$ROOT/openwrt/luci-app-opensocks/po/ja/opensocks.po" "$luci_data/usr/lib/lua/luci/i18n/opensocks.ja.lmo"
"$po2lmo" "$ROOT/openwrt/luci-app-opensocks/po/zh_Hans/opensocks.po" "$luci_data/usr/lib/lua/luci/i18n/opensocks.zh-cn.lmo"
build_ipk luci-app-opensocks all "$luci_control" "$luci_data"

# Build an opkg feed index beside the release packages.
: > "$OUT/Packages"
for ipk in "$OUT"/*.ipk; do
	name="$(basename "$ipk")"
	ipk_sha="$(shasum -a 256 "$ipk" | awk '{print $1}')"
	tar -xOzf "$ipk" control.tar.gz | tar -xzO ./control >> "$OUT/Packages"
	printf 'Filename: by-sha/%s/%s\nSize: %s\nSHA256sum: %s\n\n' "$ipk_sha" "$name" \
		"$(wc -c < "$ipk" | tr -d ' ')" "$ipk_sha" >> "$OUT/Packages"
done
gzip -9c "$OUT/Packages" > "$OUT/Packages.gz"

USIGN_BIN="${USIGN:-usign}"
SIGNING_KEY="${OPKG_SIGNING_KEY_FILE:-}"
PUBLIC_KEY="${OPKG_PUBLIC_KEY_FILE:-}"
if [ -n "$SIGNING_KEY" ]; then
	[ -f "$SIGNING_KEY" ] || { printf 'Signing key not found: %s\n' "$SIGNING_KEY" >&2; exit 1; }
	[ -f "$PUBLIC_KEY" ] || { printf 'Public key not found: %s\n' "$PUBLIC_KEY" >&2; exit 1; }
	command -v "$USIGN_BIN" >/dev/null 2>&1 || [ -x "$USIGN_BIN" ] || {
		printf 'usign executable not found: %s\n' "$USIGN_BIN" >&2
		exit 1
	}
	key_fingerprint="$($USIGN_BIN -F -p "$PUBLIC_KEY")"
	cp "$PUBLIC_KEY" "$OUT/$key_fingerprint"
	"$USIGN_BIN" -S -m "$OUT/Packages" -s "$SIGNING_KEY" -x "$OUT/Packages.sig"
	"$USIGN_BIN" -V -m "$OUT/Packages" -p "$PUBLIC_KEY" -x "$OUT/Packages.sig"
elif [ "${REQUIRE_SIGNATURE:-0}" = "1" ]; then
	printf 'OPKG_SIGNING_KEY_FILE is required; refusing to publish unsigned packages.\n' >&2
	exit 1
fi

# Build the OpenWrt 25.12 apk repository beside the opkg feed. Both feeds are
# produced from the same release directory so the artifacts stay in lockstep.
#
# Apk tooling is optional: a developer without apk-tools still gets a complete
# opkg release. The step is skipped with a clear message unless BUILD_APK=1 was
# requested explicitly, in which case a missing apk binary is a hard failure.
build_apk_repo() {
	APK_BIN="${APK_BIN:-}"
	apk_key_dir="${APK_KEY_DIR:-}"

	if [ -z "$APK_BIN" ] && [ -n "${APK_TOOLS_DIR:-}" ]; then
		APK_BIN="$APK_TOOLS_DIR/apk"
	fi
	if [ -z "$APK_BIN" ]; then
		APK_BIN="$(command -v apk 2>/dev/null || true)"
	fi

	if [ -z "$APK_BIN" ] || [ ! -x "$APK_BIN" ]; then
		if [ "${BUILD_APK:-auto}" = "1" ]; then
			printf 'BUILD_APK=1 but no apk binary was found.\n' >&2
			printf 'Set APK_BIN or APK_TOOLS_DIR, or build one with tools/build-apk-tool.sh.\n' >&2
			exit 1
		fi
		printf 'Skipping apk packages: no apk-tools binary found.\n'
		printf 'Install apk-tools or run tools/build-apk-tool.sh, then set APK_BIN.\n'
		return 0
	fi

	# A signing key is required for a publishable repository. APK_KEY_DIR is a
	# convenience for a directory holding private.pem/public.pem.
	if [ -z "${APK_SIGNING_KEY_FILE:-}" ] && [ -n "$apk_key_dir" ]; then
		APK_SIGNING_KEY_FILE="$apk_key_dir/private.pem"
		APK_PUBLIC_KEY_FILE="${APK_PUBLIC_KEY_FILE:-$apk_key_dir/public.pem}"
	fi
	if [ -z "${APK_SIGNING_KEY_FILE:-}" ] && [ -n "${APK_SIGNING_KEY:-}" ]; then
		# Same convention as the usign secret: a raw key in an environment var.
		umask 077
		printf '%s\n' "$APK_SIGNING_KEY" > "$WORK/opensocks-apk.sec"
		APK_SIGNING_KEY_FILE="$WORK/opensocks-apk.sec"
	fi

	# apk-tools 3 cannot represent an unsigned repository: mkpkg signs each
	# package with the key in its trust context, and mkndx then refuses to index
	# a package whose signature it cannot verify. Without a key there is
	# therefore nothing installable to publish, so skip the feed entirely rather
	# than fail halfway through. A tagged release is expected to have the key.
	if [ -z "${APK_SIGNING_KEY_FILE:-}" ]; then
		if [ "${APK_REQUIRE_SIGNATURE:-0}" = "1" ]; then
			printf 'APK_SIGNING_KEY_FILE is required for this release.\n' >&2
			exit 1
		fi
		printf 'Skipping apk packages: no apk signing key.\n'
		printf 'apk-tools cannot index unsigned packages; set APK_SIGNING_KEY_FILE or APK_KEY_DIR.\n'
		return 0
	fi

	# Pass the NORMALISED target list: build-apk-repo.sh splits on ';' too, but
	# the raw value can carry stray separators or newlines from the caller, and
	# a malformed list silently drops architectures (aarch64 in particular).
	APK_SIGNING_KEY_FILE="${APK_SIGNING_KEY_FILE:-}" \
	APK_PUBLIC_KEY_FILE="${APK_PUBLIC_KEY_FILE:-}" \
	REQUIRE_SIGNATURE=1 \
	FEED_BASE="$FEED_BASE" \
	OPENSOCKS_TARGETS="$TARGETS_NORMALIZED" \
		"$ROOT/tools/build-apk-repo.sh" \
			"$OUT" "$VERSION" "$RELEASE" "$APK_BIN" "" || return 1

	# The apk feed is served from <feed-base>/apk/ so the URL does not collide
	# with the opkg index that lives at the feed root. tools/sync-opkg-release.sh
	# publishes it there; keep this default in step with that script and the
	# README install instructions.
	apk_base="${APK_FEED_BASE:-$FEED_BASE/apk}"
	printf '\napk repository published under %s\n' "$OUT/repo"
	printf '  install the key:  wget -O /etc/apk/keys/opensocks.pem %s/opensocks.pem\n' "$apk_base"
	printf '  register the feed: echo %s/packages.adb >> /etc/apk/repositories.d/customfeeds.list\n' "$apk_base"
	printf '  install:          apk update && apk add opensocks-minimal luci-app-opensocks\n'
}

write_checksums() {
	( cd "$OUT" && find . -maxdepth 1 -type f ! -name SHA256SUMS ! -name SHA256SUMS.sig -print | sort | xargs shasum -a 256 > SHA256SUMS )
	# The apk repository lives in repo/ so it gets its own manifest; the flat
	# opkg manifest above keeps its maxdepth-1 behaviour untouched.
	if [ -d "$OUT/repo" ]; then
		( cd "$OUT/repo" && find . -maxdepth 1 -type f ! -name SHA256SUMS -print | sort | xargs shasum -a 256 > SHA256SUMS )
	fi
	if [ -n "$SIGNING_KEY" ]; then
		"$USIGN_BIN" -S -m "$OUT/SHA256SUMS" -s "$SIGNING_KEY" -x "$OUT/SHA256SUMS.sig"
		"$USIGN_BIN" -V -m "$OUT/SHA256SUMS" -p "$PUBLIC_KEY" -x "$OUT/SHA256SUMS.sig"
	fi
}

if [ "${OPENWRT_ONLY:-0}" = "1" ]; then
	build_apk_repo
	write_checksums
	printf 'OpenWrt release artifacts written to %s\n' "$OUT"
	exit 0
fi

( cd "$ROOT/mobile" && flutter build apk --release )
cp "$ROOT/mobile/build/app/outputs/flutter-apk/app-release.apk" "$OUT/OpenSocks-${VERSION}-android.apk"
build_apk_repo
write_checksums
printf 'Release artifacts written to %s\n' "$OUT"
