package core

import "testing"

func TestConfigValidateMTURange(t *testing.T) {
	tests := []struct {
		name    string
		mtu     int
		wantErr bool
	}{
		{"below minimum", 1199, true},
		{"minimum", 1200, false},
		{"within range", 1436, false},
		{"maximum", 1500, false},
		{"above maximum", 1501, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Server:   "test.example.com",
				Username: "u",
				Password: "p",
				Port:     10010,
				MTU:      tt.mtu,
			}
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("MTU=%d: got error %v, wantErr=%v", tt.mtu, err, tt.wantErr)
			}
		})
	}
}
