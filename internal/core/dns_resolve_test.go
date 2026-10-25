package core

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	protocol "sdwan-go/pkg/protocol"
)

// withResolveLookup installs a DNS seam for the duration of the test.
func withResolveLookup(t *testing.T, fn func(ctx context.Context, host string) ([]net.IPAddr, error)) {
	t.Helper()
	orig := resolveLookup
	resolveLookup = fn
	t.Cleanup(func() { resolveLookup = orig })
}

// withResolveNow installs a clock seam for the duration of the test so TTL
// expiry is deterministic. Tests using it must not call t.Parallel.
func withResolveNow(t *testing.T, fn func() time.Time) {
	t.Helper()
	orig := resolveNow
	resolveNow = fn
	t.Cleanup(func() { resolveNow = orig })
}

// clearServerIPCache empties the package-level resolved-IP cache so tests do
// not leak entries into each other.
func clearServerIPCache(t *testing.T) {
	t.Helper()
	serverIPCache.Range(func(k, _ any) bool {
		serverIPCache.Delete(k)
		return true
	})
}

// An IP-literal server must skip DNS entirely.
func TestResolveServerAddrIPLiteralSkipsDNS(t *testing.T) {
	called := false
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		called = true
		return nil, errors.New("DNS must not be used for an IP literal")
	})

	addr, err := resolveServerAddr("10.1.2.3", 10010)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("resolver was called for an IP-literal server")
	}
	if addr.IP.String() != "10.1.2.3" || addr.Port != 10010 {
		t.Fatalf("unexpected addr: %v", addr)
	}
}

// Successful resolution returns the expected address, preferring IPv4.
func TestResolveServerAddrSuccessPrefersIPv4(t *testing.T) {
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("2001:db8::1")},
			{IP: net.ParseIP("192.0.2.7")},
		}, nil
	})

	addr, err := resolveServerAddr("example.test", 10010)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr.IP.String() != "192.0.2.7" || addr.Port != 10010 {
		t.Fatalf("expected 192.0.2.7:10010, got %v", addr)
	}
}

// With no IPv4 available, the first resolved address is used.
func TestResolveServerAddrIPv6OnlyFallback(t *testing.T) {
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("2001:db8::1")}}, nil
	})

	addr, err := resolveServerAddr("v6.example", 10010)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr.IP.String() != "2001:db8::1" || addr.Port != 10010 {
		t.Fatalf("expected [2001:db8::1]:10010, got %v", addr)
	}
}

// A slow resolver must be cut off by the bounded timeout, well under the old
// ~10s OS-resolver stall, and surface the deadline error.
func TestResolveServerAddrSlowResolverFailsFast(t *testing.T) {
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	start := time.Now()
	_, err := resolveServerAddr("slow.example", 10010)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("resolution took %v, not bounded below the old ~10s stall", elapsed)
	}
	if elapsed > serverResolveTimeout+2*time.Second {
		t.Fatalf("resolution took %v, expected near %v", elapsed, serverResolveTimeout)
	}
}

// newSession must keep wrapping resolve failures as "resolve server: %w".
func TestNewSessionWrapsResolveError(t *testing.T) {
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	cfg := &Config{Server: "slow.example", Port: 10010, MTU: 1436, TUNName: "iwan1"}
	start := time.Now()
	_, err := newSession(cfg, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "resolve server:") {
		t.Fatalf("error must keep the 'resolve server:' wrapping, got %q", err.Error())
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("newSession took %v, not bounded below the old ~10s stall", elapsed)
	}
}

// newSession with an IP-literal server must not touch DNS and must dial.
func TestNewSessionIPLiteral(t *testing.T) {
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, errors.New("DNS must not be used for an IP literal")
	})

	cfg := &Config{Server: "127.0.0.1", Port: 10010, MTU: 1436, TUNName: "iwan1"}
	s, err := newSession(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer s.Close()
	if s.server == nil || s.server.IP.String() != "127.0.0.1" || s.server.Port != 10010 {
		t.Fatalf("unexpected server addr: %v", s.server)
	}
}

// A second resolution for a cached hostname must reuse the cached IP and not
// invoke the resolver seam.
func TestResolveServerAddrCacheHitSkipsDNS(t *testing.T) {
	clearServerIPCache(t)
	calls := 0
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}, nil
	})

	first, err := resolveServerAddr("cache-hit.example", 10010)
	if err != nil {
		t.Fatalf("first resolve failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls after first resolve = %d, want 1", calls)
	}

	second, err := resolveServerAddr("cache-hit.example", 10010)
	if err != nil {
		t.Fatalf("second resolve failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("resolver called %d times, want 1: cache hit must skip DNS", calls)
	}
	if !second.IP.Equal(first.IP) || second.IP.String() != "192.0.2.10" {
		t.Fatalf("cached addr = %v, want 192.0.2.10", second)
	}
}

// A cached IP is reused while fresh, and re-resolved once the entry is older
// than serverIPTTL, picking up a changed server IP.
func TestResolveServerAddrCacheTTLExpiry(t *testing.T) {
	clearServerIPCache(t)
	now := time.Unix(1_000_000, 0)
	withResolveNow(t, func() time.Time { return now })

	ip := "192.0.2.10"
	calls := 0
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	})

	if _, err := resolveServerAddr("ttl.example", 10010); err != nil {
		t.Fatalf("first resolve failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}

	// Just inside the TTL: still a cache hit, no resolver call.
	now = now.Add(serverIPTTL - time.Second)
	if _, err := resolveServerAddr("ttl.example", 10010); err != nil {
		t.Fatalf("fresh-cache resolve failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d within TTL, want 1 (cache hit)", calls)
	}

	// Past the TTL: the entry is stale, so re-resolve and pick up the new IP.
	now = now.Add(2 * time.Second)
	ip = "192.0.2.99"
	got, err := resolveServerAddr("ttl.example", 10010)
	if err != nil {
		t.Fatalf("expired-cache resolve failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("resolver calls = %d after TTL expiry, want 2 (re-resolve)", calls)
	}
	if got.IP.String() != "192.0.2.99" {
		t.Fatalf("resolve after expiry = %v, want re-resolved 192.0.2.99", got)
	}
}

// IP literals bypass the cache entirely: no entry is ever created.
func TestServerIPCacheIPLiteralNotCached(t *testing.T) {
	clearServerIPCache(t)
	withResolveLookup(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, errors.New("DNS must not be used for an IP literal")
	})

	addr, err := resolveServerAddr("10.1.2.3", 10010)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr.IP.String() != "10.1.2.3" {
		t.Fatalf("unexpected addr: %v", addr)
	}
	if _, ok := serverIPCache.Load("10.1.2.3"); ok {
		t.Fatal("IP literal must never be stored in the cache")
	}
}

// REGRESSION: a failed SwitchServer connect+handshake (e.g. during a network
// outage) must KEEP the cached IP, and the next attempt must reuse it with no
// resolver call. Previously the failure evicted the cache, so the following
// attempt re-resolved and stalled ~serverResolveTimeout in DNS — the exact
// DNS-stall retry loop this cache exists to prevent.
func TestSwitchServerFailureKeepsCacheAndSkipsDNS(t *testing.T) {
	clearServerIPCache(t)
	now := time.Unix(1_000_000, 0)
	withResolveNow(t, func() time.Time { return now })

	srv := newFakeOpenACKServer(t)
	const host = "switch-outage.example"
	calls := 0
	withResolveLookup(t, func(ctx context.Context, h string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	})

	cfg := &Config{Server: host, Port: srv.port(), Username: "u", Password: "p", MTU: 1400}
	c := NewClient(cfg)
	c.SetTunnelConfig(&protocol.OPENACKResult{LocalIP: "10.100.100.7", GatewayIP: "10.100.100.1"})
	defer c.Close()

	// Cold attempt: resolves once, then the rejected handshake fails. The
	// failure must NOT evict the freshly cached IP.
	srv.reject.Store(true)
	if _, err := c.SwitchServer(cfg); err == nil {
		t.Fatal("expected a rejected handshake to fail the switch")
	}
	if calls != 1 {
		t.Fatalf("cold-start resolver calls = %d, want 1", calls)
	}
	if entry, ok := serverIPCache.Load(host); !ok || entry.(serverIPEntry).ip.String() != "127.0.0.1" {
		t.Fatalf("cache must survive a failed handshake, got %v (ok=%t)", entry, ok)
	}

	// Network still down: the next attempt must reuse the cached IP with NO
	// DNS call, instead of stalling in the resolver.
	if _, err := c.SwitchServer(cfg); err == nil {
		t.Fatal("expected the second rejected handshake to fail")
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d during outage, want 1 (failure must reuse cached IP)", calls)
	}

	// Network recovers: the switch succeeds, still from the cached IP.
	srv.reject.Store(false)
	if _, err := c.SwitchServer(cfg); err != nil {
		t.Fatalf("switch after recovery failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d after recovery, want 1 (cached IP reused)", calls)
	}
}
