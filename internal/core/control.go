package core

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// loadOrGenerateToken reads a bearer token from the file at tokenPath.
// If the file does not exist it generates a 32-byte random token, base64url
// encodes it, and writes it with mode 0600.
func loadOrGenerateToken(tokenPath string) (string, error) {
	if tokenPath == "" {
		return "", fmt.Errorf("token file path is empty")
	}

	data, err := os.ReadFile(tokenPath)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file is empty: %s", tokenPath)
		}
		return token, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("read token file: %w", err)
	}

	// Generate new token
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	if err := os.MkdirAll(filepath.Dir(tokenPath), 0700); err != nil {
		return "", fmt.Errorf("create token dir: %w", err)
	}
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write token file: %w", err)
	}

	log.Printf("[CTRL] Generated control token at %s", tokenPath)
	return token, nil
}

// bearerAuth wraps next with Bearer token authentication. Requests that
// match pathPrefix must carry a valid Authorization: Bearer <token> header.
func bearerAuth(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, `{"error":"missing Bearer token"}`, http.StatusUnauthorized)
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if got != token {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// startControlServer binds an HTTP server on addr and serves the /v1/*
// namespace behind Bearer token authentication.  The returned server must
// be shut down by the caller.
func startControlServer(addr string, token string, c *Client) (*http.Server, error) {
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

	authMux := bearerAuth(mux, token)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen control API: %w", err)
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      authMux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
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
	if dir == "." {
		dir = "."
	}
	return filepath.Join(dir, "control.token")
}
