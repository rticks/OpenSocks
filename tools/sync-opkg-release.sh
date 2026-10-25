#!/bin/sh
# Publish the newest GitHub Release as the public feeds served by rel.n4t.su.
#
#   <publish_root>/opkg/        opkg feed (OpenWrt 22.03-24.10)
#   <publish_root>/opkg/apk/    apk feed  (OpenWrt 25.12+), when the release
#                               carries a packages.adb index
#
# The apk index and its .apk files must stay in one directory: mkndx records a
# pkgname-spec so apk resolves ${name}-${version}-${arch}.apk beside the index.
set -eu

repository="${GITHUB_REPOSITORY:-NarYuki/OpenSocks}"
publish_root="${PUBLISH_ROOT:-/srv/opensocks-repo}"
usign_bin="${USIGN:-/usr/local/bin/usign}"
public_key_fingerprint="d24a5e234001294c"
api="https://api.github.com/repos/$repository/releases/latest"

tag="${RELEASE_TAG:-}"
if [ -z "$tag" ]; then
	tag="$(curl -fsSL -H 'Accept: application/vnd.github+json' "$api" |
		jq -er '.tag_name')"
fi

case "$tag" in
	v[0-9]*) ;;
	*) printf 'Invalid release tag: %s\n' "$tag" >&2; exit 1 ;;
esac
version="${tag#v}"

# Releases are immutable. If this exact tag is already the active generation,
# do not download and republish identical assets on every timer invocation.
current_generation="$(readlink "$publish_root/opkg" 2>/dev/null || true)"
case "$current_generation" in
	".opkg-$tag-"*)
		printf 'OpenSocks Release %s is already active; skipping synchronization\n' "$tag"
		exit 0
		;;
esac

command -v curl >/dev/null
command -v jq >/dev/null
# usign is only needed to verify a SIGNED opkg feed. A fork that does not own the
# upstream key publishes an unsigned opkg index, and must still be able to sync
# the apk feed, so the binary is not a hard requirement here; the branches below
# check it before actually invoking it.
if [ -n "${REQUIRE_USIGN:-0}" ] && [ "${REQUIRE_USIGN}" = "1" ]; then
	test -x "$usign_bin"
fi

stage="$(mktemp -d "$publish_root/.opkg-${tag}.XXXXXX")"
trap 'rm -rf "$stage"' EXIT INT TERM
base="https://github.com/$repository/releases/download/$tag"
cache_bust="$(date -u +%s)"

# Packages/Packages.gz and SHA256SUMS are always published. The usign signature
# and the key file are not: a fork that does not own the upstream opkg key mints
# an UNSIGNED opkg feed, and the release then simply has no Packages.sig. The apk
# feed is what such a repository publishes, so a missing usign signature must not
# block the whole sync.
metadata_assets="Packages Packages.gz SHA256SUMS"

for asset in $metadata_assets; do
	curl -fL --retry 3 --connect-timeout 15 -o "$stage/$asset" "$base/$asset?cache=$cache_bust"
done

opkg_signed=no
if curl -fsL --retry 3 --connect-timeout 15 \
	-o "$stage/Packages.sig" "$base/Packages.sig?cache=$cache_bust" &&
	curl -fsL --retry 3 --connect-timeout 15 \
		-o "$stage/$public_key_fingerprint" \
		"$base/$public_key_fingerprint?cache=$cache_bust"; then
	opkg_signed=yes
else
	rm -f "$stage/Packages.sig" "$stage/$public_key_fingerprint"
	printf 'opkg feed is UNSIGNED (no Packages.sig in this release); publishing it as-is\n' >&2
fi

if [ "$opkg_signed" = yes ]; then
	test "$("$usign_bin" -F -p "$stage/$public_key_fingerprint")" = "$public_key_fingerprint"
	"$usign_bin" -V -m "$stage/Packages" -p "$stage/$public_key_fingerprint" -x "$stage/Packages.sig"
fi

# Collect every Filename basenames referenced by the signed Packages index.
package_assets="$(
	awk '
		BEGIN { RS=""; FS="\n" }
		{
			for (i = 1; i <= NF; i++) {
				if ($i ~ /^Filename: by-sha\//) {
					sub(/^Filename: by-sha\/[0-9a-f]+\//, "", $i)
					print $i
				}
			}
		}
	' "$stage/Packages" | sort -u
)"

# Also publish every gzipped daemon binary listed in SHA256SUMS.
binary_assets="$(
	awk '
		{
			name=$2
			sub(/^\.\//, "", name)
			if (name ~ /^opensocks-linux-.*\.gz$/) print name
		}
	' "$stage/SHA256SUMS" | sort -u
)"

assets="$(printf '%s\n%s\n' "$package_assets" "$binary_assets" | awk 'NF && !seen[$0]++')"
[ -n "$assets" ] || {
	printf 'No package assets found in Packages/SHA256SUMS\n' >&2
	exit 1
}

printf '%s\n' "$assets" > "$stage/.asset-list"
while IFS= read -r asset; do
	[ -n "$asset" ] || continue
	case "$asset" in
		luci-app-opensocks_${version}-*_all.ipk) ;;
		opensocks-minimal_${version}-*_all.ipk) ;;
		opensocks_${version}-*_*.ipk) ;;
		opensocks-linux-*.gz) ;;
		*)
			printf 'Unexpected release asset from index: %s\n' "$asset" >&2
			exit 1
			;;
	esac
	curl -fL --retry 3 --connect-timeout 15 -o "$stage/$asset" "$base/$asset?cache=$cache_bust"
done < "$stage/.asset-list"
rm -f "$stage/.asset-list"

if [ "$opkg_signed" = yes ]; then
	if curl -fsL --retry 3 --connect-timeout 15 \
		-o "$stage/SHA256SUMS.sig" "$base/SHA256SUMS.sig?cache=$cache_bust"; then
		"$usign_bin" -V -m "$stage/SHA256SUMS" \
			-p "$stage/$public_key_fingerprint" -x "$stage/SHA256SUMS.sig"
	else
		rm -f "$stage/SHA256SUMS.sig"
	fi
fi
# The manifest itself is checked in every case; the signature above proves the
# manifest is the one the release produced.
(cd "$stage" && sha256sum -c SHA256SUMS)
gzip -t "$stage/Packages.gz"

# Cloudflare may ignore query strings in its cache key. Publish immutable,
# content-addressed aliases so updated packages and boot binaries can never be
# confused with an older asset that used the same release filename.
while read -r checksum relative; do
	name="${relative#./}"
	case "$name" in
		Packages|Packages.gz|Packages.sig|SHA256SUMS|SHA256SUMS.sig|"$public_key_fingerprint") continue ;;
	esac
	mkdir -p "$stage/by-sha/$checksum"
	# Use a same-filesystem hard link. Some HTTP servers intentionally refuse
	# symlinks outside their configured path even when the resolved file stays
	# below the document root, which makes opkg's immutable URL return 404.
	ln "$stage/$name" "$stage/by-sha/$checksum/$name"
done < "$stage/SHA256SUMS"

# ---------------------------------------------------------------------------
# OpenWrt 25.12 apk feed (optional)
# ---------------------------------------------------------------------------
# The apk feed is published under <publish_root>/opkg/apk/ so a 25.12 client
# registers https://.../opkg/apk/packages.adb, next to the opkg index that
# older routers keep using. mkndx records a pkgname-spec, so the .apk files must
# sit in the same directory as packages.adb.
#
# Releases built before the apk feed existed simply have no packages.adb; that
# is not an error, so skip the whole block when the index is absent.
apk_stage="$stage/apk"
apk_index_url="$base/packages.adb?cache=$cache_bust"
if curl -fsL --retry 3 --connect-timeout 15 -o "$stage/.packages.adb" "$apk_index_url"; then
	mkdir -p "$apk_stage"
	mv "$stage/.packages.adb" "$apk_stage/packages.adb"

	apk_metadata="packages.adb.gz opensocks.pem apk-SHA256SUMS"
	for asset in $apk_metadata; do
		# The public key and the manifests are expected; a missing .gz or
		# manifest only means the producer skipped it.
		curl -fsL --retry 3 --connect-timeout 15 \
			-o "$apk_stage/$asset" "$base/$asset?cache=$cache_bust" || {
			rm -f "$apk_stage/$asset"
			case "$asset" in
				opensocks.pem)
					printf 'apk feed is unsigned (no opensocks.pem published)\n' >&2
					;;
				*)
					printf 'apk asset not published, skipping: %s\n' "$asset" >&2
					;;
			esac
		}
	done

	# Every package is named in the apk manifest. Using the manifest rather than
	# guessing from the index keeps the allow-list strict.
	if [ -f "$apk_stage/apk-SHA256SUMS" ]; then
		apk_assets="$(awk '{ name=$2; sub(/^\.\//, "", name); if (name ~ /\.apk$/) print name }' \
			"$apk_stage/apk-SHA256SUMS" | sort -u)"
		[ -n "$apk_assets" ] || {
			printf 'apk-SHA256SUMS lists no packages\n' >&2
			exit 1
		}
		printf '%s\n' "$apk_assets" > "$stage/.apk-list"
		while IFS= read -r asset; do
			[ -n "$asset" ] || continue
			case "$asset" in
				opensocks-${version}-r*.apk) ;;
				opensocks-minimal-${version}-r*.apk) ;;
				luci-app-opensocks-${version}-r*.apk) ;;
				*)
					printf 'Unexpected apk release asset: %s\n' "$asset" >&2
					exit 1
					;;
			esac
			curl -fL --retry 3 --connect-timeout 15 \
				-o "$apk_stage/$asset" "$base/$asset?cache=$cache_bust"
		done < "$stage/.apk-list"
		rm -f "$stage/.apk-list"
		(cd "$apk_stage" && sha256sum -c apk-SHA256SUMS)
	else
		# Without a manifest fall back to whatever .apk files the release has,
		# so an older-style publish still upgrades clients.
		apk_assets="$(awk '{ name=$2; sub(/^\.\//, "", name); if (name ~ /\.apk$/) print name }' \
			"$stage/SHA256SUMS" | sort -u)"
		if [ -n "$apk_assets" ]; then
			printf '%s\n' "$apk_assets" > "$stage/.apk-list"
			while IFS= read -r asset; do
				[ -n "$asset" ] || continue
				curl -fL --retry 3 --connect-timeout 15 \
					-o "$apk_stage/$asset" "$base/$asset?cache=$cache_bust"
			done < "$stage/.apk-list"
			rm -f "$stage/.apk-list"
		fi
	fi

	printf 'apk feed staged with %s package(s)\n' \
		"$(find "$apk_stage" -maxdepth 1 -name '*.apk' | wc -l | tr -d ' ')"
else
	rm -f "$stage/.packages.adb"
	printf 'No apk index in this release; publishing the opkg feed only\n' >&2
	rmdir "$apk_stage" 2>/dev/null || true
fi

find "$stage" -type d -exec chmod 0755 {} +
find "$stage" -type f -exec chmod 0644 {} +

generation=".opkg-$tag-$(date -u +%Y%m%dT%H%M%SZ)"
final="$publish_root/$generation"
mv "$stage" "$final"
trap - EXIT INT TERM

next="$publish_root/.opkg.next"
ln -s "$generation" "$next"
if [ -d "$publish_root/opkg" ] && [ ! -L "$publish_root/opkg" ]; then
	mv "$publish_root/opkg" "$publish_root/opkg.before-sync"
fi
mv -Tf "$next" "$publish_root/opkg"

find "$publish_root" -maxdepth 1 -type d -name '.opkg-v*' -mtime +7 -exec rm -rf {} +
printf 'Published %s from GitHub Release %s\n' "$final" "$tag"
