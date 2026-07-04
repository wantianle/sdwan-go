package core

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	controlapi "sdwan-go/pkg/controlapi"
)

func TestLoadControlTokenExisting(t *testing.T) {
	f := filepath.Join(t.TempDir(), "control.token")
	if err := os.WriteFile(f, []byte("cli-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tok, err := controlapi.LoadControlToken(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "cli-token" {
		t.Fatalf("expected cli-token, got %q", tok)
	}
}

func TestLoadControlTokenMissing(t *testing.T) {
	_, err := controlapi.LoadControlToken("/nonexistent/token.file")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadControlTokenEmpty(t *testing.T) {
	f := filepath.Join(t.TempDir(), "empty.token")
	if err := os.WriteFile(f, []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := controlapi.LoadControlToken(f)
	if err == nil {
		t.Fatal("expected error for empty token file")
	}
}

func TestLoadControlTokenEmptyPath(t *testing.T) {
	_, err := controlapi.LoadControlToken("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestLoadOrCreateControlTokenGenerates(t *testing.T) {
	f := filepath.Join(t.TempDir(), "subdir", "control.token")
	tok, err := controlapi.LoadOrCreateControlToken(f)
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
	tok2, err := controlapi.LoadOrCreateControlToken(f)
	if err != nil {
		t.Fatal(err)
	}
	if tok != tok2 {
		t.Fatalf("second load returned different token")
	}
}

func TestLoadOrCreateControlTokenExisting(t *testing.T) {
	f := filepath.Join(t.TempDir(), "control.token")
	if err := os.WriteFile(f, []byte("my-new-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tok, err := controlapi.LoadOrCreateControlToken(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "my-new-token" {
		t.Fatalf("expected 'my-new-token', got %q", tok)
	}
}

func TestLoadOrCreateControlTokenRejectsEmpty(t *testing.T) {
	f := filepath.Join(t.TempDir(), "control.token")
	if err := os.WriteFile(f, []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlapi.LoadOrCreateControlToken(f); err == nil {
		t.Fatal("expected error for empty token file, got nil")
	}
}

func TestLoadOrCreateControlTokenEmptyPath(t *testing.T) {
	_, err := controlapi.LoadOrCreateControlToken("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestLoadOrCreateControlTokenUnreadableDir(t *testing.T) {
	// A path whose parent directory cannot be created (e.g. under /root)
	// should return an error rather than panic.
	_, err := controlapi.LoadOrCreateControlToken("/root/nonexistent/control.token")
	if err == nil {
		t.Fatal("expected error for unwritable parent path")
	}
}

// ---------- controlapi.ControlStatus / controlapi.ControlSwitch against httptest ----------

func TestControlClientStatusAuth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(&controlapi.StatusResult{
			State: "running", Server: "s", Port: 10010, SessionID: 42,
			TUN: "iwan1", LocalIP: "10.0.0.2", GatewayIP: "10.0.0.1",
			Route: "192.168.0.0/16", MTU: 1436,
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")

	// Wrong token
	_, err := controlapi.ControlStatus(addr, "wrong")
	if err == nil {
		t.Fatal("expected error with wrong token")
	}

	// Correct token
	sr, err := controlapi.ControlStatus(addr, "secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sr.SessionID != 42 {
		t.Errorf("session_id: got %d, want 42", sr.SessionID)
	}
}

func TestControlClientSwitch(t *testing.T) {
	var reqBody []byte
	var reqAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqAuth = r.Header.Get("Authorization")
		reqBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(controlapi.SwitchResponse{
			Status: &controlapi.StatusResult{State: "running", SessionID: 99, TUN: "iwan1"},
			Tunnel: &controlapi.TunnelInfo{LocalIP: "10.0.0.2", GatewayIP: "10.0.0.1"},
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	resp, err := controlapi.ControlSwitch(addr, "cli-tok", "new.host.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status.SessionID != 99 {
		t.Errorf("session_id: got %d", resp.Status.SessionID)
	}

	// Verify Authorization header was sent
	if reqAuth != "Bearer cli-tok" {
		t.Errorf("Authorization: got %q, want Bearer cli-tok", reqAuth)
	}

	// Verify request body
	var req struct {
		Server string `json:"server"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	if req.Server != "new.host.example.com" {
		t.Errorf("server: got %q", req.Server)
	}
}

func TestControlClientSwitchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"server unreachable"}`, http.StatusInternalServerError)
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	_, err := controlapi.ControlSwitch(addr, "tok", "bad.host")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestControlClientShutdown(t *testing.T) {
	var reqMethod, reqAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMethod = r.Method
		reqAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	err := controlapi.ControlShutdown(addr, "shutdown-tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reqMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", reqMethod)
	}
	if reqAuth != "Bearer shutdown-tok" {
		t.Errorf("Authorization: got %q, want Bearer shutdown-tok", reqAuth)
	}
}

func TestControlClientShutdownError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"gone"}`, http.StatusGone)
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	err := controlapi.ControlShutdown(addr, "tok")
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}
