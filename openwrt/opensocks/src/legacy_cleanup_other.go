//go:build !linux

package main

// cleanupLegacyEngineProcesses is a no-op off OpenWrt; the previous releases
// that spawned external engines only ran on Linux routers.
func cleanupLegacyEngineProcesses() {}
