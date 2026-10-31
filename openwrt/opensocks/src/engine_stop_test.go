package main

import (
	"net"
	"strconv"
	"testing"
	"time"
)

// TestStopDropsIdleConnections is the regression test for "disconnect hangs".
//
// stopAll() used to close only the listening socket and then wait on the
// listener's WaitGroup. Connections already accepted keep running, so a single
// idle keep-alive - or a long download - kept the group non-zero and stopAll()
// never returned. Because engine.stop() holds e.mu while it waits, the whole
// control API wedged behind it: /disconnect never answered and /status hung
// with it, while the nftables table that teardownRedirect should have removed
// stayed in place.
//
// The test opens a connection through a real listener and leaves it idle, then
// asserts stopAll() returns promptly.
func TestStopDropsIdleConnections(t *testing.T) {
	server := newFakeSSServer(t, "chacha20-ietf-poly1305", "pw")
	upstream := ssUpstream{Server: "127.0.0.1", Port: server.tcpPort(), Password: "pw", Method: "chacha20-ietf-poly1305"}
	runtime := newSSRuntime()
	listener, err := runtime.start(upstream, "test-idle", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	port := listener.tcp.Addr().(*net.TCPAddr).Port

	// Connect but never send anything: the relay blocks in its handshake read
	// with a 20 s deadline, so without explicit teardown stopAll() would wait at
	// least that long (and indefinitely for a connection that stays active).
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Give the accept loop a moment to hand the connection to a relay goroutine.
	time.Sleep(150 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		runtime.stopAll()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(ssStopWait + 2*time.Second):
		t.Fatal("stopAll blocked on an idle connection; a disconnect would hang the API")
	}

	// The client connection must be closed by the teardown, not left dangling.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("client connection still open after stopAll")
	}

	if n := runtime.activeCount(); n != 0 {
		t.Fatalf("activeCount after stopAll = %d, want 0", n)
	}
}

// TestTrackRejectsAfterClose proves a connection accepted after close() cannot
// be registered and is therefore the caller's to close. Without this, a
// connection slipping in between Accept and track would never be closed by
// teardown and would hold the WaitGroup open.
func TestTrackRejectsAfterClose(t *testing.T) {
	l := &ssListener{label: "test"}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	if !l.track(a) {
		t.Fatal("track must accept a connection while the listener is open")
	}
	l.close()
	if l.track(b) {
		t.Fatal("track must refuse a connection after close")
	}
}
