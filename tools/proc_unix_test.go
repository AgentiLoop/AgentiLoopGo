//go:build !windows

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

// Cancelling the context is what Esc does to a running tool.
func TestBashCancelKillsTheWholeProcessTree(t *testing.T) {
	d := t.TempDir()
	pidfile := filepath.Join(d, "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	input, _ := json.Marshal(map[string]string{"command": fmt.Sprintf("sleep 30 & echo $! > %s; wait", pidfile)})
	Bash{}.Call(ctx, core.ToolContext{Cwd: d}, input)
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild %d outlived the cancelled command", pid)
}

func TestBashDoesNotWaitForBackgroundChildrenHoldingThePipes(t *testing.T) {
	started := time.Now()
	out, err := Bash{}.Call(context.Background(), core.ToolContext{Cwd: t.TempDir()}, json.RawMessage(`{"command":"sleep 30 & echo started"}`))
	if err != nil || !strings.Contains(out, "started") {
		t.Fatalf("%v %q", err, out)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("waited %v", time.Since(started))
	}
}
