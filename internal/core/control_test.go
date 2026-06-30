package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
