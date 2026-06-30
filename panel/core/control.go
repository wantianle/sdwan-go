package core

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// --- control API types (mirror core StatusResult / SwitchResponse) ---

type controlStatusResult struct {
	State     string `json:"state"`
	Server    string `json:"server"`
	Port      int    `json:"port"`
	SessionID uint16 `json:"session_id"`
	TUN       string `json:"tun"`
	LocalIP   string `json:"local_ip"`
	GatewayIP string `json:"gateway_ip"`
	Route     string `json:"route"`
	MTU       int    `json:"mtu"`
}

type controlSwitchResponse struct {
	Status *controlStatusResult `json:"status"`
	Tunnel *controlOpenAck      `json:"tunnel,omitempty"`
}

type controlOpenAck struct {
	LocalIP   string `json:"local_ip"`
	GatewayIP string `json:"gateway_ip"`
}

type controlError struct {
	Error string `json:"error"`
}

// --- token helpers ---

// loadControlToken reads the bearer token from the file at path.
// Returns an error if the file is missing or empty.
func loadControlToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("empty token file")
	}
	return token, nil
}

// loadOrGenerateToken reads the token file if it exists; otherwise it
// generates a 32-byte random token, base64url encodes it, writes it with
// mode 0600 (creating parent directories), and returns it.
// Matches the core daemon's token generation so panel and daemon share one token.
func loadOrGenerateToken(path string) (string, error) {
	tok, err := loadControlToken(path)
	if err == nil {
		return tok, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

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

const controlTimeout = 10 * time.Second

func getControlStatus(addr, token string) (*controlStatusResult, error) {
	url := "http://" + addr + "/v1/status"
	body, err := controlRequest(http.MethodGet, url, token, nil)
	if err != nil {
		return nil, err
	}
	var sr controlStatusResult
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}
	return &sr, nil
}

func postControlSwitch(addr, token, server string) (*controlSwitchResponse, error) {
	url := "http://" + addr + "/v1/switch"
	reqBody, _ := json.Marshal(map[string]string{"server": server})
	body, err := controlRequest(http.MethodPost, url, token, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	var resp controlSwitchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse switch response: %w", err)
	}
	return &resp, nil
}

func controlRequest(method, url, token string, body io.Reader) ([]byte, error) {
	client := &http.Client{Timeout: controlTimeout}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var ce controlError
		if json.Unmarshal(data, &ce) == nil && ce.Error != "" {
			return nil, fmt.Errorf("control API %d: %s", resp.StatusCode, ce.Error)
		}
		return nil, fmt.Errorf("control API %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}
