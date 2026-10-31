#!/bin/sh
# Regression test for the feed refresh clobbering an installed pin.
#
# The bug this guards: refresh_release_metadata() read a usign-signed Packages
# index and wrote its binary_url/binary_sha256 straight into UCI. The default
# feed is the rel.n4t.su mirror, which still advertises 0.2.13. On a 25.12.6
# router that undid a package upgrade: "apk add --upgrade" installed 0.3.3 and
# pinned binary_url to the v0.3.3 asset, then the next service start read the
# stale index, wrote 0.2.13's URL and SHA back, and ran the old daemon while
# apk reported 0.3.3 installed.
#
# The refresh is a fallback for a router with no usable pin. It must never
# replace a pin the package installed.
set -eu

root="$(CDPATH= cd -- "$(dirname "$0")/../../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT INT TERM

fail() {
	printf 'FAIL feed pin: %s\n' "$1" >&2
	exit 1
}

launcher="$root/openwrt/opensocks-minimal/files/usr/lib/opensocks/launcher.sh"
[ -f "$launcher" ] || fail "launcher.sh not found"

# --- Static checks -----------------------------------------------------------
# The refresh must read the installed pin and return before writing when it is
# usable. Matching on the shipped text keeps this honest without standing up a
# fake apk database and usign.

# 1) It reads the current UCI pin.
grep -q 'current_sha="\$(uci -q get opensocks.settings.binary_sha256)"' "$launcher" ||
	fail 'refresh must read the installed binary_sha256 before deciding'

# 2) It returns when the installed pin is a well-formed 64-hex hash.
grep -q 'if \[ \${#current_sha} -eq 64 \] && \[ -n "\$current_url" \]; then' "$launcher" ||
	fail 'refresh must treat a well-formed installed pin as authoritative'

# 3) The comparison is against "current", not "new": a newer-index-wins check
#    would have passed the test above while still rolling 0.3.3 back to 0.2.13.
if grep -q 'newer_than_installed' "$launcher"; then
	fail 'refresh must not compare release versions; the installed pin wins outright'
fi

# 4) The refresh still exists as a fallback (a router with no pin needs it).
grep -q '^refresh_release_metadata() {' "$launcher" ||
	fail 'refresh_release_metadata must still exist for a router with no pin'

# --- Behavioural checks ------------------------------------------------------
# Drive the shipped decision block with stub uci/wget/usign, exactly as the
# launcher calls it. Only the guard under test is exercised.

# Extract the refresh function body and run it against a stub environment.
awk '/^refresh_release_metadata\(\) \{/,/^\}$/' "$launcher" > "$work/refresh.sh"
[ -s "$work/refresh.sh" ] || fail 'could not extract refresh_release_metadata'

mkdir -p "$work/bin" "$work/tmp"
cat > "$work/bin/uci" <<'STUB'
#!/bin/sh
# uci stub: "uci -q get <key>" reads from $UCI_FILE; set/commit append to a log.
# The launcher always passes -q before the verb, so skip leading options first.
while [ $# -gt 0 ]; do
	case "$1" in
	-q | -quiet) shift ;;
	*) break ;;
	esac
done
case "$1" in
get)
	sed -n "s/^$2=//p" "$UCI_FILE" 2>/dev/null | head -n 1
	;;
set)
	printf '%s\n' "$2" >> "$UCI_CHANGES"
	;;
commit) : ;;
esac
STUB
chmod 0755 "$work/bin/uci"

cat > "$work/bin/wget" <<'STUB'
#!/bin/sh
# wget stub: "wget -q -O <path> <url>" writes the canned index or signature.
out=""
while [ $# -gt 0 ]; do
	case "$1" in
	-O) out="$2"; shift 2 ;;
	*) shift ;;
	esac
done
[ -n "$out" ] || exit 1
case "$out" in
*.sig) printf 'sig\n' > "$out" ;;
*) cat "$FEED_PACKAGES" > "$out" ;;
esac
STUB
chmod 0755 "$work/bin/wget"

cat > "$work/bin/usign" <<'STUB'
#!/bin/sh
# Signature verification always succeeds; this test is about the pin decision.
exit 0
STUB
chmod 0755 "$work/bin/usign"

cat > "$work/bin/uname" <<'STUB'
#!/bin/sh
printf 'mipsel\n'
STUB
chmod 0755 "$work/bin/uname"

# A feed that advertises an OLDER release than the installed pin, which is what
# the rel.n4t.su mirror does today.
cat > "$work/Packages" <<'EOF'
Package: opensocks-minimal
Version: 0.2.13-1
X-OpenSocks-Binary-SHA256-mipsle: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
X-OpenSocks-Binary-URL-mipsle: https://rel.n4t.su/opkg/by-sha/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/opensocks-linux-mipsle.gz
EOF

installed_sha='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
installed_url='https://github.com/rticks/OpenSocks/releases/download/v0.3.3/opensocks-linux-mipsle.gz'

run_refresh() {
	: > "$work/changes"
	(
		# uci and wget are separate processes, so these must be exported or the
		# stubs see nothing and every case looks like "no change".
		PATH="$work/bin:$PATH"
		export PATH
		UCI_FILE="$work/uci.conf"
		export UCI_FILE
		UCI_CHANGES="$work/changes"
		export UCI_CHANGES
		FEED_PACKAGES="$work/Packages"
		export FEED_PACKAGES
		dir="$work/tmp"
		feed='https://rel.n4t.su/opkg'
		# refresh_release_metadata bails on "[ -s $feed_key ]", so the stub key
		# must be non-empty or every case would return before the guard.
		feed_key="$work/key"
		printf 'stub-key\n' > "$feed_key"
		# Functions the refresh calls, taken verbatim from the launcher.
		detect_binary_suffix() { printf 'mipsle\n'; }
		binary_url_matches() {
			name="opensocks-linux-$3.gz"
			case "$1" in
			*/"$name") return 0 ;;
			*/by-sha/"$2"/"$name") return 0 ;;
			esac
			return 1
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
		. "$work/refresh.sh"
		refresh_release_metadata || true
	) 2>/dev/null || true
}

# 1) A usable installed pin must survive an index that advertises an older build.
printf 'opensocks.settings.binary_sha256=%s\nopensocks.settings.binary_url=%s\n' \
	"$installed_sha" "$installed_url" > "$work/uci.conf"
run_refresh
if [ -s "$work/changes" ]; then
	fail "the refresh rewrote UCI although the installed pin was usable:
$(cat "$work/changes")"
fi

# 2) A missing pin must be filled in from the index (the fallback still works).
: > "$work/uci.conf"
run_refresh
grep -q "binary_sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "$work/changes" ||
	fail 'the refresh must fill in a missing pin from the index'

# 3) A malformed pin must also be replaced.
printf 'opensocks.settings.binary_sha256=not-a-hash\nopensocks.settings.binary_url=https://example.invalid/x.gz\n' > "$work/uci.conf"
run_refresh
grep -q "binary_sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "$work/changes" ||
	fail 'the refresh must replace a malformed pin'

printf 'PASS feed pin: installed pin wins, missing/malformed pin filled (2 static groups + 3 cases)\n'
