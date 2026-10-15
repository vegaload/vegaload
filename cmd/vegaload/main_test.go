package main

import (
	"os"
	"testing"
)

// TestMain keeps the tests away from the machine they run on: the live view
// is off unless a test turns it on, no browser is opened, and the live view's
// identity goes to a throwaway directory. Without this, `go test` would stream
// to a service running on the developer's machine, open browser tabs, and write
// into their real config directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vegaload-test-config-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("VEGALOAD_CONFIG_DIR", dir)
	os.Setenv("VEGALOAD_LIVE", "off")
	os.Setenv("VEGALOAD_OPEN", "off")
	for _, k := range []string{"VEGALOAD_LIVE_URL", "VEGALOAD_STREAM_FILE", "VEGALOAD_STREAM_PUSH", "VEGALOAD_STREAM_TOKEN", "VEGALOAD_RUN_ID"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
