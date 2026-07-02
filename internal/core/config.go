package core

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all settings from iwan.conf
type Config struct {
	Server   string
	Username string
	Password string
	Port     int
	MTU      int
	Encrypt  int
	PipeID   int
	PipeIdx  int
	RouteNet string // route network, e.g. "192.168.0.0/16"
	TUNName  string // TUN interface name, e.g. "iwan1"
}

// LoadConfig parses /etc/sdwan/iwan.conf (INI-like format)
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	cfg := &Config{
		Port:     10010,
		MTU:      1436,
		Encrypt:  0,
		PipeID:   0,
		PipeIdx:  0,
		RouteNet: "192.168.0.0/16",
		TUNName:  "iwan1",
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "server":
			cfg.Server = val
		case "username":
			cfg.Username = val
		case "password":
			cfg.Password = val
		case "port":
			v, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("config: invalid port %q: %w", val, err)
			}
			if v > 0 {
				cfg.Port = v
			}
		case "mtu":
			v, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("config: invalid MTU %q: %w", val, err)
			}
			if v > 0 {
				cfg.MTU = v
			}
		case "encrypt":
			v, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("config: invalid encrypt %q: %w", val, err)
			}
			if v != 0 && v != 1 {
				return nil, fmt.Errorf("config: encrypt must be 0 or 1, got %d", v)
			}
			cfg.Encrypt = v
		case "pipeid":
			v, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("config: invalid pipeid %q: %w", val, err)
			}
			cfg.PipeID = v
		case "pipeidx":
			v, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("config: invalid pipeidx %q: %w", val, err)
			}
			cfg.PipeIdx = v
		case "routenet":
			cfg.RouteNet = val
		case "tunname":
			cfg.TUNName = val
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks required fields
func (c *Config) Validate() error {
	if c.Server == "" {
		return fmt.Errorf("INVALID peer server")
	}
	if c.Username == "" {
		return fmt.Errorf("INVALID username")
	}
	if c.Password == "" {
		return fmt.Errorf("INVALID password")
	}
	if c.MTU <= 0 {
		return fmt.Errorf("INVALID MTU")
	}
	if c.Port <= 0 {
		return fmt.Errorf("INVALID port")
	}
	return nil
}
