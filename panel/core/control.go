package core

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	controlapi "sdwan-go/pkg/controlapi"
)

// isAuthError returns true if err represents a 401 Unauthorized response
// from the control API (wrong/mismatched token).
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized")
}

// --- HTTP control client ---

const (
	controlTimeoutStatus = 2 * time.Second
	controlTimeoutAction = 15 * time.Second
)

func getControlStatus(addr, token string) (*controlapi.StatusResult, error) {
	return controlapi.ControlStatusWithTimeout(addr, token, controlTimeoutStatus)
}

func postControlSwitch(addr, token, server string) (*controlapi.SwitchResponse, error) {
	return controlapi.ControlSwitchWithTimeout(addr, token, server, controlTimeoutAction)
}

func postControlPause(addr, token string, pause bool) error {
	return controlapi.ControlPauseWithTimeout(addr, token, pause, controlTimeoutAction)
}

// postControlShutdown sends a graceful shutdown request to the daemon.
func postControlShutdown(addr, token string) error {
	return controlapi.ControlShutdownWithTimeout(addr, token, controlTimeoutAction)
}

// loadOrGenerateToken reads the token file if it exists; otherwise it
// generates a 32-byte random token, base64url encodes it, writes it with
// mode 0600 (creating parent directories), and returns it.
// Matches the core daemon's token generation so panel and daemon share one token.
func loadOrGenerateToken(path string) (string, error) {
	tok, err := controlapi.LoadControlToken(path)
	if err == nil {
		return tok, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	// Generate new token (first install)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	return token, nil
}
