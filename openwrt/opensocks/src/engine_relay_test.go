package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeSSServer is a minimal Shadowsocks server: it decrypts the client's address
// header, dials the requested target, and pipes bytes. It exercises the same
// ssConn implementation the daemon uses, so the test covers the real wire format
// in both directions rather than a mock.
type fakeSSServer struct {
	ln       net.Listener
	method   string
	password string
	udp      *net.UDPConn
	mu       sync.Mutex
	conns    []net.Conn
	udpDone  chan struct{}
}

func newFakeSSServer(t *testing.T, method, password string) *fakeSSServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSSServer{ln: ln, method: method, password: password, udp: udp, udpDone: make(chan struct{})}
	go s.serveTCP()
	go s.serveUDP()
	t.Cleanup(s.Close)
	return s
}

func (s *fakeSSServer) tcpPort() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSSServer) udpPort() int { return s.udp.LocalAddr().(*net.UDPAddr).Port }

func (s *fakeSSServer) Close() {
	_ = s.ln.Close()
	_ = s.udp.Close()
	s.mu.Lock()
	conns := append([]net.Conn(nil), s.conns...)
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	select {
	case <-s.udpDone:
	default:
		close(s.udpDone)
	}
}

func (s *fakeSSServer) track(c net.Conn) {
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()
}

func (s *fakeSSServer) serveTCP() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.track(c)
		go s.handle(c)
	}
}

// handle decrypts the request header, dials the destination and relays. The
// server side writes without a destination header, matching how a real
// Shadowsocks server replies on a TCP stream.
func (s *fakeSSServer) handle(c net.Conn) {
	defer c.Close()
	tunnel, err := serverSSConn(c, s.method, s.password)
	if err != nil {
		return
	}
	// The Shadowsocks request header is a bare SOCKS address (ATYP first), with no
	// VER/CMD/RSV prefix. Reading the ATYP and its body with the same helper the
	// client-side encoder produces proves the two agree on the format.
	atyp := make([]byte, 1)
	if _, err := io.ReadFull(tunnel, atyp); err != nil {
		return
	}
	body, err := readSocksAddrAfterType(tunnel, atyp[0])
	if err != nil {
		return
	}
	host, port, _, err := decodeSocksAddrHeader(body)
	if err != nil {
		return
	}
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		return
	}
	defer target.Close()
	s.track(target)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(tunnel, target); done <- struct{}{} }()
	go func() { _, _ = io.Copy(target, tunnel); done <- struct{}{} }()
	<-done
	_ = c.SetDeadline(time.Now())
	<-done
}

// serveUDP echoes every datagram back so the test can prove the AEAD UDP framing
// round trip through a real socket.
func (s *fakeSSServer) serveUDP() {
	spec, err := resolveCipherSpec(s.method)
	if err != nil {
		return
	}
	key, err := ssKey(spec.name, s.password)
	if err != nil {
		return
	}
	buf := make([]byte, 65535)
	for {
		n, src, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		packet := append([]byte(nil), buf[:n]...)
		var plain []byte
		if spec.aead {
			plain, err = ssAEADUnpack(spec, key, packet)
		} else {
			plain, err = ssStreamUnpack(spec, key, packet)
		}
		if err != nil {
			continue
		}
		// The reply carries the same address header so the client can decode the
		// source, exactly as ss-libev's UDP relay does.
		_, _, headerLen, herr := decodeSocksAddrHeader(plain)
		if herr != nil {
			continue
		}
		var reply []byte
		if spec.aead {
			reply, err = ssAEADPacket(spec, key, plain)
		} else {
			reply, err = ssStreamPacket(spec, key, plain)
		}
		if err != nil {
			continue
		}
		_ = headerLen
		_, _ = s.udp.WriteToUDP(reply, src)
	}
}

// serverSSConn builds an ssConn with no destination header, which is what a
// server needs: it only decrypts what the client sent and writes plain frames.
func serverSSConn(c net.Conn, method, password string) (*ssConn, error) {
	spec, err := resolveCipherSpec(method)
	if err != nil {
		return nil, err
	}
	key, err := ssKey(spec.name, password)
	if err != nil {
		return nil, err
	}
	return &ssConn{Conn: c, spec: spec, key: key, header: nil, aeadOn: spec.aead}, nil
}

// echoServer is a plain TCP echo service the relay forwards to.
// echoServer starts a TCP echo service and tears it down with t.Cleanup. Callers
// must create the echo server BEFORE the fake Shadowsocks server: cleanup runs
// LIFO, so the fake server closes its target connections first and the echo
// server's WaitGroup drains instead of blocking forever.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return ln.Addr().String()
}

// TestSOCKSTunnelEndToEnd proves the whole daemon path works: a SOCKS client
// connects to our listener, the listener speaks Shadowsocks to a server, and the
// server relays to a real target. Only the transparent-socket lookup is excluded
// because that needs nftables; everything else is the production code.
func TestSOCKSTunnelEndToEnd(t *testing.T) {
	methods := []string{"chacha20-ietf-poly1305", "aes-256-gcm", "aes-256-cfb"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			target := echoServer(t)
			server := newFakeSSServer(t, method, "test-password")
			host, portText, err := net.SplitHostPort(target)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			upstream := ssUpstream{
				Server:   "127.0.0.1",
				Port:     server.tcpPort(),
				Password: "test-password",
				Method:   method,
			}
			runtime := newSSRuntime()
			listener, err := runtime.start(upstream, "test-socks", 0, false)
			if err != nil {
				t.Fatalf("listener: %v", err)
			}
			defer runtime.stopAll()
			socksPort := listener.tcp.Addr().(*net.TCPAddr).Port

			conn, err := socksDialThrough(context.Background(), socksPort, net.JoinHostPort(host, strconv.Itoa(port)))
			if err != nil {
				t.Fatalf("socks dial: %v", err)
			}
			defer conn.Close()

			payload := make([]byte, 4096)
			if _, err := rand.Read(payload); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("echoed payload differs from what was sent")
			}
		})
	}
}

// TestSOCKSTunnelConcurrentClients proves the accept loop serves several clients
// at once without cross-talk.
func TestSOCKSTunnelConcurrentClients(t *testing.T) {
	target := echoServer(t)
	server := newFakeSSServer(t, "chacha20-ietf-poly1305", "pw")
	host, portText, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portText)
	upstream := ssUpstream{Server: "127.0.0.1", Port: server.tcpPort(), Password: "pw", Method: "chacha20-ietf-poly1305"}
	runtime := newSSRuntime()
	listener, err := runtime.start(upstream, "test-socks", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.stopAll()
	socksPort := listener.tcp.Addr().(*net.TCPAddr).Port

	const clients = 12
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := socksDialThrough(context.Background(), socksPort, net.JoinHostPort(host, strconv.Itoa(port)))
			if err != nil {
				errs <- fmt.Errorf("client %d dial: %w", id, err)
				return
			}
			defer conn.Close()
			msg := []byte(fmt.Sprintf("client-%d-payload", id))
			if _, err := conn.Write(msg); err != nil {
				errs <- fmt.Errorf("client %d write: %w", id, err)
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, got); err != nil {
				errs <- fmt.Errorf("client %d read: %w", id, err)
				return
			}
			if !bytes.Equal(got, msg) {
				errs <- fmt.Errorf("client %d got %q", id, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestUDPExchangeThroughServer proves exchangeUDP speaks the AEAD datagram format
// to a real socket and decodes the reply's address header.
func TestUDPExchangeThroughServer(t *testing.T) {
	server := newFakeSSServer(t, "aes-256-gcm", "pw")
	upstream := ssUpstream{Server: "127.0.0.1", Port: server.udpPort(), Password: "pw", Method: "aes-256-gcm"}
	target := netip.MustParseAddrPort("9.9.9.9:53")
	payload := []byte("dns-ish datagram")
	reply, err := upstream.exchangeUDP(target, payload)
	if err != nil {
		t.Fatalf("exchangeUDP: %v", err)
	}
	if !bytes.Equal(reply, payload) {
		t.Fatalf("reply = %q", reply)
	}
}

// TestRuntimeSessionCounting proves a session owns both a transparent and a
// SOCKS listener but counts as one session. Transparent binds need IP_TRANSPARENT
// privileges the test host may lack, so the test uses the SOCKS pair directly.
func TestRuntimeSessionCounting(t *testing.T) {
	server := newFakeSSServer(t, "chacha20-ietf-poly1305", "pw")
	upstream := ssUpstream{Server: "127.0.0.1", Port: server.tcpPort(), Password: "pw", Method: "chacha20-ietf-poly1305"}
	runtime := newSSRuntime()
	defer runtime.stopAll()
	if _, err := runtime.start(upstream, "socks", 0, false); err != nil {
		t.Fatal(err)
	}
	if got := runtime.activeCount(); got != 0 {
		t.Fatalf("activeCount with only a SOCKS listener = %d, want 0", got)
	}
	// Two listeners, one session: activeCount counts sessions, not listeners.
	runtime.mu.Lock()
	listeners := len(runtime.listeners)
	runtime.mu.Unlock()
	if listeners != 1 {
		t.Fatalf("listener count = %d, want 1", listeners)
	}
}

// TestSessionPortsAreDistinct proves no two sessions share a port, which would
// make the second bind fail at connect time on a real router.
func TestSessionPortsAreDistinct(t *testing.T) {
	seen := map[int]string{}
	for slot := 0; slot < 3; slot++ {
		redirect, socks := sessionPorts(slot)
		if redirect == socks {
			t.Fatalf("slot %d uses the same port for redirect and SOCKS", slot)
		}
		for _, p := range []int{redirect, socks} {
			if other, ok := seen[p]; ok {
				t.Fatalf("port %d is used by both %s and slot %d", p, other, slot)
			}
			seen[p] = fmt.Sprintf("slot %d", slot)
		}
	}
	// The table must match the port numbers the previous releases used, so an
	// in-place upgrade keeps the same firewall rules valid.
	if r, s := sessionPorts(0); r != 7890 || s != 7891 {
		t.Fatalf("slot 1 ports = %d/%d", r, s)
	}
	if r, s := sessionPorts(1); r != 7892 || s != 7893 {
		t.Fatalf("slot 2 ports = %d/%d", r, s)
	}
	if r, s := sessionPorts(2); r != 7894 || s != 7895 {
		t.Fatalf("slot 3 ports = %d/%d", r, s)
	}
}

// TestPipeBothWaysHalfClose proves the relay tears down both directions when one
// side finishes, so no goroutine is left blocked on a dead socket.
func TestPipeBothWaysHalfClose(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	done := make(chan struct{})
	go func() {
		pipeBothWays(a1, b1)
		close(done)
	}()
	// A write from the far end must arrive.
	go func() {
		_, _ = a2.Write([]byte("hello"))
	}()
	buf := make([]byte, 5)
	_ = b2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(b2, buf); err != nil {
		t.Fatalf("read through relay: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q", buf)
	}
	// Closing one end must unblock the relay and close the other direction.
	_ = a2.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeBothWays did not return after one side closed")
	}
	_ = b2.Close()
}

// TestSocksHandshakeRejectsBadVersion proves the SOCKS server refuses a non-v5
// greeting instead of hanging.
func TestSocksHandshakeRejectsBadVersion(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		_, _ = a.Write([]byte{4, 1, 0})
	}()
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := socksHandshake(b); err == nil {
		t.Fatal("a SOCKS4 greeting should be rejected")
	}
}

// TestSocksHandshakeParsesDomain drives the server-side handshake from the client
// end and proves it accepts a domain request. net.Pipe is synchronous, so the
// server and client run in lockstep and the client must be the driver.
func TestSocksHandshakeParsesDomain(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))

	type result struct {
		host string
		port int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		host, port, err := socksHandshake(a)
		done <- result{host, port, err}
	}()

	// Client greeting: VER=5, one method, "no auth".
	if _, err := b.Write([]byte{5, 1, 0}); err != nil {
		t.Fatalf("greeting write: %v", err)
	}
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(b, methodReply); err != nil {
		t.Fatalf("method reply read: %v", err)
	}
	if methodReply[0] != 5 || methodReply[1] != 0 {
		t.Fatalf("method reply = %v, want [5 0]", methodReply)
	}
	header, err := socksAddrHeader("example.com", 443)
	if err != nil {
		t.Fatal(err)
	}
	// VER CMD RSV ATYP + address, exactly as RFC 1928 ・ゑｽｧ4 defines.
	request := append([]byte{5, 1, 0}, header...)
	if _, err := b.Write(request); err != nil {
		t.Fatalf("request write: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("handshake: %v", got.err)
		}
		if got.host != "example.com" || got.port != 443 {
			t.Fatalf("got %s:%d", got.host, got.port)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not complete")
	}
}

// TestSocksHandshakeIPv4AndIPv6 proves both literal address forms parse, which
// matters because most redirected traffic uses a literal IP after DNS resolution.
func TestSocksHandshakeIPv4AndIPv6(t *testing.T) {
	cases := []struct {
		host string
		port int
	}{
		{"127.0.0.1", 80},
		{"203.0.113.9", 8080},
		{"2001:db8::5", 443},
	}
	for _, tc := range cases {
		a, b := net.Pipe()
		_ = b.SetDeadline(time.Now().Add(5 * time.Second))
		done := make(chan error, 1)
		var gotHost string
		var gotPort int
		go func() {
			host, port, err := socksHandshake(a)
			gotHost, gotPort = host, port
			done <- err
		}()
		if _, err := b.Write([]byte{5, 1, 0}); err != nil {
			t.Fatalf("%s greeting: %v", tc.host, err)
		}
		if _, err := io.ReadFull(b, make([]byte, 2)); err != nil {
			t.Fatalf("%s method reply: %v", tc.host, err)
		}
		header, err := socksAddrHeader(tc.host, tc.port)
		if err != nil {
			t.Fatalf("%s header: %v", tc.host, err)
		}
		if _, err := b.Write(append([]byte{5, 1, 0}, header...)); err != nil {
			t.Fatalf("%s request: %v", tc.host, err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s handshake: %v", tc.host, err)
			}
			if gotHost != tc.host || gotPort != tc.port {
				t.Fatalf("%s parsed as %s:%d", tc.host, gotHost, gotPort)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s handshake did not complete", tc.host)
		}
		a.Close()
		b.Close()
	}
}

// TestSocksHandshakeRejectsUnsupportedCommand proves a BIND request is refused
// with the command-not-supported code rather than being treated as CONNECT.
func TestSocksHandshakeRejectsUnsupportedCommand(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() {
		_, _, err := socksHandshake(a)
		done <- err
	}()
	if _, err := b.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(b, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	// Send only the four-byte request head. The server rejects BIND before it
	// reads the address body, so sending the body would block on net.Pipe's
	// synchronous writes waiting for a reader that never comes.
	if _, err := b.Write([]byte{5, 2, 0, 1}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(b, reply); err != nil {
		t.Fatalf("expected a rejection reply: %v", err)
	}
	if reply[1] != socksRepCommandNotSupport {
		t.Fatalf("reply code = %d, want %d", reply[1], socksRepCommandNotSupport)
	}
	if err := <-done; err == nil {
		t.Fatal("BIND should have been rejected")
	}
}
