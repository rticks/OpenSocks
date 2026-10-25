#!/bin/sh
# Download-on-boot launcher for the OpenSocks daemon.
#
# The package ships no daemon binary; it carries the expected URL and SHA-256 in
# UCI (written by the package's post-install) and fetches the binary on start.
# That keeps the installed footprint small, which is the whole point of the
# "minimal" variant.
#
# Two URL layouts are accepted:
#
#   <base>/opensocks-linux-<suffix>.gz               flat (GitHub Releases)
#   <base>/by-sha/<sha256>/opensocks-linux-<suffix>.gz  content addressed
#
# The second is what the rel.n4t.su mirror uses so a CDN can cache forever; the
# first is what a plain release download looks like. Either way the archive is
# verified against the configured SHA-256 before anything is executed, so the
# URL itself is not a trust boundary.
set -u

dir=/tmp/opensocks
program="$dir/opensocks"
archive="$dir/opensocks.gz"

# Optional index refresh. On OpenWrt 22.03-24.10 the opkg mirror publishes a
# usign-signed Packages file we can re-read to learn about newer builds. OpenWrt
# 25.12 has neither opkg nor usign, so this is skipped there and the URL baked in
# by post-install is used as-is.
feed="${OPENSOCKS_FEED:-https://rel.n4t.su/opkg}"
feed_key="${OPENSOCKS_FEED_KEY:-/etc/opkg/keys/d24a5e234001294c}"

attempts=5
retry_delay=10

log() {
	echo "opensocks-minimal: $*" >&2
}

detect_binary_suffix() {
	case "$(uname -m)" in
		mipsel) printf '%s\n' mipsle ;;
		mips)
			# Some little-endian OpenWrt kernels report only "mips". Read the
			# ELF EI_DATA byte instead of guessing the byte order from uname.
			# hexdump, not od: the stock BusyBox has CONFIG_OD unset, so od is
			# absent on a default OpenWrt install while hexdump is present.
			elf_data="$(hexdump -s 5 -n 1 -e '1/1 "%u"' /bin/busybox 2>/dev/null)"
			case "$elf_data" in
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

package_field() {
	field="$1"
	awk -v wanted="$field: " '
		BEGIN { RS=""; FS="\n" }
		$0 ~ /(^|\n)Package: opensocks-minimal(\n|$)/ {
			for (i = 1; i <= NF; i++)
				if (index($i, wanted) == 1) {
					print substr($i, length(wanted) + 1)
					exit
				}
		}
	' "$dir/Packages"
}

# Is $1 exactly opensocks-linux-<suffix>.gz, or that name under by-sha/<hex>/?
binary_url_matches() {
	url="$1"
	sha="$2"
	suffix="$3"
	name="opensocks-linux-${suffix}.gz"
	case "$url" in
	*/"$name") return 0 ;;
	*/by-sha/"$sha"/"$name") return 0 ;;
	esac
	return 1
}

# Best effort: only runs where a usign-signed opkg index is actually available.
refresh_release_metadata() {
	command -v usign >/dev/null 2>&1 || return 1
	[ -s "$feed_key" ] || return 1
	suffix="$(detect_binary_suffix)" || return 1
	rm -f "$dir/Packages.new" "$dir/Packages.sig.new"
	wget -q -O "$dir/Packages.new" "$feed/Packages" || {
		rm -f "$dir/Packages.new" "$dir/Packages.sig.new"
		return 1
	}
	wget -q -O "$dir/Packages.sig.new" "$feed/Packages.sig" || {
		rm -f "$dir/Packages.new" "$dir/Packages.sig.new"
		return 1
	}
	usign -V -m "$dir/Packages.new" -p "$feed_key" -x "$dir/Packages.sig.new" >/dev/null 2>&1 || {
		rm -f "$dir/Packages.new" "$dir/Packages.sig.new"
		return 1
	}
	mv "$dir/Packages.new" "$dir/Packages"
	mv "$dir/Packages.sig.new" "$dir/Packages.sig"

	# Prefer architecture-specific metadata. Only mipsle may use the legacy
	# fields because releases before multi-architecture support were mipsle-only.
	new_sha="$(package_field "X-OpenSocks-Binary-SHA256-${suffix}")"
	new_url="$(package_field "X-OpenSocks-Binary-URL-${suffix}")"
	if { [ ${#new_sha} -ne 64 ] || [ -z "$new_url" ]; } && [ "$suffix" = "mipsle" ]; then
		new_sha="$(package_field X-OpenSocks-Binary-SHA256)"
		new_url="$(package_field X-OpenSocks-Binary-URL)"
	fi
	[ ${#new_sha} -eq 64 ] || return 1
	case "$new_sha" in *[!0-9a-f]*) return 1;; esac
	binary_url_matches "$new_url" "$new_sha" "$suffix" || return 1
	uci set opensocks.settings.binary_sha256="$new_sha"
	uci set opensocks.settings.binary_url="$new_url"
	uci commit opensocks
}

download_and_verify() {
	suffix="$1"
	url="$(uci -q get opensocks.settings.binary_url)"
	sha="$(uci -q get opensocks.settings.binary_sha256)"
	[ -n "$url" ] && [ ${#sha} -eq 64 ] || return 1
	case "$sha" in *[!0-9a-f]*) return 1;; esac
	# Refuse to fetch anything that is not the daemon archive for this CPU, even
	# if the URL in UCI were tampered with: the name is pinned and the contents
	# are checked against the hash below.
	binary_url_matches "$url" "$sha" "$suffix" || return 1

	rm -f "$archive.part" "$program.new"
	wget -q -O "$archive.part" "$url" || {
		rm -f "$archive.part"
		return 1
	}
	actual="$(sha256sum "$archive.part" | awk '{print $1}')"
	[ "$actual" = "$sha" ] || {
		log "checksum mismatch for $url (expected $sha, got $actual)"
		rm -f "$archive.part"
		return 1
	}
	gzip -t "$archive.part" || {
		log "downloaded file is not a gzip archive: $url"
		rm -f "$archive.part"
		return 1
	}
	mv "$archive.part" "$archive"
	gzip -dc "$archive" > "$program.new" || {
		log "cannot decompress $archive"
		rm -f "$program.new"
		return 1
	}
	chmod 0755 "$program.new"
	mv "$program.new" "$program"
}

# If the daemon is already unpacked there is nothing to do.
[ -x "$program" ] && exec "$program" --daemon

mkdir -p "$dir"

binary_suffix="$(detect_binary_suffix)" || {
	log "unsupported CPU architecture: $(uname -m)"
	exit 1
}

refresh_release_metadata || true

configured_sha="$(uci -q get opensocks.settings.binary_sha256)"
configured_url="$(uci -q get opensocks.settings.binary_url)"

# A stale archive from an older release must not be reused.
if [ -s "$archive" ] &&
	[ "$(sha256sum "$archive" 2>/dev/null | awk '{print $1}')" != "$configured_sha" ]; then
	rm -f "$archive"
fi

if [ ! -s "$archive" ] &&
	! binary_url_matches "$configured_url" "$configured_sha" "$binary_suffix"; then
	log "no download configured for CPU architecture: $(uname -m)"
	log "binary_url is '$configured_url'"
	exit 1
fi

# A bounded retry: procd respawns this script with its own backoff, so looping
# here forever (as earlier revisions did) only hides the failure from the log and
# keeps the service in "running" state while the API stays unreachable.
n=1
while :; do
	if [ -s "$archive" ] &&
		[ "$(sha256sum "$archive" 2>/dev/null | awk '{print $1}')" = "$configured_sha" ]; then
		gzip -dc "$archive" > "$program" 2>/dev/null && chmod 0755 "$program" && break
		rm -f "$program"
	fi
	download_and_verify "$binary_suffix" && break
	if [ "$n" -ge "$attempts" ]; then
		log "download failed after $n attempts; giving up so procd can back off"
		log "url: $configured_url"
		exit 1
	fi
	n=$((n + 1))
	log "download failed, retrying in ${retry_delay}s ($n/$attempts)"
	sleep "$retry_delay"
done

exec "$program" --daemon
