package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadTokenExisting(t *testing.T) {
	f := filepath.Join(t.TempDir(), "control.token")
	if err := os.WriteFile(f, []byte("my-test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tok, err := loadOrGenerateToken(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "my-test-token" {
		t.Fatalf("expected 'my-test-token', got %q", tok)
	}
}

func TestLoadTokenGeneratesNew(t *testing.T) {
	f := filepath.Join(t.TempDir(), "subdir", "control.token")
	tok, err := loadOrGenerateToken(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == "" {
		t.Fatal("generated token is empty")
	}
	decoded, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(decoded)) != tok {
		t.Fatalf("file contents %q != returned token %q", string(decoded), tok)
	}
	// Second call should read the same token from disk.
	tok2, err := loadOrGenerateToken(f)
	if err != nil {
		t.Fatal(err)
	}
	if tok != tok2 {
		t.Fatalf("second load returned different token")
	}
}

func TestLoadTokenRejectsEmpty(t *testing.T) {
	f := filepath.Join(t.TempDir(), "control.token")
	if err := os.WriteFile(f, []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrGenerateToken(f); err == nil {
		t.Fatal("expected error for empty token file, got nil")
	}
}

func TestBearerAuthRejectsMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := bearerAuth(mux, "secret")

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestBearerAuthRejectsWrongToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := bearerAuth(mux, "secret")

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestBearerAuthPasses(t *testing.T) {
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := bearerAuth(mux, "secret")

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestStatusEndpointReturnsJSON(t *testing.T) {
	c, err := NewClient(&Config{
		Server:   "test.example.com",
		Username: "u",
		Password: "p",
		Port:     10010,
		MTU:      1436,
		RouteNet: "192.168.0.0/16",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetTunnelConfig(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
	})

	// Build the handler stack the same way RunDaemon does.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		sr := c.Status()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sr)
	})
	handler := bearerAuth(mux, "daemon-token")

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer daemon-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected JSON content-type, got %q", ct)
	}

	var sr StatusResult
	if err := json.Unmarshal(rec.Body.Bytes(), &sr); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if sr.State != "disconnected" {
		t.Errorf("state: got %q, want disconnected", sr.State)
	}
	if sr.Server != "test.example.com" {
		t.Errorf("server: got %q", sr.Server)
	}
	if sr.Port != 10010 {
		t.Errorf("port: got %d", sr.Port)
	}
	if sr.Route != "192.168.0.0/16" {
		t.Errorf("route: got %q", sr.Route)
	}
	if sr.MTU != 1436 {
		t.Errorf("mtu: got %d", sr.MTU)
	}
	if sr.LocalIP != "10.0.0.2" {
		t.Errorf("local_ip: got %q", sr.LocalIP)
	}
	if sr.GatewayIP != "10.0.0.1" {
		t.Errorf("gateway_ip: got %q", sr.GatewayIP)
	}
	if sr.SessionID != 0 {
		t.Errorf("session_id: got %d, want 0 (not handshaked)", sr.SessionID)
	}
}

func newTestClient() *Client {
	c, _ := NewClient(&Config{
		Server:   "current.example.com",
		Username: "u",
		Password: "p",
		Port:     10010,
		MTU:      1436,
		RouteNet: "192.168.0.0/16",
	})
	c.SetTunnelConfig(&OPENACKResult{
		LocalIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
	})
	return c
}

// authMux wraps newControlMux output with bearerAuth so we can exercise
// the full auth + handler stack in one request.
func authMux(c *Client, fn switchServerFunc, token string) http.Handler {
	return bearerAuth(newControlMux(c, fn, nil), token)
}

func TestSwitchEndpointRejectsGET(t *testing.T) {
	c := newTestClient()
	h := authMux(c, nil, "tok")
	req := httptest.NewRequest(http.MethodGet, "/v1/switch", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSwitchEndpointRejectsEmptyServer(t *testing.T) {
	c := newTestClient()
	h := authMux(c, nil, "tok")
	body := strings.NewReader(`{"server":""}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", body)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSwitchEndpointRejectsBlankServer(t *testing.T) {
	c := newTestClient()
	h := authMux(c, nil, "tok")
	body := strings.NewReader(`{"server":"   "}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", body)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSwitchEndpointRejectsInvalidJSON(t *testing.T) {
	c := newTestClient()
	h := authMux(c, nil, "tok")
	body := strings.NewReader(`not-json`)
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", body)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSwitchEndpointAuthRejected(t *testing.T) {
	c := newTestClient()
	h := authMux(c, nil, "tok")
	body := strings.NewReader(`{"server":"test.example.com"}`)

	// No token
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Wrong token
	req2 := httptest.NewRequest(http.MethodPost, "/v1/switch", body)
	req2.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong token, got %d", rec2.Code)
	}
}

func TestSwitchEndpointSuccess(t *testing.T) {
	c := newTestClient()
	var switchedTo string
	fn := func(next *Config) (*OPENACKResult, error) {
		switchedTo = next.Server
		return &OPENACKResult{
			LocalIP:   "10.0.0.2",
			GatewayIP: "10.0.0.1",
		}, nil
	}
	h := authMux(c, fn, "tok")

	body := strings.NewReader(`{"server":"new.example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", body)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if switchedTo != "new.example.com" {
		t.Fatalf("switch called with %q, expected new.example.com", switchedTo)
	}

	var resp SwitchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status.State != "disconnected" {
		t.Errorf("status state: got %q", resp.Status.State)
	}
	if resp.Tunnel.LocalIP != "10.0.0.2" {
		t.Errorf("tunnel local_ip: got %q", resp.Tunnel.LocalIP)
	}
}

func TestSwitchEndpointError(t *testing.T) {
	c := newTestClient()
	fn := func(next *Config) (*OPENACKResult, error) {
		return nil, fmt.Errorf("handshake \"timeout\"")
	}
	h := authMux(c, fn, "tok")

	body := strings.NewReader(`{"server":"bad.example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", body)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("expected valid JSON error response, got %q: %v", rec.Body.String(), err)
	}
	if !strings.Contains(errBody["error"], "handshake") {
		t.Fatalf("unexpected error body: %#v", errBody)
	}
}

func TestSwitchEndpointServerOnly(t *testing.T) {
	// Verify that the switch handler only allows Server to change and
	// does not propagate other config fields from the request body.
	c := newTestClient()
	var received *Config
	fn := func(next *Config) (*OPENACKResult, error) {
		received = next
		return &OPENACKResult{LocalIP: "10.0.0.2", GatewayIP: "10.0.0.1"}, nil
	}
	h := authMux(c, fn, "tok")

	// Try to sneak in password/port changes via the JSON body.
	body := strings.NewReader(`{"server":"evil.com","password":"hacked","port":666}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/switch", body)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if received.Server != "evil.com" {
		t.Errorf("server: got %q", received.Server)
	}
	// Password, port, and username must come from current config, not from
	// the request body.
	if received.Password != "p" {
		t.Errorf("password: got %q, want original value", received.Password)
	}
	if received.Port != 10010 {
		t.Errorf("port: got %d, want 10010", received.Port)
	}
	if received.Username != "u" {
		t.Errorf("username: got %q, want original value", received.Username)
	}
}

func TestShutdownEndpointRejectsGET(t *testing.T) {
	c := newTestClient()
	h := authMux(c, nil, "tok")
	req := httptest.NewRequest(http.MethodGet, "/v1/shutdown", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestShutdownEndpointAuthRejected(t *testing.T) {
	c := newTestClient()
	h := authMux(c, nil, "tok")
	req := httptest.NewRequest(http.MethodPost, "/v1/shutdown", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestShutdownEndpointSignalsChannel(t *testing.T) {
	c := newTestClient()
	shutdownCh := make(chan struct{}, 1)

	// Use newControlMux directly with a shutdown channel.
	mux := newControlMux(c, nil, shutdownCh)
	h := bearerAuth(mux, "tok")

	req := httptest.NewRequest(http.MethodPost, "/v1/shutdown", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Channel must have received the signal.
	select {
	case <-shutdownCh:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown channel was not signalled")
	}
}

func TestShutdownEndpointWithoutChannel(t *testing.T) {
	c := newTestClient()

	// newControlMux with nil shutdownCh must not panic.
	mux := newControlMux(c, nil, nil)
	h := bearerAuth(mux, "tok")

	req := httptest.NewRequest(http.MethodPost, "/v1/shutdown", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestDefaultTokenPath(t *testing.T) {
	got := DefaultTokenPath("/etc/sdwan/iwan.conf")
	if got != "/etc/sdwan/control.token" {
		t.Fatalf("expected /etc/sdwan/control.token, got %q", got)
	}

	got2 := DefaultTokenPath("iwan.conf")
	if !strings.HasSuffix(got2, "control.token") {
		t.Fatalf("expected .../control.token, got %q", got2)
	}
}
