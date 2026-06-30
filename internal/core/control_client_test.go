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
)

func TestLoadControlTokenExisting(t *testing.T) {
	f := filepath.Join(t.TempDir(), "control.token")
	if err := os.WriteFile(f, []byte("cli-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadControlToken(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "cli-token" {
		t.Fatalf("expected cli-token, got %q", tok)
	}
}

func TestLoadControlTokenMissing(t *testing.T) {
	_, err := LoadControlToken("/nonexistent/token.file")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadControlTokenEmpty(t *testing.T) {
	f := filepath.Join(t.TempDir(), "empty.token")
	if err := os.WriteFile(f, []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadControlToken(f)
	if err == nil {
		t.Fatal("expected error for empty token file")
	}
}

func TestLoadControlTokenEmptyPath(t *testing.T) {
	_, err := LoadControlToken("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

// ---------- ControlStatus / ControlSwitch against httptest ----------

func TestControlClientStatusAuth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(&StatusResult{
			State: "running", Server: "s", Port: 10010, SessionID: 42,
			TUN: "iwan1", LocalIP: "10.0.0.2", GatewayIP: "10.0.0.1",
			Route: "192.168.0.0/16", MTU: 1436,
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")

	// Wrong token
	_, err := ControlStatus(addr, "wrong")
	if err == nil {
		t.Fatal("expected error with wrong token")
	}

	// Correct token
	sr, err := ControlStatus(addr, "secret")
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
		json.NewEncoder(w).Encode(SwitchResponse{
			Status: &StatusResult{State: "running", SessionID: 99, TUN: "iwan1"},
			Tunnel: &OPENACKResult{LocalIP: "10.0.0.2", GatewayIP: "10.0.0.1"},
		})
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	resp, err := ControlSwitch(addr, "cli-tok", "new.host.example.com")
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
	_, err := ControlSwitch(addr, "tok", "bad.host")
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
	err := ControlShutdown(addr, "shutdown-tok")
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
	err := ControlShutdown(addr, "tok")
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}
