#!/bin/sh
# Regression test for the launcher's binary cache.
#
# The bug this guards: the launcher used to exec /tmp/opensocks/opensocks as soon
# as the file existed, without comparing it to the binary_sha256 the package
# ships. /tmp is a tmpfs that only resets on reboot, so after the first boot every
# later package upgrade kept running the previous release's binary: "apk add"
# reported the new version while the daemon did not change.
#
# The fix records binary_sha256 in a stamp file next to the binary and only takes
# the fast path when the stamp matches the configured release. This test drives
# the real decision code, extracted from the shipped script, so the condition
# cannot drift from what ships.
set -eu

root="$(CDPATH= cd -- "$(dirname "$0")/../../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT INT TERM

src="$root/openwrt/opensocks-minimal/files/usr/lib/opensocks/launcher.sh"

fail() {
	printf 'FAIL %s\n' "$*" >&2
	exit 1
}

# Extract the fast-path condition verbatim from the shipped launcher.
tr -d '\r' < "$src" | sed -n '/^# Fast path:/,/^fi$/p' > "$work/fastpath.sh"
[ -s "$work/fastpath.sh" ] || fail "could not extract the fast-path block from launcher.sh"

# The extracted block refers to "$program" and "$stamp"; point them at the sandbox
# and replace "exec" with a marker so the test can observe the decision instead of
# handing the shell over to a binary.
sed -i 's|exec "\$program" --daemon|echo FASTPATH|' "$work/fastpath.sh"

# 1) Fresh boot: nothing unpacked yet -> must NOT take the fast path.
program="$work/opensocks"
stamp="$work/opensocks.sha256"
rm -f "$program" "$stamp"
out="$(sh -c "configured_sha='aaa'; program='$program'; stamp='$stamp'; . '$work/fastpath.sh'" 2>/dev/null || true)"
[ -z "$out" ] || fail "empty /tmp should not take the fast path (got '$out')"

# 2) Unpacked and stamped with the configured release -> fast path.
printf '#!/bin/sh\nexit 0\n' > "$program"
chmod 0755 "$program"
printf 'aaa\n' > "$stamp"
out="$(sh -c "configured_sha='aaa'; program='$program'; stamp='$stamp'; . '$work/fastpath.sh'" 2>/dev/null || true)"
[ "$out" = FASTPATH ] || fail "matching stamp should take the fast path (got '$out')"

# 3) THE BUG: binary present from the previous release, stamp names that release,
#    but the package now configures a new sha -> must NOT reuse the stale binary.
printf 'aaa\n' > "$stamp"
out="$(sh -c "configured_sha='bbb'; program='$program'; stamp='$stamp'; . '$work/fastpath.sh'" 2>/dev/null || true)"
[ -z "$out" ] || fail "a newer configured sha must not reuse the stale binary (got '$out')"

# 4) Binary present but no stamp at all (an upgrade from a release that had no
#    stamp) -> must NOT trust it, because its origin is unknown.
rm -f "$stamp"
out="$(sh -c "configured_sha='aaa'; program='$program'; stamp='$stamp'; . '$work/fastpath.sh'" 2>/dev/null || true)"
[ -z "$out" ] || fail "a binary with no stamp must not be trusted (got '$out')"

# 5) Stamped but the binary is gone -> must NOT take the fast path.
rm -f "$program"
printf 'aaa\n' > "$stamp"
out="$(sh -c "configured_sha='aaa'; program='$program'; stamp='$stamp'; . '$work/fastpath.sh'" 2>/dev/null || true)"
[ -z "$out" ] || fail "a missing binary must not take the fast path (got '$out')"

# 6) Stamped but not executable -> must NOT take the fast path.
#
# Skipped where chmod cannot remove the execute bit. On Windows/NTFS every file
# reads as executable, so `[ -x ]` is true even after chmod 0644 and the case
# cannot be expressed. OpenWrt runs on Linux, where it applies.
printf '#!/bin/sh\nexit 0\n' > "$program"
chmod 0644 "$program"
if [ -x "$program" ]; then
	printf 'SKIP case 6: this filesystem cannot clear the execute bit\n' >&2
else
	out="$(sh -c "configured_sha='aaa'; program='$program'; stamp='$stamp'; . '$work/fastpath.sh'" 2>/dev/null || true)"
	[ -z "$out" ] || fail "a non-executable binary must not take the fast path (got '$out')"
fi

# 7) The stamp must be written after a successful install. Check the shipped script
#    records it in both unpack paths.
stamp_writes="$(tr -d '\r' < "$src" | grep -c '> "\$stamp"')"
[ "$stamp_writes" -ge 2 ] || fail "expected the stamp to be written in both unpack paths, found $stamp_writes"

# 8) The unpack must not write straight into the running executable, or a restart
#    while the previous daemon exits fails with "Text file busy".
if tr -d '\r' < "$src" | grep -q 'gzip -dc "$archive" > "$program"'; then
	fail "launcher still unpacks straight into \$program (Text file busy on restart)"
fi
tr -d '\r' < "$src" | grep -q 'gzip -dc "$archive" > "$program.new"' ||
	fail "launcher should unpack into \$program.new"

printf 'PASS launcher cache: 6 decision cases + 2 static checks\n'
