package main

import "runtime"

// posixFileModesEnforced reports whether the host OS actually enforces POSIX
// permission bits. Windows stores an ACL and ignores the 0600 these packages
// request, so a mode assertion there would fail for a reason that has nothing to
// do with the code under test. The daemon only ever runs on OpenWrt (Linux), so
// the permission checks matter exactly where they are meaningful.
func posixFileModesEnforced() bool {
	return runtime.GOOS != "windows"
}
