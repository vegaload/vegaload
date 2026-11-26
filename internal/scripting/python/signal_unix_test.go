//go:build !windows

package python

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// Ctrl-C in a terminal reaches every process in the group. The VU must live
// on so that vegaload can run teardown after it stops the load.
func TestVU_OutlivesCtrlC(t *testing.T) {
	vu := newTestVU(t, `
def iteration():
    pass

def teardown(data):
    pass
`)
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := vu.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("the VU died on Ctrl-C: %v", err)
	}
	if err := vu.Teardown(context.Background(), nil); err != nil {
		t.Fatalf("teardown after Ctrl-C: %v", err)
	}
}
