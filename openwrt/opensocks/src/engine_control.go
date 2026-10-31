package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// engine is the supervisor-facing handle for the in-process Shadowsocks data
// plane. It replaces the former external ss-redir/mihomo child-process model:
// there is no binary to spawn, no config file to write and no PID to reap.
//
// The zero value is usable; newEngine prepares the runtime.
type engine struct {
	mu           sync.Mutex
	runtime      *ssRuntime
	connected    bool
	lineName     string
	lineID       int
	server       string
	servers      []string
	sessionLines []sessionLineStatus
	upstreams    []ssUpstream

	// upstreamOK records whether the last real upstream dial succeeded. A bound
	// listener only proves the daemon owns its port; it says nothing about
	// whether the Shadowsocks server answers. Without this, a router whose
	// upstream has gone away reports "Connected" forever because the listeners
	// are still bound. See engine.upstreamReachable.
	upstreamOK bool
	// upstreamCheckedAt is when upstreamOK was last refreshed by a real dial.
	upstreamCheckedAt time.Time
	// upstreamFailures counts consecutive failed probes. One refused dial is
	// usually a transient blip, so the tunnel is only declared down after this
	// reaches upstreamFailureThreshold.
	upstreamFailures int
}

type sessionLineStatus struct {
	Slot     int    `json:"slot"`
	LineID   int    `json:"lineID"`
	LineName string `json:"lineName"`
}

func newEngine() *engine {
	return &engine{runtime: newSSRuntime()}
}

// cleanupStaleEngine is retained for callers that ran older releases. The
// in-process engine leaves no child processes behind, but a router that was
// upgraded from a release that spawned ss-redir may still have one running, so
// those are terminated when found.
func cleanupStaleEngine() {
	cleanupLegacyEngineProcesses()
}

// start launches the engine for one authenticated session. The TCP redirect and
// UDP TPROXY listeners bind the shared redirect port; the SOCKS listener on
// socksPort serves the speed tests and the region test.
func (e *engine) start(conn *connectResponse, mode string, tun bool, geoipURL string) error {
	return e.startConnections(conn)
}

func (e *engine) startDual(primary, secondary *connectResponse) error {
	return e.startConnections(primary, secondary)
}

// startConnections starts one listener per authenticated session. Slot 1 owns
// the transparent redirect port; slots 2 and 3 own their own ports so nftables
// can spread flows across independent sessions.
func (e *engine) startConnections(connections ...*connectResponse) error {
	if len(connections) < 1 || len(connections) > 3 {
		return fmt.Errorf("session count must be between 1 and 3")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.connected {
		return fmt.Errorf("engine already running")
	}

	upstreams := make([]ssUpstream, 0, len(connections))
	for i, conn := range connections {
		upstream, err := upstreamFromBoot(selectedSSBoot(conn))
		if err != nil {
			return fmt.Errorf("session %d: %w", i+1, err)
		}
		upstreams = append(upstreams, upstream)
	}

	// Every session binds both halves of its port pair. The first failure rolls
	// back everything already bound so a partial start never leaves a listener
	// holding a port the next attempt needs.
	for i, upstream := range upstreams {
		redirect, socks := sessionPorts(i)
		if err := e.startListenerLocked(upstream, fmt.Sprintf("socks-%d", i+1), socks, false); err != nil {
			e.stopLocked()
			return fmt.Errorf("session %d SOCKS listener failed: %w", i+1, err)
		}
		if err := e.startListenerLocked(upstream, fmt.Sprintf("redirect-%d", i+1), redirect, true); err != nil {
			e.stopLocked()
			return fmt.Errorf("session %d redirect listener failed: %w", i+1, err)
		}
	}

	e.connected = true
	e.upstreams = upstreams
	e.lineName = connections[0].LineName
	e.lineID = connections[0].LineID
	e.server = upstreams[0].Server
	e.servers = nil
	e.sessionLines = nil
	for i, conn := range connections {
		e.servers = append(e.servers, upstreams[i].Server)
		e.sessionLines = append(e.sessionLines, sessionLineStatus{Slot: i + 1, LineID: conn.LineID, LineName: conn.LineName})
	}
	// The control API just issued this session, so treat the upstream as working
	// until probes say otherwise. The supervisor re-probes every tick and clears
	// this after enough consecutive failures that the UI stops claiming Connected.
	e.upstreamOK = true
	e.upstreamCheckedAt = time.Time{}
	e.upstreamFailures = 0
	return nil
}

func (e *engine) startListenerLocked(upstream ssUpstream, label string, port int, transparent bool) error {
	_, err := e.runtime.start(upstream, label, port, transparent)
	return err
}

// stop closes every listener and clears the session state.
func (e *engine) stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
}

func (e *engine) stopLocked() {
	if e.runtime != nil {
		e.runtime.stopAll()
	}
	e.connected = false
	e.upstreams = nil
	e.lineName = ""
	e.lineID = 0
	e.server = ""
	e.servers = nil
	e.sessionLines = nil
	// The next connect has to prove its own upstream; a result cached against the
	// previous server would otherwise leak into the new session.
	e.upstreamOK = false
	e.upstreamCheckedAt = time.Time{}
	e.upstreamFailures = 0
}

// isRunning reports whether the engine owns its listeners. It deliberately says
// nothing about the upstream: a bound listener stays bound long after the
// Shadowsocks server has stopped answering, so "listeners bound" is not the same
// question as "the tunnel works". Callers that show connection state to a user
// must combine this with upstreamReachable.
func (e *engine) isRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.connected && e.runtime.activeCount() > 0
}

// upstreamProbeTTL bounds how often the upstream is dialled. The supervisor ticks
// every 30 s and status() reads the cached verdict, so polling LuCI does not open
// a connection per request.
const upstreamProbeTTL = 25 * time.Second

// upstreamProbeTimeout is short on purpose: this only has to prove the
// Shadowsocks server accepts a TCP connection. A slow answer is a failure for the
// purpose of telling a user whether the tunnel works.
const upstreamProbeTimeout = 5 * time.Second

// upstreamFailureThreshold is how many consecutive failed probes mark the tunnel
// down. The supervisor probes every 30 s, so this tolerates roughly a minute of
// loss before the UI stops claiming Connected — long enough to ride out a blip,
// short enough that a user is not misled for minutes.
const upstreamFailureThreshold = 2

// probeUpstream dials every configured upstream and reports whether at least one
// accepted the connection, caching the verdict in upstreamOK. The dial is a plain
// TCP connect to the server port: the Shadowsocks handshake only happens once a
// client asks for a destination, so there is nothing more to verify without
// sending real traffic.
//
// The dial runs without holding e.mu so a 5 s timeout cannot block status() or the
// supervisor. It is driven by the supervisor ticker and by connect(), not by
// status(): LuCI polls status every few seconds and must never wait on a dial.
func (e *engine) probeUpstream() bool {
	e.mu.Lock()
	if !e.connected || len(e.upstreams) == 0 {
		e.mu.Unlock()
		return false
	}
	if !e.upstreamCheckedAt.IsZero() && time.Since(e.upstreamCheckedAt) < upstreamProbeTTL {
		ok := e.upstreamOK
		e.mu.Unlock()
		return ok
	}
	addresses := make([]string, 0, len(e.upstreams))
	for _, u := range e.upstreams {
		addresses = append(addresses, u.address())
	}
	e.mu.Unlock()

	reachable := false
	for _, address := range addresses {
		conn, err := net.DialTimeout("tcp", address, upstreamProbeTimeout)
		if err != nil {
			logf("upstream %s unreachable: %v", address, err)
			continue
		}
		_ = conn.Close()
		reachable = true
		break
	}

	e.mu.Lock()
	e.upstreamCheckedAt = time.Now()
	if reachable {
		e.upstreamFailures = 0
		e.upstreamOK = true
	} else {
		e.upstreamFailures++
		// A single refused dial is usually a blip; keep reporting the tunnel as up
		// until the failures are consecutive enough to be a real outage.
		if e.upstreamFailures >= upstreamFailureThreshold {
			e.upstreamOK = false
		}
	}
	verdict := e.upstreamOK
	e.mu.Unlock()
	return verdict
}

// upstreamReachable reports whether the tunnel is usable: the listeners are bound
// and the last probe found an answering upstream. This is what "Connected" must
// mean. isRunning() alone is not enough — a bound listener survives a dead
// upstream, which is how a router with an expired account kept reporting
// "Connected" while nothing worked.
func (e *engine) upstreamReachable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.connected || e.runtime.activeCount() == 0 {
		return false
	}
	// Before the first probe completes, trust the successful connect: the control
	// API just issued this session. The supervisor corrects it within one tick.
	if e.upstreamCheckedAt.IsZero() {
		return e.upstreamOK
	}
	return e.upstreamOK
}

func (e *engine) activeSessionCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.connected {
		return 0
	}
	return e.runtime.activeCount()
}

// lineInfo returns the connected line's identity. The fields are guarded by e.mu,
// so callers must not read e.lineID / e.lineName directly from another goroutine.
func (e *engine) lineInfo() (int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lineID, e.lineName
}

// serverAddresses returns every upstream server address in use so nftables can
// exempt them from redirection.
func (e *engine) serverAddresses() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.servers...)
}

func (e *engine) sessionLineStatuses() []sessionLineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sessionLineStatus(nil), e.sessionLines...)
}

// supervise watches the listeners and the upstream. Unlike the child-process
// engine there is no process to poll; the listeners either serve or report a
// closed socket. A closed listener while connected means the runtime was torn
// down unexpectedly, so request a restart.
//
// It also dials the upstream on every tick. A bound listener survives a dead
// upstream, so without this the daemon keeps reporting "Connected" while no
// packet can leave the router. That is the difference the user sees as "it says
// connected but nothing works".
func (e *engine) supervise(onRestart func()) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		e.mu.Lock()
		connected := e.connected
		running := connected && e.runtime.activeCount() > 0
		e.mu.Unlock()
		if connected && !running {
			logf("engine listeners stopped while connected, requesting restart")
			onRestart()
			continue
		}
		if !connected {
			continue
		}
		if !e.probeUpstream() {
			// The upstream is gone but the listeners are still bound. Report it
			// so the UI stops claiming Connected, and let the controller's
			// autoRoute watchdog decide whether to reconnect.
			logf("upstream is unreachable; reporting the engine as disconnected")
		}
	}
}

// activeEngine is the daemon-wide engine handle. Speed tests and the region test
// reach it to decide whether a China route is available without threading a
// controller pointer through every helper.
var activeEngine atomic.Pointer[engine]

// activeEngineStartExtra binds an additional session's listener pair for the
// temporary third speed-test session. It returns an error when the daemon is not
// connected, so the caller can fall back to fewer sessions.
func activeEngineStartExtra(upstream ssUpstream, label string, redirect, socks int) error {
	e := activeEngine.Load()
	if e == nil {
		return errors.New("engine is not initialised")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.connected {
		return errors.New("engine is not connected")
	}
	if _, err := e.runtime.start(upstream, label+"-socks", socks, false); err != nil {
		return err
	}
	if _, err := e.runtime.start(upstream, label, redirect, true); err != nil {
		// Roll back the SOCKS half so the port is free for the next attempt.
		e.runtime.stopLast(1)
		return err
	}
	return nil
}

// engineRunning reports whether a session is currently connected.
func engineRunning() bool {
	e := activeEngine.Load()
	return e != nil && e.isRunning()
}

// engineSessionCount reports how many authenticated sessions are bound.
func engineSessionCount() int {
	e := activeEngine.Load()
	if e == nil {
		return 0
	}
	return e.activeSessionCount()
}

// socksDialThrough dials address through the daemon's own SOCKS listener. The
// speed tests and the region test use this so their traffic follows the same
// China route as LAN clients, without spawning helper processes.
func socksDialThrough(ctx context.Context, port int, address string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: ssDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(ssHandshakeLimit)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		conn.Close()
		return nil, err
	}
	portNumber, err := strconv.Atoi(portText)
	if err != nil {
		conn.Close()
		return nil, err
	}
	header, err := socksAddrHeader(host, portNumber)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Method negotiation: one no-auth method.
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		conn.Close()
		return nil, err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		conn.Close()
		return nil, err
	}
	if reply[0] != 5 || reply[1] != 0 {
		conn.Close()
		return nil, errors.New("SOCKS negotiation was rejected")
	}
	request := make([]byte, 0, 3+len(header))
	request = append(request, 5, 1, 0)
	request = append(request, header...)
	if _, err := conn.Write(request); err != nil {
		conn.Close()
		return nil, err
	}
	// CONNECT reply: VER REP RSV ATYP BND.ADDR BND.PORT. The bound address is
	// informational and often all-zero, so it is read and discarded.
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		conn.Close()
		return nil, err
	}
	if head[0] != 5 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS reply used version %d", head[0])
	}
	if head[1] != socksRepSuccess {
		conn.Close()
		return nil, fmt.Errorf("SOCKS connect failed with code %d", head[1])
	}
	skip := 0
	switch head[3] {
	case 1:
		skip = net.IPv4len + 2
	case 4:
		skip = net.IPv6len + 2
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			conn.Close()
			return nil, err
		}
		skip = int(length[0]) + 2
	default:
		conn.Close()
		return nil, errors.New("SOCKS reply used an unsupported address type")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip)); err != nil {
		conn.Close()
		return nil, err
	}
	// Clear the deadline: leaving the handshake deadline armed would abort every
	// transfer that outlives it.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
