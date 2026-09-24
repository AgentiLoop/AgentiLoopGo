package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"github.com/AgentiLoop/AgentiLoopGo/mcp"
)

func TestRateExcludesTimeToFirstToken(t *testing.T) {
	ft := uint64(500)
	// 100 tokens, 500 ms to first token, 2500 ms total → 2 s generating → 50 tok/s.
	if r, ok := tokensPerSec(100, 2500, &ft); !ok || r != 50 {
		t.Fatal(r)
	}
	// No text streamed: whole call.
	if r, ok := tokensPerSec(100, 2000, nil); !ok || r != 50 {
		t.Fatal(r)
	}
	if _, ok := tokensPerSec(0, 2000, nil); ok {
		t.Fatal("zero tokens")
	}
	if _, ok := tokensPerSec(10, 0, nil); ok {
		t.Fatal("zero time")
	}
	if s := speedLine(100, 2500, &ft); s != "50.0 tok/s · ttft 0.50s · 100 tok in 2.5s" {
		t.Fatal(s)
	}
	if s := speedLine(40, 2000, nil); s != "20.0 tok/s · 40 tok in 2.0s" {
		t.Fatal(s)
	}
}

func TestCompactJSONTrimsAndKeepsHTMLChars(t *testing.T) {
	if s := compactJSON(json.RawMessage(`{ "cmd": "a && b > c" }`)); s != `{"cmd":"a && b > c"}` {
		t.Fatal(s)
	}
	long := compactJSON(json.RawMessage(`"` + strings.Repeat("é", 100) + `"`))
	if !strings.HasSuffix(long, "…") || len(long) > 124 {
		t.Fatal(long)
	}
}

func TestSettingsRoundTripAndRustFormat(t *testing.T) {
	t.Setenv("AGENTILOOP_HOME", t.TempDir())
	s := loadSettings()
	if s.ModelFor("anthropic") != "" {
		t.Fatal("empty settings")
	}
	s.SetModel("anthropic", "claude-opus-5")
	s.SetModel("omlx", "Qwen3")
	p, n, c := "omlx", 7, uint64(1000)
	s.Last = LastLaunch{Provider: &p, TUI: true, MaxTurns: &n, CompactAt: &c}
	if err := saveSettings(&s); err != nil {
		t.Fatal(err)
	}
	back := loadSettings()
	if back.ModelFor("omlx") != "Qwen3" || back.ModelFor("anthropic") != "claude-opus-5" || !back.Last.TUI || *back.Last.MaxTurns != 7 {
		t.Fatalf("%+v", back)
	}
	// The Rust CLI's file (legacy top-level model, no "last") loads too.
	os.WriteFile(settingsPath(), []byte(`{"model":"claude-x","models":{}}`), 0o644)
	if back = loadSettings(); back.ModelFor("anthropic") != "claude-x" || back.Last.Provider != nil {
		t.Fatalf("%+v", back)
	}
	// Garbage is ignored, not fatal.
	os.WriteFile(settingsPath(), []byte(`{nope`), 0o644)
	if back = loadSettings(); back.ModelFor("anthropic") != "" {
		t.Fatal("garbage")
	}
}

func TestParseArgs(t *testing.T) {
	for _, k := range []string{"AGENTILOOP_PROVIDER", "AGENTILOOP_MODEL", "AGENTILOOP_YES", "AGENTILOOP_TUI", "AGENTILOOP_NO_MCP", "AGENTILOOP_COMPACT_AT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	var out bytes.Buffer
	c, done, err := parseArgs([]string{"-p", "omlx", "fix", "--max-turns", "3", "the", "bug", "-C", "/tmp"}, &out)
	if err != nil || done || c.provider != "omlx" || *c.maxTurns != 3 || c.cwd != "/tmp" || !reflect.DeepEqual(c.prompt, []string{"fix", "the", "bug"}) {
		t.Fatalf("%v %+v", err, c)
	}
	if c.compactAt != nil {
		t.Fatal("compact-at should be unset")
	}
	t.Setenv("AGENTILOOP_TUI", "1")
	t.Setenv("AGENTILOOP_COMPACT_AT", "42")
	t.Setenv("AGENTILOOP_MODEL", "m1")
	c, _, err = parseArgs([]string{"-m", "m2"}, &out)
	if err != nil || !c.tui || *c.compactAt != 42 || c.model != "m2" {
		t.Fatalf("%v %+v", err, c)
	}
	os.Unsetenv("AGENTILOOP_TUI")
	for _, bad := range [][]string{{"--new", "-c"}, {"-r", "x", "-c"}, {"--tui", "hi"}, {"--tui", "--no-tui"}, {"--bogus"}} {
		if _, _, err := parseArgs(bad, &out); err == nil {
			t.Fatal("expected error for", bad)
		}
	}
	if _, done, _ := parseArgs([]string{"-V"}, &out); !done || !strings.Contains(out.String(), "agentiloop "+version) {
		t.Fatal(out.String())
	}
}

func TestReplayRebuildsTranscript(t *testing.T) {
	history := []core.Message{
		core.UserText("list files"),
		{Role: core.RoleAssistant, Content: []core.ContentBlock{core.TextBlock("sure"), core.ToolUseBlock("t1", "list_dir", json.RawMessage(`{}`))}},
		core.ToolResults([]core.ContentBlock{core.ToolResultBlock("t1", "a.go", false)}),
		{Role: core.RoleAssistant, Content: []core.ContentBlock{core.TextBlock("  ")}},
	}
	var users []string
	var evs []core.Event
	replay(history, func(s string) { users = append(users, s) }, func(e core.Event) { evs = append(evs, e) })
	if !reflect.DeepEqual(users, []string{"list files"}) || len(evs) != 3 {
		t.Fatalf("%v %#v", users, evs)
	}
	if r, ok := evs[2].(core.EvToolResult); !ok || r.Name != "list_dir" || r.Output != "a.go" {
		t.Fatalf("%#v", evs[2])
	}
}

// fakeProvider answers every request with fixed text and lists two models.
type fakeProvider struct{}

func (fakeProvider) Name() string         { return "openai" }
func (fakeProvider) DefaultModel() string { return "m-default" }
func (fakeProvider) ListModels(context.Context) ([]core.ModelInfo, error) {
	return []core.ModelInfo{{ID: "m1", DisplayName: "M1"}, {ID: "m2", DisplayName: "M2"}}, nil
}
func (fakeProvider) Complete(context.Context, core.ProviderRequest) (core.ProviderResponse, error) {
	return core.ProviderResponse{Message: core.Message{Role: core.RoleAssistant, Content: []core.ContentBlock{core.TextBlock("SUMMARY")}}, StopReason: core.StopEndTurn}, nil
}
func (p fakeProvider) CompleteStream(ctx context.Context, r core.ProviderRequest, on func(string)) (core.ProviderResponse, error) {
	return core.CompleteAsStream(ctx, p, r, on)
}

func TestSlashCommands(t *testing.T) {
	t.Setenv("AGENTILOOP_HOME", t.TempDir())
	ctx := context.Background()
	saved := loadSettings()
	cfg := core.DefaultAgentConfig()
	cfg.Model = "m-default"
	agent := core.NewAgent(fakeProvider{}, core.NewToolRegistry(), core.AllowAll{}, cfg, core.ToolContext{Cwd: "/proj"})
	st := &cmdState{agent: agent, provider: fakeProvider{}, saved: &saved, session: core.NewSession("/proj", "openai", "m-default"), sessionsDir: sessionsDir(), mcp: &mcp.Manager{}}
	var out []string
	say := func(s string) { out = append(out, s) }
	cmd := func(line string) string {
		out = nil
		if err := st.slashCommand(ctx, line, say); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, "\n")
	}

	if s := cmd("/model"); !strings.Contains(s, " 1. M1") || !strings.Contains(s, "pick with /model <n|id>") {
		t.Fatal(s)
	}
	if s := cmd("/model 2"); s != "model: m2" || agent.Model() != "m2" || loadSettings().ModelFor("openai") != "m2" {
		t.Fatal(s)
	}
	if s := cmd("/model 9"); !strings.Contains(s, "out of range [1-2]") {
		t.Fatal(s)
	}
	if s := cmd("/model custom-id"); s != "model: custom-id" {
		t.Fatal(s)
	}
	if s := cmd("/compact"); s != "nothing to compact" {
		t.Fatal(s)
	}

	// Build a session, persist it, clear, then resume it by number.
	if err := agent.Run(ctx, "first question", func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	st.persist()
	first := st.session.ID
	if s := cmd("/sessions"); !strings.Contains(s, "*  1. "+first) || !strings.Contains(s, "first question") {
		t.Fatal(s)
	}
	if s := cmd("/compact"); !strings.Contains(s, "context compacted") {
		t.Fatal(s)
	}
	if s := cmd("/clear"); !strings.Contains(s, "new session") || len(agent.History) != 0 {
		t.Fatal(s)
	}
	st.session.ID = "zz-second" // distinct id even within the same second
	if s := cmd("/resume 1"); !strings.Contains(s, "resumed session "+first) || st.session.ID != first || len(agent.History) != 2 {
		t.Fatal(s)
	}
	if s := cmd("/resume 5"); s != "no session #5" {
		t.Fatal(s)
	}
	if s := cmd("/resume"); !strings.HasPrefix(s, "usage: /resume") {
		t.Fatal(s)
	}
	if s := cmd("/mcp"); !strings.Contains(s, "no MCP servers configured") {
		t.Fatal(s)
	}
	if s := cmd("/help"); !strings.Contains(s, "/resume <id|n>") {
		t.Fatal(s)
	}
	if s := cmd("/wat"); s != "unknown command /wat (try /help)" {
		t.Fatal(s)
	}
}

func TestInteractivePolicyPrompts(t *testing.T) {
	var out bytes.Buffer
	p := &interactivePolicy{always: map[string]bool{}, in: strings.NewReader("y\nn\na\n"), out: &out}
	ctx := context.Background()
	in := json.RawMessage(`{"command":"ls"}`)
	if p.Check(ctx, "read_file", false, in) != core.Allow {
		t.Fatal("non-mutating")
	}
	if p.Check(ctx, "bash", true, in) != core.Allow || p.Check(ctx, "bash", true, in) != core.Deny {
		t.Fatal("y / n")
	}
	if p.Check(ctx, "write_file", true, in) != core.Allow || p.Check(ctx, "write_file", true, in) != core.Allow {
		t.Fatal("always")
	}
	if !strings.Contains(out.String(), "⚠ bash wants to run:\n{\n  \"command\": \"ls\"\n}") {
		t.Fatal(out.String())
	}
}

func TestHistoryFileFormats(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.txt")
	os.WriteFile(plain, []byte("one\n\ntwo\n"), 0o644)
	if h := loadHistory(plain); !reflect.DeepEqual(h, []string{"one", "two"}) {
		t.Fatal(h)
	}
	v2 := filepath.Join(dir, "v2.txt")
	if err := saveHistory(v2, []string{"multi\nline", `back\slash`}); err != nil {
		t.Fatal(err)
	}
	if h := loadHistory(v2); !reflect.DeepEqual(h, []string{"multi\nline", `back\slash`}) {
		t.Fatal(h)
	}
	if loadHistory(filepath.Join(dir, "missing")) != nil {
		t.Fatal("missing file")
	}
}
