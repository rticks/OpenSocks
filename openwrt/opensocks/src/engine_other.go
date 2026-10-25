//go:build !linux

package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// Non-Linux stubs keep the daemon buildable and unit-testable off OpenWrt. The
// transparent data plane only exists on Linux; the SOCKS listener still works,
// which is what local development and the tests exercise.

var errTransparentUnsupported = errors.New("transparent proxying requires Linux")

func originalDestinationTCP(net.Conn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errTransparentUnsupported
}

func listenTCP(port int, transparent bool) (net.Listener, error) {
	if transparent {
		return nil, errTransparentUnsupported
	}
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
}

func listenUDP(port int, transparent bool) (*net.UDPConn, error) {
	if transparent {
		return nil, errTransparentUnsupported
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp", addr)
}

func udpOriginalDestination([]byte, netip.AddrPort) (netip.AddrPort, bool) {
	return netip.AddrPort{}, false
}

func udpReplyConn(netip.AddrPort) (*net.UDPConn, error) {
	return nil, errTransparentUnsupported
}

func netipAddrFromIP(ip net.IP) netip.Addr {
	addr, _ := netip.AddrFromSlice(ip)
	return addr.Unmap()
}
