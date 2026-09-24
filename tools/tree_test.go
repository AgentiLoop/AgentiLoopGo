//go:build !windows

package tools

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBashTimeoutKillsChildren(t *testing.T) {
	_, err := call(t, Bash{}, t.TempDir(), `{"command":"sleep 7.123 & sleep 7.123; wait","timeout_secs":1}`)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if out, _ := exec.Command("pgrep", "-f", "sleep 7.123").Output(); len(out) > 0 {
		t.Fatalf("children survived: %s", out)
	}
}
