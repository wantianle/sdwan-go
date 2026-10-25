package core

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	protocol "sdwan-go/pkg/protocol"
)

// ---------------------------------------------------------------------------
// connectWithRetry: bounded startup retry, auth rejection is terminal.
// ---------------------------------------------------------------------------

func TestConnectWithRetryBoundedOnPersistentTransientFailure(t *testing.T) {
	var connects, handshakes int
	_, err := connectWithRetry(
		func() error { connects++; return nil },
		func() ([]byte, error) {
			handshakes++
			return nil, errors.New("read OPENACK: i/o timeout")
		},
		3, time.Millisecond,
	)
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if connects != 3 || handshakes != 3 {
		t.Fatalf("connects=%d handshakes=%d, want 3/3 (bounded retry)", connects, handshakes)
	}
}

func TestConnectWithRetryRetriesConnectFailure(t *testing.T) {
	var connects, handshakes int
	_, err := connectWithRetry(
		func() error {
			connects++
			if connects < 3 {
				return errors.New("resolve server: no such host")
			}
			return nil
		},
		func() ([]byte, error) { handshakes++; return []byte("ack"), nil },
		3, time.Millisecond,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if connects != 3 || handshakes != 1 {
		t.Fatalf("connects=%d handshakes=%d, want 3/1", connects, handshakes)
	}
}

func TestConnectWithRetrySucceedsAfterTransientHandshakeFailure(t *testing.T) {
	var handshakes int
	raw, err := connectWithRetry(
		func() error { return nil },
		func() ([]byte, error) {
			handshakes++
			if handshakes == 1 {
				return nil, errors.New("read OPENACK: i/o timeout")
			}
			return []byte("ack"), nil
		},
		3, time.Millisecond,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(raw) != "ack" || handshakes != 2 {
		t.Fatalf("raw=%q handshakes=%d, want ack/2", raw, handshakes)
	}
}

func TestConnectWithRetryDoesNotRetryAuthRejection(t *testing.T) {
	var connects, handshakes int
	_, err := connectWithRetry(
		func() error { connects++; return nil },
		func() ([]byte, error) {
			handshakes++
			return nil, errors.New("peer AUTH REJECTED")
		},
		3, time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "AUTH REJECTED") {
		t.Fatalf("err = %v, want AUTH REJECTED", err)
	}
	if connects != 1 || handshakes != 1 {
		t.Fatalf("connects=%d handshakes=%d, want 1/1 (no retry on auth rejection)", connects, handshakes)
	}
}

// ---------------------------------------------------------------------------
// RunOnce: auth rejection must not retry.
// ---------------------------------------------------------------------------

func TestRunOnceDoesNotRetryAuthRejection(t *testing.T) {
	srv := newFakeOpenACKServer(t)
	srv.reject.Store(true)

	cfgPath := writeRunOnceConfig(t, srv.port())
	err := RunOnce(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "AUTH REJECTED") {
		t.Fatalf("RunOnce err = %v, want AUTH REJECTED", err)
	}
	if got := srv.opens.Load(); got != 1 {
		t.Fatalf("OPEN attempts = %d, want 1 (auth rejection must not retry)", got)
	}
}

// ---------------------------------------------------------------------------
// RunOnce: transient session failure during the running phase must self-heal
// instead of returning/exiting.
// ---------------------------------------------------------------------------

// TestRunOnceRunningPhaseSelfHealsOnSessionFailure drives the exact running
// phase invoked by RunOnce (runTunnelPhase -> Client.Start) against a reachable
// fake server, injects a session failure, and asserts RunOnce neither returns
// nor loses the tunnel: the reconnect worker establishes a fresh session.
// setupTUN and the signal source are stubbed so the test needs no
// CAP_NET_ADMIN and no process-wide signal.
func TestRunOnceRunningPhaseSelfHealsOnSessionFailure(t *testing.T) {
	srv := newFakeOpenACKServer(t)

	cfgPath := writeRunOnceConfig(t, srv.port())
	sigCh := make(chan os.Signal, 1)

	var mu sync.Mutex
	var client *Client
	withRunOnceSeams(t,
		func(_ *Config, _ *protocol.OPENACKResult, c *Client) (string, func(), error) {
			mu.Lock()
			client = c
			mu.Unlock()
			c.startDelay = 0 // skip the 3s protocol settling delay in tests
			return "test0", func() {}, nil
		},
		func() (<-chan os.Signal, func()) { return sigCh, func() {} },
	)

	errCh := make(chan error, 1)
	go func() { errCh <- RunOnce(cfgPath) }()

	// Wait for the initial handshake and the reconnect worker to be live.
	waitForCondition(t, 5*time.Second, func() bool {
		mu.Lock()
		c := client
		mu.Unlock()
		return c != nil && c.reconnectStarted.Load() && srv.opens.Load() >= 1
	})

	mu.Lock()
	c := client
	mu.Unlock()

	// Simulate a transient UDP session failure (the same path a read/write
	// timeout takes) while RunOnce is in its running phase.
	c.failSession(c.currentSession(), errors.New("simulated transient UDP timeout"))

	// Self-heal: a second handshake reaches the reachable server and a new
	// session is active.
	waitForCondition(t, 5*time.Second, func() bool { return srv.opens.Load() >= 2 })
	waitForCondition(t, 5*time.Second, func() bool { return c.currentSession() != nil })

	// RunOnce must still be blocked in its running phase (not returned/exit).
	select {
	case err := <-errCh:
		t.Fatalf("RunOnce returned after transient session failure: %v", err)
	default:
	}

	// Graceful shutdown: a signal must unblock RunOnce cleanly.
	sigCh <- syscall.SIGTERM
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunOnce returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not return after shutdown signal")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func withRunOnceSeams(t *testing.T,
	setup func(*Config, *protocol.OPENACKResult, *Client) (string, func(), error),
	signals func() (<-chan os.Signal, func()),
) {
	t.Helper()
	oldSetup, oldSignals := runOnceSetupTUN, runOnceSignals
	runOnceSetupTUN = setup
	runOnceSignals = signals
	t.Cleanup(func() {
		runOnceSetupTUN = oldSetup
		runOnceSignals = oldSignals
	})
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not satisfied within timeout")
}

func writeRunOnceConfig(t *testing.T, port int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "iwan.conf")
	content := fmt.Sprintf(
		"server=127.0.0.1\nport=%d\nusername=u\npassword=p\nmtu=1400\ntunname=test0\nroutenet=192.168.0.0/16\n",
		port)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeOpenACKServer answers OPEN handshakes with a valid, fixed OPENACK. It is
// intentionally free of TUN/route dependencies.
type fakeOpenACKServer struct {
	conn   *net.UDPConn
	opens  atomic.Int32
	reject atomic.Bool
	wg     sync.WaitGroup
}

func newFakeOpenACKServer(t *testing.T) *fakeOpenACKServer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen fake server: %v", err)
	}
	s := &fakeOpenACKServer{conn: conn}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

func (s *fakeOpenACKServer) port() int {
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}

func (s *fakeOpenACKServer) Close() {
	_ = s.conn.Close()
	s.wg.Wait()
}

func (s *fakeOpenACKServer) serve() {
	defer s.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, raddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 1 || protocol.MsgType(buf[:n]) != protocol.MsgOPEN {
			continue
		}
		s.opens.Add(1)
		if s.reject.Load() {
			// 24 bytes so Session.Handshake reaches the message-type check
			// (shorter packets are ignored as non-OPENACK input).
			reject := make([]byte, 24)
			reject[0] = 0x11
			_, _ = s.conn.WriteToUDP(reject, raddr)
			continue
		}
		_, _ = s.conn.WriteToUDP(buildTestOPENACK(), raddr)
	}
}

// buildTestOPENACK returns a signed OPENACK with MTU 1400, LocalIP 10.100.100.7
// and GatewayIP 10.100.100.1.
func buildTestOPENACK() []byte {
	buf := make([]byte, 24+4+6+10)
	buf[0] = protocol.MsgOPENACK
	binary.BigEndian.PutUint16(buf[2:4], 1)
	binary.BigEndian.PutUint32(buf[4:8], 1)

	pos := 24
	// MTU TLV (type 3, len 4)
	buf[pos] = 0x03
	buf[pos+1] = 0x04
	binary.BigEndian.PutUint16(buf[pos+2:pos+4], 1400)
	pos += 4
	// LocalIP TLV (type 4, len 6)
	buf[pos] = 0x04
	buf[pos+1] = 0x06
	copy(buf[pos+2:pos+6], []byte{10, 100, 100, 7})
	pos += 6
	// GatewayIP TLV (type 5, len 10)
	buf[pos] = 0x05
	buf[pos+1] = 0x0A
	copy(buf[pos+2:pos+6], []byte{10, 100, 100, 1})
	pos += 10

	pkt := buf[:pos]
	protocol.PktSignInPlace(pkt[:24])
	return pkt
}
