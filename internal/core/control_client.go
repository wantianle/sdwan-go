package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// LoadControlToken reads and trims the control token from the file at path.
// Returns an error if the file is missing or empty. This helper is exported
// for CLI control clients; it deliberately does NOT generate a new token.
func LoadControlToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("token file path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// ControlStatus fetches the daemon status from a running sdwan daemon
// via GET /v1/status. addr should be "host:port" (no scheme).
func ControlStatus(addr, token string) (*StatusResult, error) {
	url := "http://" + addr + "/v1/status"
	body, err := doControlRequest(http.MethodGet, url, token, nil)
	if err != nil {
		return nil, err
	}
	var sr StatusResult
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("parse status response: %w", err)
	}
	return &sr, nil
}

// ControlSwitch asks a running sdwan daemon to switch its tunnel session to
// the given server via POST /v1/switch. addr should be "host:port" (no scheme).
func ControlSwitch(addr, token, server string) (*SwitchResponse, error) {
	url := "http://" + addr + "/v1/switch"
	reqBody, _ := json.Marshal(map[string]string{"server": server})

	body, err := doControlRequest(http.MethodPost, url, token, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	var resp SwitchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse switch response: %w", err)
	}
	return &resp, nil
}

func doControlRequest(method, url, token string, body io.Reader) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
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
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}
