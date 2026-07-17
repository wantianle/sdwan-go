package main

import "testing"

func TestFileLoggingEnabledOnlyOnWindows(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		if fileLoggingEnabled(goos) {
			t.Errorf("file logging must be disabled on %s", goos)
		}
	}
	if !fileLoggingEnabled("windows") {
		t.Fatal("file logging must remain enabled on Windows")
	}
}
