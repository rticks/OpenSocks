package main

// Runtime paths and the port the transparent chain redirects to. Runtime assets
// stay in tmpfs: typical OpenWrt targets have too little writable flash to keep
// a China CIDR list or logs on the overlay filesystem.
var engineDir = envOr("OPENSOCKS_DIR", "/tmp/opensocks")

// The redirect and SOCKS ports are declared in engine.go, next to the listeners
// that bind them, and the nftables generator reads them through sessionPorts so
// the firewall rules and the engine can never disagree.
