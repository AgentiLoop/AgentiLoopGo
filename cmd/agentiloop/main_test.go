package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"github.com/AgentiLoop/AgentiLoopGo/mcp"
	"github.com/AgentiLoop/AgentiLoopGo/tools"
	"github.com/peterh/liner"
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

func TestCtrlCAtPermissionPromptCancelsTheRequest(t *testing.T) {
	cancelled := false
	p := &interactivePolicy{always: map[string]bool{}, out: &bytes.Buffer{},
		ask:    func(string) (string, error) { return "", liner.ErrPromptAborted },
		cancel: func() { cancelled = true }}
	if p.Check(context.Background(), "bash", true, json.RawMessage(`{}`)) != core.Deny || !cancelled {
		t.Fatal("Ctrl-C at the prompt should cancel the whole request")
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

func TestUsageLineTotalsTokens(t *testing.T) {
	u := core.Usage{Requests: 3, InputTokens: 1200, OutputTokens: 340}
	if got := usageLine(core.Usage{Requests: 3, InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 900, CacheWriteTokens: 50}); got != "3 request(s) since start: 1200 input + 340 output = 1540 tokens (prompt cache: 900 read, 50 written)" {
		t.Fatal(got)
	}
	if got := usageLine(u); got != "3 request(s) since start: 1200 input + 340 output = 1540 tokens" {
		t.Fatal(got)
	}
}

func TestExpandStdinReplacesLoneDash(t *testing.T) {
	never := func() (string, error) { t.Fatal("stdin must not be read"); return "", nil }
	for _, c := range []struct {
		words []string
		want  string
	}{{[]string{"fix", "the", "bug"}, "fix the bug"}, {[]string{"a-b", "--x"}, "a-b --x"}} {
		if got, err := expandStdin(c.words, never); err != nil || got != c.want {
			t.Fatalf("%v: got %q, %v", c.words, got, err)
		}
	}
	if got, err := expandStdin([]string{"review", "-"}, func() (string, error) { return "diff text\n\n", nil }); err != nil || got != "review\n\ndiff text" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := expandStdin([]string{"-"}, func() (string, error) { return "only stdin", nil }); err != nil || got != "only stdin" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := expandStdin([]string{"-"}, func() (string, error) { return "  \n", nil }); err == nil {
		t.Fatal("empty stdin accepted")
	}
	if _, err := expandStdin([]string{"x", "-"}, func() (string, error) { return "", errors.New("closed") }); err == nil {
		t.Fatal("read error swallowed")
	}
}

func TestGitChangesReportsNonRepoCleanAndChangedTrees(t *testing.T) {
	d := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", d, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitChanges(d); err == nil {
		t.Skip("temp dir is inside a git repository")
	}
	git("init", "-q")
	if got, err := gitChanges(d); err != nil || got != "no changes in the git working tree" {
		t.Fatalf("clean: %q, %v", got, err)
	}
	write("a.txt", "one\n")
	git("add", "a.txt")
	git("commit", "-q", "-m", "first")

	write("a.txt", "two\n")
	write("new.txt", "x\n")
	out, err := gitChanges(d)
	if err != nil || !strings.Contains(out, " M a.txt") || !strings.Contains(out, "?? new.txt") ||
		!strings.Contains(out, "-one") || !strings.Contains(out, "+two") {
		t.Fatalf("changed: %q, %v", out, err)
	}

	write("a.txt", strings.Repeat("line\n", 300))
	if out, _ := gitChanges(d); !strings.Contains(out, "more lines") {
		t.Fatalf("long diff not cut: %q", out)
	}
}

func TestJSONResultHasStableKeysAndErrorOnlyOnFailure(t *testing.T) {
	got := jsonResult("a", nil, "s", "p", "m", core.Usage{Requests: 1, InputTokens: 2, OutputTokens: 3})
	want := `{"is_error":false,"model":"m","provider":"p","result":"a","session_id":"s","usage":{"cache_read_tokens":0,"cache_write_tokens":0,"input_tokens":2,"output_tokens":3,"requests":1}}`
	if got != want {
		t.Fatalf("got %s", got)
	}
	var ok map[string]any
	if err := json.Unmarshal([]byte(jsonResult("hi \"x\"\n <b>", nil, "s1", "omlx", "m", core.Usage{})), &ok); err != nil || ok["result"] != "hi \"x\"\n <b>" {
		t.Fatal(ok, err)
	}
	var bad map[string]any
	if err := json.Unmarshal([]byte(jsonResult("", errors.New("boom"), "s1", "omlx", "m", core.Usage{})), &bad); err != nil || bad["is_error"] != true || bad["error"] != "boom" {
		t.Fatal(bad, err)
	}
	if _, _, err := parseArgs([]string{"--stream-json"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--stream-json without a prompt accepted")
	}
	if _, _, err := parseArgs([]string{"--json", "--stream-json", "x"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--json with --stream-json accepted")
	}
	if _, _, err := parseArgs([]string{"--json"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--json without a prompt accepted")
	}
	if c, _, err := parseArgs([]string{"--json", "hi"}, &bytes.Buffer{}); err != nil || !c.jsonOut {
		t.Fatal(err)
	}
}

func TestAppendExtraOnlyWhenNotBlank(t *testing.T) {
	if appendExtra("base", "") != "base" || appendExtra("base", "  \n") != "base" {
		t.Fatal("blank extra changed the prompt")
	}
	if got := appendExtra("base", " Answer in French. "); got != "base\n\nAnswer in French." {
		t.Fatal(got)
	}
	t.Setenv("AGENTILOOP_APPEND_SYSTEM_PROMPT", "from env")
	if c, _, err := parseArgs(nil, &bytes.Buffer{}); err != nil || c.appendPrompt != "from env" {
		t.Fatal(c, err)
	}
	if c, _, err := parseArgs([]string{"--append-system-prompt", "flag"}, &bytes.Buffer{}); err != nil || c.appendPrompt != "flag" {
		t.Fatal(c, err)
	}
}

func TestToolRuleFlagsAreRepeatableAndCommaSeparated(t *testing.T) {
	c, _, err := parseArgs([]string{"--allow-tool", "write_file,edit_file", "--allow-tool", "mcp_*", "--deny-tool", "bash", "hi"}, &bytes.Buffer{})
	if err != nil || !reflect.DeepEqual(c.allowTools, []string{"write_file", "edit_file", "mcp_*"}) || !reflect.DeepEqual(c.denyTools, []string{"bash"}) {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestStreamJSONLinesHaveATypeAndTheEventFields(t *testing.T) {
	ms := uint64(900)
	cases := map[string]core.Event{
		`{"text":"hi","type":"text_delta"}`:                                                          core.EvTextDelta{Text: "hi"},
		`{"text":"hi there","type":"text"}`:                                                          core.EvText{Text: "hi there"},
		`{"id":"t1","input":{"command":"ls"},"name":"bash","type":"tool_call"}`:                      core.EvToolCall{ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		`{"id":"t1","is_error":false,"name":"bash","output":"a\nb","type":"tool_result"}`:            core.EvToolResult{ID: "t1", Name: "bash", Output: "a\nb"},
		`{"elapsed_ms":900,"first_token_ms":null,"input_tokens":10,"output_tokens":5,"type":"turn"}`: core.EvTurnComplete{InputTokens: 10, OutputTokens: 5, ElapsedMs: 900},
		`{"elapsed_ms":900,"first_token_ms":900,"input_tokens":10,"output_tokens":5,"type":"turn"}`:  core.EvTurnComplete{InputTokens: 10, OutputTokens: 5, ElapsedMs: 900, FirstTokenMs: &ms},
		`{"before_tokens":100,"messages_dropped":4,"type":"compacted"}`:                              core.EvCompacted{BeforeTokens: 100, MessagesDropped: 4},
		`{"stop_reason":"end_turn","type":"done"}`:                                                   core.EvDone{StopReason: core.StopEndTurn},
		`{"attempt":1,"delay_ms":1000,"error":"Anthropic 529","max_retries":3,"type":"retry"}`:       core.EvRetry{Attempt: 1, MaxRetries: 3, DelayMs: 1000, Error: "Anthropic 529"},
	}
	for want, ev := range cases {
		if got := eventJSON(ev); got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
	v := jsonResultValue("a", nil, "s", "p", "m", core.Usage{})
	v["type"] = "result"
	if got := core.MarshalString(v, ""); !strings.Contains(got, `"type":"result"`) || !strings.Contains(got, `"result":"a"`) {
		t.Fatal(got)
	}
}

func TestRetryLineKeepsTheFirstLineOfTheError(t *testing.T) {
	if got := retryLine(2, 3, 2000, "Anthropic 529 Overloaded\ndetail"); got != "\u27f3 retry 2/3 in 2s: Anthropic 529 Overloaded" {
		t.Fatal(got)
	}
	if got := retryLine(1, 3, 1000, strings.Repeat("x", 300)); !strings.HasSuffix(got, "…") {
		t.Fatal(got)
	}
}

func TestMaxRetriesFlagAndEnv(t *testing.T) {
	var out strings.Builder
	c, _, err := parseArgs([]string{"hi"}, &out)
	if err != nil || c.maxRetries != core.DefaultMaxRetries {
		t.Fatalf("%+v %v", c, err)
	}
	c, _, err = parseArgs([]string{"--max-retries", "0", "hi"}, &out)
	if err != nil || c.maxRetries != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("AGENTILOOP_MAX_RETRIES", "5")
	c, _, err = parseArgs([]string{"hi"}, &out)
	if err != nil || c.maxRetries != 5 {
		t.Fatalf("%+v %v", c, err)
	}
	c, _, err = parseArgs([]string{"--max-retries", "1", "hi"}, &out)
	if err != nil || c.maxRetries != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("AGENTILOOP_MAX_RETRIES", "-1")
	if _, _, err = parseArgs([]string{"hi"}, &out); err == nil {
		t.Fatal("negative env value must be rejected")
	}
}

func TestShellLineShowsTheCommandThenTheOutput(t *testing.T) {
	if got := shellLine("ls", "a\nb\n"); got != "$ ls\na\nb" {
		t.Fatal(got)
	}
	if got := shellLine("true", ""); got != "$ true\n(no output)" {
		t.Fatal(got)
	}
}

func TestStatusListsEveryItemAndAdaptsToWhatIsKnown(t *testing.T) {
	info := statusInfo{cwd: "/p", provider: "anthropic", model: "m", sessionID: "s1", tools: []string{"read_file", "bash"}, maxTurns: 50, maxRetries: 3}
	has := func(text string, parts ...string) {
		t.Helper()
		for _, p := range parts {
			if !strings.Contains(text, p) {
				t.Fatalf("missing %q in:\n%s", p, text)
			}
		}
	}
	has(statusText(info), "folder:        /p", "provider:      anthropic", "model:         m  (limits are looked up on the first request)",
		"session:       s1  (0 messages)", "plan mode:     off", "tools:         2: read_file, bash\n", "hooks:         none",
		"instructions:  none (/init creates AGENTS.md)", "settings:      max turns 50, retries 3", "usage:         0 request(s) since start")
	win := uint64(200_000)
	info.limits = &core.ModelLimits{MaxTokens: 8192, ContextWindow: &win, CompactAtTokens: 160_000}
	info.messages, info.lastInputTokens, info.planMode = 4, 1234, true
	info.mcpServers, info.mcpTools, info.hooks = 1, 1, 2
	info.instructions = []string{"/p/AGENTS.md"}
	has(statusText(info), "model:         m  (max output 8192 tokens, context 200000, compact at 160000)",
		"session:       s1  (4 messages, last request 1234 input tokens)", "plan mode:     on (/plan off to run tools again)",
		"tools:         2: read_file, bash  (1 of them from 1 MCP server(s))", "hooks:         2 (/hooks lists them)", "instructions:  /p/AGENTS.md")
	info.limits = &core.ModelLimits{MaxTokens: 8192}
	has(statusText(info), "model:         m  (max output 8192 tokens, compaction off)")
}

func TestRewindListingNumbersPromptsAndCountsChangedFiles(t *testing.T) {
	if got, want := promptTitle("  "+strings.Repeat("x", 70)+"\nmore"), strings.Repeat("x", 59)+"…"; got != want {
		t.Fatalf("promptTitle = %q, want %q", got, want)
	}
	got := rewindListing([]tools.RewindPrompt{{Text: "fix the bug\nin main", Files: 2}, {Text: "what is this?"}, {Text: "add a test", Files: 1}})
	want := "/rewind <n> goes back to before prompt n: its file changes and everything after it are undone\n  1. fix the bug  (2 files changed)\n  2. what is this?\n  3. add a test  (1 file changed)"
	if got != want {
		t.Fatalf("rewindListing =\n%s\nwant\n%s", got, want)
	}
}
