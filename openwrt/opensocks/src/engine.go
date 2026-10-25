package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// In-process Shadowsocks data plane. The daemon itself accepts the traffic
// that nftables redirects to loopback ports, so OpenSocks no longer needs the
// ss-redir / ss-local helpers that OpenWrt 25.12 dropped from its feed.
//
// Port map. Each authenticated session owns a pair of listeners: a transparent
// one that nftables redirects LAN traffic to (wildcard bind), and a loopback
// SOCKS5 one that the speed tests and the region test dial. Keeping them apart
// matters: a transparent listener resolves its destination from the socket's
// pre-redirect metadata, so dialing one as a SOCKS client would try to relay to
// itself. These are the same port pairs the previous ss-redir/ss-local pair used.
//
//	slot 1  redirect 7890 (0.0.0.0)   socks 7891 (127.0.0.1)
//	slot 2  redirect 7892 (0.0.0.0)   socks 7893 (127.0.0.1)
//	slot 3  redirect 7894 (0.0.0.0)   socks 7895 (127.0.0.1)
const (
	redirectPort     = 7890
	socksPort        = 7891
	dualPort         = 7892
	speedSocksPort   = 7893
	triplePort       = 7894
	speedTriplePort  = 7895
	ssDialTimeout    = 12 * time.Second
	ssHandshakeLimit = 20 * time.Second
	ssUDPTimeout     = 15 * time.Second
	ssMaxUDPSessions = 512
	ssUDPBufferSize  = 32 * 1024
	ssRelayBuffer    = 16 * 1024
)

// sessionPorts maps a session slot (0-based) to its redirect and SOCKS ports.
// The nftables generator uses the redirect ports, the speed pool uses the SOCKS
// ports, and both derive them from this one table so they cannot drift apart.
func sessionPorts(slot int) (redirect, socks int) {
	switch slot {
	case 1:
		return dualPort, speedSocksPort
	case 2:
		return triplePort, speedTriplePort
	default:
		return redirectPort, socksPort
	}
}

// ssUpstream is one resolved Shadowsocks endpoint plus the credentials the
// service issued for it.
type ssUpstream struct {
	Server   string
	Port     int
	Password string
	Method   string
}

func (u ssUpstream) address() string {
	return net.JoinHostPort(u.Server, strconv.Itoa(u.Port))
}

func (u ssUpstream) valid() bool {
	return u.Server != "" && u.Port > 0 && u.Port <= 65535 && u.Password != ""
}

// selectedSSBoot returns the first Shadowsocks-capable protocol variant of a
// connect response. orderedBoots puts the line's declared protocol first, so this
// prefers what the service recommended while still accepting a line whose declared
// protocol is Trojan but which also publishes an SS configuration. A Trojan-only
// line returns nil and the caller reports the unsupported protocol.
func selectedSSBoot(resp *connectResponse) *bootsInfo {
	if resp == nil {
		return nil
	}
	configs := append([]*connectConfig{resp.Config}, resp.Configs...)
	for _, cfg := range configs {
		if cfg == nil {
			continue
		}
		for _, boot := range orderedBoots(cfg) {
			if boot == nil || boot.Server == "" || boot.Port <= 0 {
				continue
			}
			if strings.EqualFold(boot.Proto, "Trojan") {
				continue
			}
			return boot
		}
	}
	return nil
}

// upstreamFromBoot converts the service's resolved configuration into an
// upstream, rejecting protocols this engine cannot speak.
func upstreamFromBoot(b *bootsInfo) (ssUpstream, error) {
	if b == nil {
		return ssUpstream{}, errors.New("no Shadowsocks configuration was returned")
	}
	if strings.EqualFold(b.Proto, "Trojan") {
		return ssUpstream{}, errors.New("this engine supports Shadowsocks lines only")
	}
	u := ssUpstream{
		Server:   strings.TrimSpace(b.Server),
		Port:     b.Port,
		Password: strings.TrimSpace(b.Password),
		Method:   defaultStr(strings.TrimSpace(b.Method), defaultSSMethod),
	}
	if !u.valid() {
		return ssUpstream{}, fmt.Errorf("server %q:%d has an incomplete Shadowsocks configuration", b.Server, b.Port)
	}
	if _, err := resolveCipherSpec(u.Method); err != nil {
		return ssUpstream{}, err
	}
	return u, nil
}

// ssListener is one bound listener plus the upstream it forwards to. Multi-
// session routing installs one listener per authenticated session.
type ssListener struct {
	upstream    ssUpstream
	label       string
	transparent bool

	tcp    net.Listener
	udp    *net.UDPConn
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ssRuntime owns every listener the daemon started.
type ssRuntime struct {
	mu        sync.Mutex
	listeners []*ssListener
	udpGuard  chan struct{}
}

func newSSRuntime() *ssRuntime {
	return &ssRuntime{udpGuard: make(chan struct{}, ssMaxUDPSessions)}
}

// startRedirect binds the transparent TCP and UDP listeners for LAN traffic.
func (r *ssRuntime) startRedirect(upstream ssUpstream, label string) (*ssListener, error) {
	return r.start(upstream, label, redirectPort, true)
}

// startSOCKS binds a loopback SOCKS5 listener on the given port.
func (r *ssRuntime) startSOCKS(upstream ssUpstream, label string, port int) (*ssListener, error) {
	return r.start(upstream, label, port, false)
}

func (r *ssRuntime) start(upstream ssUpstream, label string, port int, transparent bool) (*ssListener, error) {
	if !upstream.valid() {
		return nil, errors.New("incomplete Shadowsocks upstream")
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &ssListener{upstream: upstream, label: label, transparent: transparent, ctx: ctx, cancel: cancel}

	tcp, err := listenTCP(port, transparent)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s TCP listener on port %d failed: %w", label, port, err)
	}
	l.tcp = tcp

	udp, err := listenUDP(port, transparent)
	if err != nil {
		// UDP is best-effort: a TCP-only session still proxies HTTP, and the
		// nftables UDP chain simply finds no listener to hand datagrams to.
		logf("%s UDP listener on port %d unavailable: %v", label, port, err)
	} else {
		l.udp = udp
	}

	l.wg.Add(1)
	go l.acceptLoop()
	if l.udp != nil {
		l.wg.Add(1)
		go l.readUDPLoop(r.udpGuard)
	}
	r.mu.Lock()
	r.listeners = append(r.listeners, l)
	r.mu.Unlock()
	return l, nil
}

// stopAll closes every listener and waits for its serving goroutines.
func (r *ssRuntime) stopAll() {
	r.mu.Lock()
	listeners := r.listeners
	r.listeners = nil
	r.mu.Unlock()
	for _, l := range listeners {
		l.close()
	}
	for _, l := range listeners {
		l.wg.Wait()
	}
}

// stopLast closes the n most recently added listeners and waits for them. It is
// the rollback path when a session's second listener cannot bind.
func (r *ssRuntime) stopLast(n int) {
	r.mu.Lock()
	if n > len(r.listeners) {
		n = len(r.listeners)
	}
	removed := append([]*ssListener(nil), r.listeners[len(r.listeners)-n:]...)
	r.listeners = r.listeners[:len(r.listeners)-n]
	r.mu.Unlock()
	for _, l := range removed {
		l.close()
	}
	for _, l := range removed {
		l.wg.Wait()
	}
}

// activeCount reports how many session slots are currently bound. Each slot owns
// one transparent and one SOCKS listener, so counting the transparent ones counts
// sessions; counting all listeners would report double.
func (r *ssRuntime) activeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, l := range r.listeners {
		if l.transparent {
			count++
		}
	}
	return count
}

func (l *ssListener) close() {
	if l.cancel != nil {
		l.cancel()
	}
	if l.tcp != nil {
		_ = l.tcp.Close()
	}
	if l.udp != nil {
		_ = l.udp.Close()
	}
}

// acceptLoop serves redirected and SOCKS connections until the listener closes.
func (l *ssListener) acceptLoop() {
	defer l.wg.Done()
	for {
		conn, err := l.tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-l.ctx.Done():
				return
			default:
			}
			// Transient accept failures (EMFILE, ECONNABORTED) must not kill
			// the listener on a long-running router.
			time.Sleep(20 * time.Millisecond)
			continue
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handleTCP(conn)
		}()
	}
}

// handleTCP resolves the destination and relays the connection. Transparent
// sockets carry their original destination in the socket itself; SOCKS clients
// announce it in the handshake.
//
// For SOCKS the success reply is sent only after the upstream dial succeeded, so
// a client sees a genuine connect failure instead of a success followed by a
// silent EOF. A transparent client gets no reply at all; the redirect happened
// before the application ever wrote its request.
func (l *ssListener) handleTCP(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(ssHandshakeLimit))
	var host string
	var port int
	var err error
	if l.transparent {
		var dst netip.AddrPort
		if dst, err = tcpOriginalDestination(conn); err == nil {
			host, port = dst.Addr().String(), int(dst.Port())
		}
	} else {
		host, port, err = socksHandshake(conn)
	}
	if err != nil {
		logf("%s destination lookup failed: %v", l.label, err)
		if !l.transparent {
			socksReply(conn, socksRepHostUnreachable)
		}
		return
	}
	// Clear the handshake deadline; the relay manages its own idle deadline.
	_ = conn.SetDeadline(time.Time{})
	server, tunnel, err := dialTunnel(l.upstream, host, port)
	if err != nil {
		logf("%s upstream %s dial failed: %v", l.label, l.upstream.address(), err)
		if !l.transparent {
			socksReply(conn, socksRepConnectionRefused)
		}
		return
	}
	defer server.Close()
	if !l.transparent {
		socksReply(conn, socksRepSuccess)
	}
	pipeBothWays(conn, tunnel)
}

// readUDPLoop receives datagrams together with their pre-redirect destination.
func (l *ssListener) readUDPLoop(guard chan struct{}) {
	defer l.wg.Done()
	buf := make([]byte, ssUDPBufferSize)
	oob := make([]byte, 512)
	for {
		n, oobn, _, src, err := l.udp.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-l.ctx.Done():
				return
			default:
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if n == 0 {
			continue
		}
		target, ok := udpOriginalDestination(oob[:oobn], src)
		if !ok {
			continue
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		select {
		case guard <- struct{}{}:
		default:
			// Shed load instead of growing goroutines without bound.
			continue
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			defer func() { <-guard }()
			l.relayUDP(src, target, payload)
		}()
	}
}

// relayUDP forwards one datagram upstream and returns the reply to the client
// with the original destination restored as the source address.
func (l *ssListener) relayUDP(client, target netip.AddrPort, payload []byte) {
	reply, err := l.upstream.exchangeUDP(target, payload)
	if err != nil {
		return
	}
	if len(reply) == 0 {
		return
	}
	conn, err := udpReplyConn(target)
	if err != nil {
		logf("%s UDP reply socket for %s failed: %v", l.label, target, err)
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(ssUDPTimeout))
	if _, err := conn.WriteToUDPAddrPort(reply, client); err != nil {
		return
	}
}

// exchangeUDP encrypts one datagram, waits for the upstream reply and decrypts
// it. A fresh socket per datagram keeps no UDP state on a memory-constrained
// router and matches how ss-redir relays short game packets.
func (u ssUpstream) exchangeUDP(target netip.AddrPort, payload []byte) ([]byte, error) {
	spec, err := resolveCipherSpec(u.Method)
	if err != nil {
		return nil, err
	}
	key, err := ssKey(spec.name, u.Password)
	if err != nil {
		return nil, err
	}
	header, err := socksAddrHeader(target.Addr().String(), int(target.Port()))
	if err != nil {
		return nil, err
	}
	plain := make([]byte, 0, len(header)+len(payload))
	plain = append(plain, header...)
	plain = append(plain, payload...)
	var packet []byte
	if spec.aead {
		packet, err = ssAEADPacket(spec, key, plain)
	} else {
		packet, err = ssStreamPacket(spec, key, plain)
	}
	if err != nil {
		return nil, err
	}
	server, err := net.DialTimeout("udp", u.address(), ssDialTimeout)
	if err != nil {
		return nil, err
	}
	defer server.Close()
	_ = server.SetDeadline(time.Now().Add(ssUDPTimeout))
	if _, err := server.Write(packet); err != nil {
		return nil, err
	}
	replyBuf := make([]byte, ssUDPBufferSize)
	n, err := server.Read(replyBuf)
	if err != nil {
		return nil, err
	}
	var decoded []byte
	if spec.aead {
		decoded, err = ssAEADUnpack(spec, key, replyBuf[:n])
	} else {
		decoded, err = ssStreamUnpack(spec, key, replyBuf[:n])
	}
	if err != nil {
		return nil, err
	}
	_, _, headerLen, err := decodeSocksAddrHeader(decoded)
	if err != nil {
		return nil, err
	}
	return decoded[headerLen:], nil
}

// dialTunnel connects to the Shadowsocks server and prepares the tunnel. The
// caller owns the returned server connection and must close it.
func dialTunnel(upstream ssUpstream, host string, port int) (net.Conn, net.Conn, error) {
	server, err := net.DialTimeout("tcp", upstream.address(), ssDialTimeout)
	if err != nil {
		return nil, nil, err
	}
	tunnel, err := newSSConn(server, upstream.Method, upstream.Password, host, port)
	if err != nil {
		server.Close()
		return nil, nil, err
	}
	return server, tunnel, nil
}

// pipeBothWays copies data in both directions until either side finishes, then
// tears the other direction down so neither half-close leaks a goroutine.
func pipeBothWays(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, ssRelayBuffer)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	<-done
	_ = a.SetDeadline(time.Now())
	_ = b.SetDeadline(time.Now())
	<-done
}

// SOCKS5 reply codes (RFC 1928 ﾂｧ6).
const (
	socksRepSuccess           = 0
	socksRepConnectionRefused = 5
	socksRepHostUnreachable   = 4
	socksRepCommandNotSupport = 7
	socksRepAddrNotSupported  = 8
)

// socksHandshake performs the SOCKS5 no-auth negotiation and returns the
// requested destination. It deliberately does not send the final CONNECT reply:
// the caller sends it once the upstream tunnel is established, so the client sees
// a real failure code instead of a success followed by an immediate close.
//
// Wire layout handled here (RFC 1928 ﾂｧ3窶督ｧ4):
//
//	greeting  VER NMETHODS METHODS...
//	request   VER CMD RSV ATYP DST.ADDR DST.PORT
func socksHandshake(conn net.Conn) (string, int, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return "", 0, err
	}
	if head[0] != 5 {
		return "", 0, fmt.Errorf("unsupported SOCKS version %d", head[0])
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", 0, err
	}
	// Reply with "no authentication required" (0x00).
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return "", 0, err
	}
	// The request header is four bytes: VER, CMD, RSV, ATYP.
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return "", 0, err
	}
	if req[0] != 5 {
		socksReply(conn, socksRepHostUnreachable)
		return "", 0, fmt.Errorf("request used SOCKS version %d", req[0])
	}
	if req[1] != 1 {
		socksReply(conn, socksRepCommandNotSupport)
		return "", 0, fmt.Errorf("unsupported SOCKS command %d", req[1])
	}
	header, err := readSocksAddrAfterType(conn, req[3])
	if err != nil {
		socksReply(conn, socksRepAddrNotSupported)
		return "", 0, err
	}
	host, port, _, err := decodeSocksAddrHeader(header)
	if err != nil {
		socksReply(conn, socksRepAddrNotSupported)
		return "", 0, err
	}
	return host, port, nil
}

// readSocksAddrAfterType reads the address body for an already-consumed ATYP byte
// and returns the complete header (ATYP included) so decodeSocksAddrHeader can
// parse it.
func readSocksAddrAfterType(conn net.Conn, atyp byte) ([]byte, error) {
	switch atyp {
	case 1:
		header := make([]byte, 1+net.IPv4len+2)
		header[0] = 1
		if _, err := io.ReadFull(conn, header[1:]); err != nil {
			return nil, err
		}
		return header, nil
	case 4:
		header := make([]byte, 1+net.IPv6len+2)
		header[0] = 4
		if _, err := io.ReadFull(conn, header[1:]); err != nil {
			return nil, err
		}
		return header, nil
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return nil, err
		}
		header := make([]byte, 2+int(length[0])+2)
		header[0] = 3
		header[1] = length[0]
		if _, err := io.ReadFull(conn, header[2:]); err != nil {
			return nil, err
		}
		return header, nil
	}
	return nil, fmt.Errorf("unsupported SOCKS address type %d", atyp)
}

// socksReply sends a CONNECT reply with an all-zero bound address. Clients do not
// use the bound address for TCP CONNECT, so zeros are both valid and what most
// servers send.
func socksReply(conn net.Conn, code byte) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	_ = conn.SetWriteDeadline(time.Time{})
}

// tcpOriginalDestination recovers the destination of a redirected socket. The
// per-platform implementation lives in engine_linux.go / engine_other.go.
func tcpOriginalDestination(conn net.Conn) (netip.AddrPort, error) {
	return originalDestinationTCP(conn)
}
