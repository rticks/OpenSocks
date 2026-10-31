package main

import (
	"testing"
	"time"
)

// TestDisconnectSuspendsAutoRoute is the regression test for "disconnect does
// nothing". /disconnect tore the engine down, and autoRouteWatchdog - which
// reconnects whenever the engine is not running - put it back within one 10 s
// tick. Observed on a 25.12.6 router: POST /disconnect returned {"ok":true} and
// five seconds later activeSessionCount was 1 again with the nftables table
// restored.
func TestDisconnectSuspendsAutoRoute(t *testing.T) {
	c := &controller{cfg: &settings{}}

	// Nothing disconnected yet: recovery is allowed to act.
	if c.autoRouteSuspended() {
		t.Fatal("auto route must not start out suspended")
	}

	c.suspendAutoRoute()
	if !c.autoRouteSuspended() {
		t.Fatal("a manual disconnect must suspend automatic recovery")
	}
}

// TestConnectResumesAutoRoute proves an explicit connect overrides an earlier
// disconnect. Otherwise a user who reconnects from the UI would leave recovery
// disabled until the pause elapsed.
func TestConnectResumesAutoRoute(t *testing.T) {
	c := &controller{cfg: &settings{}}

	c.suspendAutoRoute()
	if !c.autoRouteSuspended() {
		t.Fatal("suspendAutoRoute did not take effect")
	}

	// connect() calls resumeAutoRoute; call it directly so the test does not need
	// a live control API.
	c.resumeAutoRoute()
	if c.autoRouteSuspended() {
		t.Fatal("resumeAutoRoute must re-enable automatic recovery")
	}
}

// TestAutoRoutePauseExpires proves the suspension is bounded. A disconnect the
// user forgot about must not disable recovery forever: after the pause the
// watchdog is allowed to act again.
func TestAutoRoutePauseExpires(t *testing.T) {
	c := &controller{cfg: &settings{}}

	c.suspendAutoRoute()
	// Age the deadline past the pause instead of sleeping for two minutes.
	c.cfgMu.Lock()
	c.autoRouteOffUntil = time.Now().Add(-time.Second)
	c.cfgMu.Unlock()

	if c.autoRouteSuspended() {
		t.Fatal("an expired pause must not keep recovery suspended")
	}
	// The flag is cleared on expiry, so the next check is also false and the
	// watchdog does not have to re-evaluate a stale deadline.
	if c.autoRouteSuspended() {
		t.Fatal("the expired suspension was not cleared")
	}
	c.cfgMu.RLock()
	stillOff := c.autoRouteOff
	c.cfgMu.RUnlock()
	if stillOff {
		t.Fatal("autoRouteOff must be cleared once the pause expires")
	}
}

// TestDisconnectStopsEngine proves disconnect actually tears the tunnel down, so
// the test above is about the watchdog and not about a no-op disconnect.
func TestDisconnectStopsEngine(t *testing.T) {
	e := newEngine()
	e.mu.Lock()
	e.connected = true
	e.upstreams = []ssUpstream{{Server: "127.0.0.1", Port: 1, Password: "pw", Method: "aes-128-gcm"}}
	e.upstreamOK = true
	e.mu.Unlock()

	// disconnect() calls refreshSettings(), which reconfigures the API client, so
	// it must be non-nil. The engine teardown is what this test asserts; the nft
	// call inside teardownRedirect simply fails where nft is absent.
	c := &controller{engine: e, cfg: &settings{}, api: newAPIClient("https://example.invalid")}
	if err := c.disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	if e.isRunning() {
		t.Fatal("disconnect left the engine running")
	}
	e.mu.Lock()
	connected := e.connected
	e.mu.Unlock()
	if connected {
		t.Fatal("disconnect left the engine marked connected")
	}
	if !c.autoRouteSuspended() {
		t.Fatal("disconnect must suspend recovery, or the watchdog reconnects")
	}
}
