//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// cleanupLegacyEngineProcesses terminates ss-redir/mihomo children left behind by
// releases up to 0.2.x. Those releases spawned an external engine and could be
// killed before reaping it, which leaves a process holding the redirect port and
// makes the next start fail with "address already in use". The in-process engine
// has no children, so this only runs once at daemon startup to clear the old
// state after an in-place upgrade.
func cleanupLegacyEngineProcesses() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	markers := []string{"ss-redir", "ss-local", "mihomo"}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		text := strings.ReplaceAll(string(cmdline), "\x00", " ")
		for _, marker := range markers {
			// Only touch processes the previous releases created: they always
			// referenced our own config path, so an unrelated user process with
			// the same binary name is never killed.
			if strings.Contains(text, marker) && strings.Contains(text, engineDir+"/config") {
				_ = syscall.Kill(pid, syscall.SIGTERM)
				logf("stopped stale %s process %d left by an earlier release", marker, pid)
				break
			}
		}
	}
}
