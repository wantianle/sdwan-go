package core

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	controlapi "sdwan-go/pkg/controlapi"
	protocol "sdwan-go/pkg/protocol"
)

// bearerAuth wraps next with Bearer token authentication. Requests that
// match pathPrefix must carry a valid Authorization: Bearer <token> header.
func bearerAuth(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSONError(w, http.StatusUnauthorized, "missing Bearer token")
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// switchServerFunc abstracts Client.SwitchServer so tests can inject a fake.
type switchServerFunc func(next *Config) (*protocol.OPENACKResult, error)

// switchRequest is the expected JSON body for POST /v1/switch.
type switchRequest struct {
	Server string `json:"server"`
}

// newControlMux builds the /v1/* handler tree backed by the given Client and
// switch function. Tests call this directly with a mock switch; production
// passes c.SwitchServer.
// shutdownCh is an optional channel that will be signalled when POST /v1/shutdown
// is called; pass nil if the caller does not need shutdown signalling.
func newControlMux(c *Client, switchFn switchServerFunc, shutdownCh chan<- struct{}) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(c.Status())
	})

	mux.HandleFunc("/v1/switch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		var req switchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		req.Server = strings.TrimSpace(req.Server)
		if req.Server == "" {
			writeJSONError(w, http.StatusBadRequest, "server is required")
			return
		}

		// Clone current config, only replacing the server name.
		cfg := c.currentConfig()
		if cfg == nil {
			writeJSONError(w, http.StatusInternalServerError, "no active config")
			return
		}
		nextCfg := cloneConfig(cfg)
		nextCfg.Server = req.Server

		tun, err := switchFn(nextCfg)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "switch failed: "+err.Error())
			return
		}

		resp := controlapi.SwitchResponse{
			Status: c.Status(),
			Tunnel: &controlapi.TunnelInfo{
				LocalIP:   tun.LocalIP,
				GatewayIP: tun.GatewayIP,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/v1/pause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req controlapi.PauseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		c.SetPaused(req.Pause)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(controlapi.PauseResponse{Status: c.Status(), Paused: c.Paused()})
	})

	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})

		// Signal the daemon to exit gracefully. Non-blocking send so the
		// handler returns cleanly even if the listener has already left.
		if shutdownCh != nil {
			select {
			case shutdownCh <- struct{}{}:
			default:
			}
		}
	})

	return mux
}

// startControlServer binds an HTTP server on addr and serves the /v1/*
// namespace behind Bearer token authentication.  The returned server must
// be shut down by the caller.
// shutdownCh is an optional channel that receives a signal when
// POST /v1/shutdown is successfully called.
func startControlServer(addr string, token string, c *Client, shutdownCh chan<- struct{}) (*http.Server, error) {
	mux := newControlMux(c, c.SwitchServer, shutdownCh)
	authMux := bearerAuth(mux, token)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen control API: %w", err)
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      authMux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
		BaseContext: func(l net.Listener) context.Context {
			return context.Background()
		},
	}

	go func() {
		log.Printf("[CTRL] Control API listening on %s", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[CTRL] Control API error: %v", err)
		}
	}()

	return srv, nil
}

// DefaultTokenPath derives the control.token path from the config file
// directory. For example, "/etc/sdwan/iwan.conf" → "/etc/sdwan/control.token".
func DefaultTokenPath(configPath string) string {
	dir := filepath.Dir(configPath)
	return filepath.Join(dir, "control.token")
}
