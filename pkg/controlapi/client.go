package controlapi

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

// DefaultControlTimeout is the HTTP client timeout for control API requests.
const DefaultControlTimeout = 10 * time.Second

// LoadControlToken reads and trims the control token from the file at path.
// Returns an error if the file is missing or empty.
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

// ControlStatus fetches the daemon status via GET /v1/status (10s default timeout).
func ControlStatus(addr, token string) (*StatusResult, error) {
	return ControlStatusWithTimeout(addr, token, DefaultControlTimeout)
}

// ControlStatusWithTimeout fetches the daemon status with an explicit timeout.
func ControlStatusWithTimeout(addr, token string, timeout time.Duration) (*StatusResult, error) {
	url := "http://" + addr + "/v1/status"
	body, err := doControlRequest(http.MethodGet, url, token, nil, timeout)
	if err != nil {
		return nil, err
	}
	var sr StatusResult
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("parse status response: %w", err)
	}
	return &sr, nil
}

// ControlSwitch asks a running sdwan daemon to switch its tunnel (10s default timeout).
func ControlSwitch(addr, token, server string) (*SwitchResponse, error) {
	return ControlSwitchWithTimeout(addr, token, server, DefaultControlTimeout)
}

// ControlSwitchWithTimeout switches the tunnel session with an explicit timeout.
func ControlSwitchWithTimeout(addr, token, server string, timeout time.Duration) (*SwitchResponse, error) {
	url := "http://" + addr + "/v1/switch"
	reqBody, _ := json.Marshal(map[string]string{"server": server})

	body, err := doControlRequest(http.MethodPost, url, token, bytes.NewReader(reqBody), timeout)
	if err != nil {
		return nil, err
	}
	var resp SwitchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse switch response: %w", err)
	}
	return &resp, nil
}

// ControlShutdown sends a graceful shutdown request (10s default timeout).
func ControlShutdown(addr, token string) error {
	return ControlShutdownWithTimeout(addr, token, DefaultControlTimeout)
}

// ControlShutdownWithTimeout sends a graceful shutdown with an explicit timeout.
func ControlShutdownWithTimeout(addr, token string, timeout time.Duration) error {
	url := "http://" + addr + "/v1/shutdown"
	_, err := doControlRequest(http.MethodPost, url, token, nil, timeout)
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// ControlPause sets or clears the paused state on the daemon via POST /v1/pause (10s default timeout).
func ControlPause(addr, token string, pause bool) error {
	return ControlPauseWithTimeout(addr, token, pause, DefaultControlTimeout)
}

// ControlPauseWithTimeout sets or clears the paused state with an explicit timeout.
func ControlPauseWithTimeout(addr, token string, pause bool, timeout time.Duration) error {
	url := "http://" + addr + "/v1/pause"
	reqBody, _ := json.Marshal(map[string]bool{"pause": pause})
	_, err := doControlRequest(http.MethodPost, url, token, bytes.NewReader(reqBody), timeout)
	return err
}

func doControlRequest(method, url, token string, body io.Reader, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
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
