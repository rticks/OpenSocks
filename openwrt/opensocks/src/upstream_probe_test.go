package main

import (
	"net"
	"testing"
	"time"
)

// deadUpstreamPort returns a port that nothing listens on. It binds a listener,
// records the port, then closes it, so the address is well formed but refuses
// connections. This is what an expired account's server looks like from the
// router: the port simply does not answer.
func deadUpstreamPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// expireProbeCache forces the next probeUpstream call to dial instead of using a
// cached verdict.
func expireProbeCache(e *engine) {
	e.mu.Lock()
	e.upstreamCheckedAt = time.Now().Add(-2 * upstreamProbeTTL)
	e.mu.Unlock()
}

// TestProbeUpstreamReportsDeadServer is the regression test for "the UI says
// Connected while nothing works". A bound listener used to be the whole answer,
// so a router whose upstream had stopped answering kept reporting Connected. The
// probe must return false once the failures are consecutive.
func TestProbeUpstreamReportsDeadServer(t *testing.T) {
	e := newEngine()
	e.mu.Lock()
	e.connected = true
	e.upstreams = []ssUpstream{{Server: "127.0.0.1", Port: deadUpstreamPort(t), Password: "pw", Method: "aes-128-gcm"}}
	e.mu.Unlock()

	// The first failure may be a blip and is tolerated; the threshold decides.
	for i := 0; i < upstreamFailureThreshold; i++ {
		expireProbeCache(e)
		if e.probeUpstream() {
			t.Fatalf("probeUpstream reported a dead upstream as reachable on attempt %d", i+1)
		}
	}
	e.mu.Lock()
	ok := e.upstreamOK
	checked := !e.upstreamCheckedAt.IsZero()
	e.mu.Unlock()
	if ok {
		t.Fatal("upstreamOK stayed true after consecutive failed probes")
	}
	if !checked {
		t.Fatal("upstreamCheckedAt was not recorded")
	}
}

// TestSingleProbeFailureIsTolerated proves one refused dial does not tear a working
// tunnel down. The supervisor probes every 30 s, so a single packet-loss blip must
// not make the UI flap to Disconnected.
func TestSingleProbeFailureIsTolerated(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	e := newEngine()
	e.mu.Lock()
	e.connected = true
	e.upstreams = []ssUpstream{{Server: "127.0.0.1", Port: port, Password: "pw", Method: "aes-128-gcm"}}
	e.mu.Unlock()

	if !e.probeUpstream() {
		t.Fatal("first probe should succeed")
	}
	// Kill the server, then let exactly one probe fail.
	_ = ln.Close()
	expireProbeCache(e)
	if !e.probeUpstream() {
		t.Fatal("a single failed probe should not mark the tunnel down yet")
	}
	if e.upstreamFailures != 1 {
		t.Fatalf("upstreamFailures = %d, want 1", e.upstreamFailures)
	}
	// A second consecutive failure is an outage.
	expireProbeCache(e)
	if e.probeUpstream() {
		t.Fatal("the second consecutive failure should mark the tunnel down")
	}
}

// TestProbeUpstreamReportsLiveServer is the positive half: when the port accepts a
// connection the probe must say so, otherwise the fix would report every healthy
// tunnel as down.
func TestProbeUpstreamReportsLiveServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Accept in the background so the dial's connect() completes.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	e := newEngine()
	e.mu.Lock()
	e.connected = true
	e.upstreams = []ssUpstream{{Server: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Password: "pw", Method: "aes-128-gcm"}}
	e.mu.Unlock()

	if !e.probeUpstream() {
		t.Fatal("probeUpstream reported a live upstream as unreachable")
	}
}

// TestUpstreamReachableNeedsBothHalves proves the two conditions are ANDed: a live
// upstream with no bound listener is not "connected", and neither is a bound
// listener with a dead upstream.
func TestUpstreamReachableNeedsBothHalves(t *testing.T) {
	e := newEngine()

	// No listeners and no upstream yet.
	if e.upstreamReachable() {
		t.Fatal("upstreamReachable was true with no listeners bound")
	}

	// Connected with a dead upstream: the listeners exist but the server does not.
	e.mu.Lock()
	e.connected = true
	e.upstreamOK = false
	e.upstreamCheckedAt = time.Now()
	e.upstreams = []ssUpstream{{Server: "127.0.0.1", Port: deadUpstreamPort(t), Password: "pw", Method: "aes-128-gcm"}}
	e.mu.Unlock()
	if e.upstreamReachable() {
		t.Fatal("upstreamReachable was true with a dead upstream")
	}
}

// TestStopClearsUpstreamVerdict proves a cached verdict cannot leak into the next
// session. Otherwise reconnecting to a different server would inherit the previous
// server's reachability until the next probe tick.
func TestStopClearsUpstreamVerdict(t *testing.T) {
	e := newEngine()
	e.mu.Lock()
	e.connected = true
	e.upstreamOK = true
	e.upstreamCheckedAt = time.Now()
	e.upstreams = []ssUpstream{{Server: "127.0.0.1", Port: 1, Password: "pw", Method: "aes-128-gcm"}}
	e.mu.Unlock()

	e.stop()

	e.mu.Lock()
	ok, checked := e.upstreamOK, !e.upstreamCheckedAt.IsZero()
	e.mu.Unlock()
	if ok || checked {
		t.Fatalf("stop left a stale upstream verdict: upstreamOK=%v checked=%v", ok, checked)
	}
	if e.upstreamReachable() {
		t.Fatal("upstreamReachable was true after stop")
	}
}

// TestProbeUpstreamCachesWithinTTL proves the probe does not dial on every status
// poll. LuCI asks for status every few seconds, and each probe is a real TCP
// connection, so a cached verdict has to be reused until the TTL expires.
func TestProbeUpstreamCachesWithinTTL(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	accepted := make(chan struct{}, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	e := newEngine()
	e.mu.Lock()
	e.connected = true
	e.upstreams = []ssUpstream{{Server: "127.0.0.1", Port: port, Password: "pw", Method: "aes-128-gcm"}}
	e.mu.Unlock()

	if !e.probeUpstream() {
		t.Fatal("first probe should succeed")
	}
	<-accepted
	// Close the listener so a second real dial would fail; the cached verdict must
	// still say reachable within the TTL.
	_ = ln.Close()

	if !e.probeUpstream() {
		t.Fatal("probeUpstream re-dialled inside the TTL instead of using the cache")
	}
	select {
	case <-accepted:
		t.Fatal("probeUpstream opened a second connection inside the TTL")
	case <-time.After(150 * time.Millisecond):
	}

	// Expire the cache and confirm the dead server is eventually reported. One
	// failure is tolerated as a blip, so this needs the full threshold.
	for i := 0; i < upstreamFailureThreshold; i++ {
		expireProbeCache(e)
		verdict := e.probeUpstream()
		if i < upstreamFailureThreshold-1 && !verdict {
			t.Fatalf("failure %d of %d marked the tunnel down too early", i+1, upstreamFailureThreshold)
		}
		if i == upstreamFailureThreshold-1 && verdict {
			t.Fatal("probeUpstream still reported reachable after consecutive failures")
		}
	}
}
