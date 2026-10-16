package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func hooksDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agentiloop"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeHooks(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".agentiloop", "hooks.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHooksLoadUserThenProjectAndReportBadEntries(t *testing.T) {
	dir := hooksDir(t)
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "hooks.json"), []byte(`{"hooks":{"Stop":[{"command":"echo done"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeHooks(t, dir, `{"hooks":{"PreToolUse":[{"matcher":"bash, mcp_*","command":"check"},{"command":""}],"Weird":[{"command":"x"}]}}`)
	hooks, errs := LoadHooks(HookPaths(dir, home))
	if len(hooks) != 2 || hooks[0].Event != StopHook || hooks[1].Event != PreToolUse {
		t.Fatalf("hooks: %+v", hooks)
	}
	if !hooks[1].applies(PreToolUse, "bash") || !hooks[1].applies(PreToolUse, "mcp_x_y") || hooks[1].applies(PreToolUse, "edit_file") || hooks[1].applies(PostToolUse, "bash") {
		t.Fatal("matcher")
	}
	if len(errs) != 2 || !strings.Contains(errs[0], "empty command") || !strings.Contains(errs[1], "unknown hook event `Weird`") {
		t.Fatalf("errs: %v", errs)
	}
	if l := hooks.Listing(); !strings.Contains(l, "PreToolUse   bash,mcp_*") {
		t.Fatal(l)
	}
}

func TestHooksMissingFilesAndBrokenJSON(t *testing.T) {
	dir := hooksDir(t)
	hooks, errs := LoadHooks(HookPaths(dir, ""))
	if len(hooks) != 0 || len(errs) != 0 || !strings.HasPrefix(hooks.Listing(), "no hooks configured") {
		t.Fatal(hooks, errs)
	}
	writeHooks(t, dir, "{not json")
	hooks, errs = LoadHooks(HookPaths(dir, ""))
	if len(hooks) != 0 || len(errs) != 1 || !strings.Contains(errs[0], "hooks.json") {
		t.Fatal(hooks, errs)
	}
}

func TestPreHookBlocksOnExit2AndReadsThePayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	dir := hooksDir(t)
	writeHooks(t, dir, `{"hooks":{"PreToolUse":[
		{"matcher":"bash","command":"grep -q rm && { echo \"no rm, tool=$AGENTILOOP_TOOL\" >&2; exit 2; }; exit 0"},
		{"matcher":"edit_file","command":"exit 1"}
	]}}`)
	hooks, errs := LoadHooks(HookPaths(dir, ""))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	ctx := context.Background()
	fb := hooks.RunPre(ctx, dir, "bash", json.RawMessage(`{"command":"rm -rf x"}`))
	if fb == nil || fb.Message != "no rm, tool=bash" {
		t.Fatalf("got %+v", fb)
	}
	if hooks.RunPre(ctx, dir, "bash", json.RawMessage(`{"command":"ls"}`)) != nil {
		t.Fatal("ls must pass")
	}
	// Exit 1 and unmatched tools never block.
	if hooks.RunPre(ctx, dir, "edit_file", nil) != nil || hooks.RunPre(ctx, dir, "write_file", nil) != nil {
		t.Fatal("must not block")
	}
}

func TestPostHookFeedbackAndStopHookSideEffect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	dir := hooksDir(t)
	writeHooks(t, dir, `{"hooks":{
		"PostToolUse":[{"command":"grep -q '\"is_error\":true' && exit 0; echo remember-to-test; exit 2"}],
		"Stop":[{"command":"cat > stop.json"}]
	}}`)
	hooks, _ := LoadHooks(HookPaths(dir, ""))
	ctx := context.Background()
	fb := hooks.RunPost(ctx, dir, "edit_file", nil, "ok", false)
	if len(fb) != 1 || fb[0].Message != "remember-to-test" {
		t.Fatalf("got %+v", fb)
	}
	if fb := hooks.RunPost(ctx, dir, "edit_file", nil, "boom", true); len(fb) != 0 {
		t.Fatal(fb)
	}
	hooks.RunStop(ctx, dir, StopEndTurn)
	b, err := os.ReadFile(filepath.Join(dir, "stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stop map[string]any
	if err := json.Unmarshal(b, &stop); err != nil || stop["event"] != "Stop" || stop["stop_reason"] != "end_turn" {
		t.Fatal(string(b), err)
	}
}

func TestCutHookKeepsRuneBoundaries(t *testing.T) {
	if cutHook("héllo", 100) != "héllo" || !strings.HasPrefix(cutHook("héllo", 2), "h…") {
		t.Fatal(cutHook("héllo", 2))
	}
}
