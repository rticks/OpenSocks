//go:build !linux

package main

// observeDNSAnswers is a no-op off OpenWrt. The sniffer needs an AF_PACKET raw
// socket and the router's LAN device, neither of which exists on a development
// host; the parsing helpers are still compiled and unit-tested.
func observeDNSAnswers() {}
