package cmd

import (
	"os"
	"testing"
)

// TestMain stops log-hook from launching background `process` runs during
// tests (it would re-run the test binary).
func TestMain(m *testing.M) {
	startBackgroundProcess = func(string) {}
	os.Exit(m.Run())
}
