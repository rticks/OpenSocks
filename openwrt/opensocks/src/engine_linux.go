//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux transparent-proxy plumbing. Everything here is only reachable on
// OpenWrt: TCP REDIRECT leaves the pre-rewrite destination in the socket and
// UDP TPROXY attaches the original destination as an IP_RECVORIGDSTADDR control
// message on every datagram.

// soOriginalDst is the netfilter REDIRECT socket option. The value is stable in
// the kernel UAPI (include/uapi/linux/netfilter_ipv4.h) across architectures.
const soOriginalDst = 80

// ip6tSoOriginalDst is the IPv6 equivalent (netfilter_ipv6.h).
const ip6tSoOriginalDst = 80

// originalDestinationTCP returns the destination the client originally
// connected to, before nftables REDIRECT rewrote it to our listener.
//
// It reads the socket option through SyscallConn rather than net.TCPConn.File():
// File() duplicates the descriptor and takes the connection off Go's netpoller,
// which silently disables SetDeadline on the live connection. A relay that later
// cleared its handshake deadline would keep the 20-second one armed and drop
// every long-lived tunnel.
func originalDestinationTCP(conn net.Conn) (netip.AddrPort, error) {
	raw, ok := conn.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, errors.New("connection is not TCP")
	}
	sc, err := raw.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var dst netip.AddrPort
	var sockErr error
	if cerr := sc.Control(func(fd uintptr) {
		dst, sockErr = originalDestinationFD(int(fd))
	}); cerr != nil {
		return netip.AddrPort{}, cerr
	}
	if sockErr != nil {
		return netip.AddrPort{}, sockErr
	}
	return dst, nil
}

// originalDestinationFD reads IP_ORIGDST / IP6T_SO_ORIGINAL_DST from fd.
func originalDestinationFD(fd int) (netip.AddrPort, error) {
	var sockaddr unix.RawSockaddrInet4
	size := uint32(unsafe.Sizeof(sockaddr))
	_, _, errno := syscall.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), uintptr(unix.SOL_IP),
		uintptr(soOriginalDst), uintptr(unsafe.Pointer(&sockaddr)), uintptr(unsafe.Pointer(&size)), 0)
	if errno == 0 {
		port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&sockaddr.Port))[:])
		ip := net.IPv4(sockaddr.Addr[0], sockaddr.Addr[1], sockaddr.Addr[2], sockaddr.Addr[3])
		return netip.AddrPortFrom(netipAddrFromIP(ip), port), nil
	}
	firstErr := errno
	if v6, err := originalDestination6(fd); err == nil {
		return v6, nil
	}
	return netip.AddrPort{}, fmt.Errorf("SO_ORIGINAL_DST unavailable: %w", firstErr)
}

// originalDestination6 reads IP6T_SO_ORIGINAL_DST for an IPv6 REDIRECT socket.
func originalDestination6(fd int) (netip.AddrPort, error) {
	var sockaddr unix.RawSockaddrInet6
	size := uint32(unsafe.Sizeof(sockaddr))
	_, _, errno := syscall.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), uintptr(unix.SOL_IPV6),
		uintptr(ip6tSoOriginalDst), uintptr(unsafe.Pointer(&sockaddr)), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 {
		return netip.AddrPort{}, errno
	}
	port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&sockaddr.Port))[:])
	ip := make(net.IP, net.IPv6len)
	copy(ip, sockaddr.Addr[:])
	return netip.AddrPortFrom(netipAddrFromIP(ip), port), nil
}

// listenTCP binds a listener for redirected traffic. A transparent listener
// must bind the wildcard address so the kernel delivers packets whose
// destination is an arbitrary remote host.
func listenTCP(port int, transparent bool) (net.Listener, error) {
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	if transparent {
		address = net.JoinHostPort("0.0.0.0", fmt.Sprint(port))
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, err
	}
	if transparent {
		if err := setTransparent(listener); err != nil {
			listener.Close()
			return nil, err
		}
	}
	return listener, nil
}

// listenUDP binds the datagram listener used by UDP TPROXY.
func listenUDP(port int, transparent bool) (*net.UDPConn, error) {
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	if transparent {
		address = net.JoinHostPort("0.0.0.0", fmt.Sprint(port))
	}
	addr, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	if transparent {
		if err := setTransparentUDP(conn); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// setTransparent enables IP_TRANSPARENT on a TCP listener so the kernel accepts
// connections addressed to a foreign local address.
func setTransparent(listener net.Listener) error {
	raw, ok := listener.(*net.TCPListener)
	if !ok {
		return errors.New("listener is not TCP")
	}
	sc, err := raw.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if cerr := sc.Control(func(fd uintptr) {
		if e := unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_TRANSPARENT, 1); e != nil {
			sockErr = fmt.Errorf("IP_TRANSPARENT: %w", e)
		}
	}); cerr != nil {
		return cerr
	}
	return sockErr
}

// setTransparentUDP enables IP_TRANSPARENT and IP_RECVORIGDSTADDR so each
// received datagram carries its pre-redirect destination.
func setTransparentUDP(conn *net.UDPConn) error {
	sc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if cerr := sc.Control(func(fd uintptr) {
		if e := unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_TRANSPARENT, 1); e != nil {
			sockErr = fmt.Errorf("IP_TRANSPARENT: %w", e)
			return
		}
		if e := unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_RECVORIGDSTADDR, 1); e != nil {
			sockErr = fmt.Errorf("IP_RECVORIGDSTADDR: %w", e)
		}
	}); cerr != nil {
		return cerr
	}
	return sockErr
}

// udpOriginalDestination extracts the original destination from the control
// message buffer that ReadMsgUDPAddrPort fills. A transparent listener always
// receives it; when the socket option is unavailable the datagram's own source
// address is the best remaining hint, so fall back to it only when it looks
// like a plausible target.
func udpOriginalDestination(oob []byte, src netip.AddrPort) (netip.AddrPort, bool) {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err == nil {
		for _, message := range messages {
			if message.Header.Level != unix.SOL_IP || message.Header.Type != unix.IP_RECVORIGDSTADDR {
				continue
			}
			if len(message.Data) < 8 {
				continue
			}
			port := binary.BigEndian.Uint16(message.Data[2:4])
			ip := net.IPv4(message.Data[4], message.Data[5], message.Data[6], message.Data[7])
			return netip.AddrPortFrom(netipAddrFromIP(ip), port), true
		}
	}
	_ = src
	return netip.AddrPort{}, false
}

// udpReplyConn creates a socket bound to the original destination so a reply
// appears to come from the address the client sent to. Without it the client's
// connected socket rejects the reply. This mirrors what ss-redir does.
func udpReplyConn(target netip.AddrPort) (*net.UDPConn, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = unix.Close(fd) }
	if err := unix.SetsockoptInt(fd, unix.SOL_IP, unix.IP_TRANSPARENT, 1); err != nil {
		cleanup()
		return nil, fmt.Errorf("reply socket IP_TRANSPARENT: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		cleanup()
		return nil, fmt.Errorf("reply socket SO_REUSEADDR: %w", err)
	}
	sockaddr := &unix.SockaddrInet4{Port: int(target.Port())}
	ip4 := target.Addr().As4()
	copy(sockaddr.Addr[:], ip4[:])
	if err := unix.Bind(fd, sockaddr); err != nil {
		cleanup()
		return nil, fmt.Errorf("reply socket bind %s: %w", target, err)
	}
	file := os.NewFile(uintptr(fd), "opensocks-udp-reply")
	conn, err := net.FilePacketConn(file)
	file.Close()
	if err != nil {
		return nil, err
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		conn.Close()
		return nil, errors.New("reply socket is not UDP")
	}
	return udp, nil
}

// netipAddrFromIP converts net.IP to netip.Addr, unmapping IPv4-in-IPv6.
func netipAddrFromIP(ip net.IP) netip.Addr {
	addr, _ := netip.AddrFromSlice(ip)
	return addr.Unmap()
}
