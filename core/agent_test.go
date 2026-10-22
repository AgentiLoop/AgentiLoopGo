package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/AgentiLoop/AgentiLoopGo/core"
)

// scripted replays a fixed list of responses, recording every request it received.
type scripted struct {
	mu        sync.Mutex
	responses []ProviderResponse
	requests  []ProviderRequest
}

func (p *scripted) Name() string         { return "scripted" }
func (p *scripted) DefaultModel() string { return "mock" }

// ListModels reports limits for `mock` only; other ids are unknown to the catalog.
func (p *scripted) ListModels(context.Context) ([]ModelInfo, error) {
	win, out := uint64(1_000_000), 128_000
	smallWin, smallOut := uint64(8192), 8192 // gpt-4: output cap equals the whole window
	return []ModelInfo{
		{ID: "mock", DisplayName: "Mock", MaxInputTokens: &win, MaxTokens: &out},
		{ID: "small", DisplayName: "Small", MaxInputTokens: &smallWin, MaxTokens: &smallOut},
	}, nil
}
func (p *scripted) Complete(_ context.Context, req ProviderRequest) (ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return ProviderResponse{}, errors.New("scripted provider ran out of responses")
	}
	r := p.responses[0]
	p.responses = p.responses[1:]
	return r, nil
}
func (p *scripted) CompleteStream(ctx context.Context, req ProviderRequest, on func(string)) (ProviderResponse, error) {
	return CompleteAsStream(ctx, p, req, on)
}
func (p *scripted) reqs() []ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProviderRequest(nil), p.requests...)
}

func text(t string) ProviderResponse {
	return ProviderResponse{
		Message:    Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock(t)}},
		StopReason: StopEndTurn, InputTokens: 10, OutputTokens: 5,
	}
}

func toolCall(id, name, input string) ProviderResponse {
	return ProviderResponse{
		Message:    Message{Role: RoleAssistant, Content: []ContentBlock{ToolUseBlock(id, name, json.RawMessage(input))}},
		StopReason: StopToolUse, InputTokens: 10, OutputTokens: 5,
	}
}

// echo echoes its `msg` argument back; mutating so it exercises the permission gate.
type echo struct{}

func (echo) Name() string        { return "echo" }
func (echo) Description() string { return "echo" }
func (echo) InputSchema() any {
	return map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}}
}
func (echo) IsMutating() bool { return true }
func (echo) Call(_ context.Context, _ ToolContext, input json.RawMessage) (string, error) {
	var in struct{ Msg string }
	_ = json.Unmarshal(input, &in)
	return "echo:" + in.Msg, nil
}

type policyFunc func(tool string, mutating bool) Permission

func (f policyFunc) Check(_ context.Context, tool string, mutating bool, _ json.RawMessage) Permission {
	return f(tool, mutating)
}

func newAgent(p *scripted, policy PermissionPolicy, cfg AgentConfig) *Agent {
	tools := NewToolRegistry()
	tools.Register(echo{})
	return NewAgent(p, tools, policy, cfg, ToolContext{Cwd: "."})
}

func cfgTurns(n int) AgentConfig {
	c := DefaultAgentConfig()
	c.Model, c.MaxTurns = "mock", n
	return c
}

func cfgCompact(at uint64) AgentConfig {
	c := DefaultAgentConfig()
	c.CompactAtTokens = &at
	return c
}

func collect(a *Agent, prompt string) (error, []Event) {
	var evs []Event
	err := a.Run(context.Background(), prompt, func(e Event) { evs = append(evs, e) })
	return err, evs
}

func any_(evs []Event, f func(Event) bool) bool {
	for _, e := range evs {
		if f(e) {
			return true
		}
	}
	return false
}

func TestPlainReplyEndsTurnAndRecordsHistory(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{text("hello")}}
	a := newAgent(p, AllowAll{}, cfgTurns(5))
	err, evs := collect(a, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.History) != 2 || a.History[0].Role != RoleUser || a.History[1].Text() != "hello" {
		t.Fatalf("history: %+v", a.History)
	}
	if d, ok := evs[0].(EvTextDelta); !ok || d.Text != "hello" {
		t.Fatalf("first event %#v", evs[0])
	}
	if !any_(evs, func(e Event) bool { x, ok := e.(EvText); return ok && x.Text == "hello" }) {
		t.Fatal("no EvText")
	}
	if d, ok := evs[len(evs)-1].(EvDone); !ok || d.StopReason != StopEndTurn {
		t.Fatalf("last event %#v", evs[len(evs)-1])
	}
	reqs := p.reqs()
	if len(reqs) != 1 || reqs[0].System != DefaultSystemPrompt || len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "echo" {
		t.Fatalf("requests: %+v", reqs)
	}
}

func TestToolCallRoundTripFeedsResultBack(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{toolCall("t1", "echo", `{"msg":"ping"}`), text("done")}}
	a := newAgent(p, AllowAll{}, cfgTurns(5))
	err, evs := collect(a, "go")
	if err != nil {
		t.Fatal(err)
	}
	if !any_(evs, func(e Event) bool { x, ok := e.(EvToolCall); return ok && x.ID == "t1" && x.Name == "echo" }) {
		t.Fatal("no tool call event")
	}
	if !any_(evs, func(e Event) bool {
		x, ok := e.(EvToolResult)
		return ok && x.ID == "t1" && x.Output == "echo:ping" && !x.IsError
	}) {
		t.Fatal("no tool result event")
	}
	if len(a.History) != 4 {
		t.Fatalf("history len %d", len(a.History))
	}
	r := a.History[2].Content[0]
	if r.Type != BlockToolResult || r.ToolUseID != "t1" || r.Content != "echo:ping" || r.IsError {
		t.Fatalf("tool result %+v", r)
	}
	reqs := p.reqs()
	if len(reqs) != 2 || len(reqs[1].Messages) != 3 {
		t.Fatalf("requests %d / %d", len(reqs), len(reqs[1].Messages))
	}
}

func TestUnknownToolIsReportedAsErrorResult(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{toolCall("t1", "nope", `{}`), text("ok")}}
	err, evs := collect(newAgent(p, AllowAll{}, cfgTurns(5)), "go")
	if err != nil {
		t.Fatal(err)
	}
	if !any_(evs, func(e Event) bool {
		x, ok := e.(EvToolResult)
		return ok && x.IsError && strings.Contains(x.Output, "unknown tool")
	}) {
		t.Fatal("expected unknown tool error")
	}
}

func TestDeniedPermissionBecomesErrorResult(t *testing.T) {
	deny := policyFunc(func(_ string, m bool) Permission {
		if m {
			return Deny
		}
		return Allow
	})
	p := &scripted{responses: []ProviderResponse{toolCall("t1", "echo", `{"msg":"x"}`), text("ok")}}
	err, evs := collect(newAgent(p, deny, cfgTurns(5)), "go")
	if err != nil {
		t.Fatal(err)
	}
	if !any_(evs, func(e Event) bool {
		x, ok := e.(EvToolResult)
		return ok && x.IsError && strings.Contains(x.Output, "permission denied")
	}) {
		t.Fatal("expected permission denied")
	}
}

func TestCancelledCallIsSkippedAndLoopContinues(t *testing.T) {
	cancel := policyFunc(func(string, bool) Permission { return Cancel })
	p := &scripted{responses: []ProviderResponse{toolCall("t1", "echo", `{"msg":"x"}`), text("carried on")}}
	err, evs := collect(newAgent(p, cancel, cfgTurns(5)), "go")
	if err != nil {
		t.Fatal(err)
	}
	if !any_(evs, func(e Event) bool {
		x, ok := e.(EvToolResult)
		return ok && x.IsError && strings.HasPrefix(x.Output, "cancelled") && strings.Contains(x.Output, "Continue")
	}) {
		t.Fatal("expected cancelled result")
	}
	if len(p.reqs()) != 2 {
		t.Fatal("model should be asked again")
	}
	if !any_(evs, func(e Event) bool { x, ok := e.(EvText); return ok && x.Text == "carried on" }) {
		t.Fatal("missing final text")
	}
}

func TestMaxTurnsStopsRunawayLoop(t *testing.T) {
	var looping []ProviderResponse
	for i := range 10 {
		looping = append(looping, toolCall("t"+string(rune('0'+i)), "echo", `{"msg":"again"}`))
	}
	p := &scripted{responses: looping}
	err, _ := collect(newAgent(p, AllowAll{}, cfgTurns(3)), "loop")
	if err == nil || !strings.Contains(err.Error(), "max_turns (3)") {
		t.Fatalf("err = %v", err)
	}
	if len(p.reqs()) != 3 {
		t.Fatalf("requests %d", len(p.reqs()))
	}
}

func TestClearDropsHistory(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{text("a"), text("b")}}
	a := newAgent(p, AllowAll{}, cfgTurns(5))
	if err, _ := collect(a, "one"); err != nil || len(a.History) != 2 {
		t.Fatal(err, len(a.History))
	}
	a.Clear()
	if len(a.History) != 0 {
		t.Fatal("not cleared")
	}
	if err, _ := collect(a, "two"); err != nil || len(a.History) != 2 {
		t.Fatal(err, len(a.History))
	}
}

// ---- compaction -------------------------------------------------------------

func big(t string, in uint64) ProviderResponse {
	r := text(t)
	r.InputTokens = in
	return r
}

func TestCompactReplacesHistoryWithSummary(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{text("first"), text("SUMMARY")}}
	a := newAgent(p, AllowAll{}, cfgCompact(0))
	if err, _ := collect(a, "do a thing"); err != nil {
		t.Fatal(err)
	}
	ev, err := a.Compact(context.Background())
	if err != nil || ev == nil || ev.MessagesDropped != 2 {
		t.Fatalf("%v %+v", err, ev)
	}
	if len(a.History) != 2 || a.History[0].Role != RoleUser || !strings.Contains(a.History[0].Text(), "SUMMARY") ||
		a.History[1].Role != RoleAssistant || a.LastInputTokens() != 0 {
		t.Fatalf("history %+v", a.History)
	}
	sum := p.reqs()[1]
	body := sum.Messages[0].Text()
	if len(sum.Tools) != 0 || len(sum.Messages) != 1 || !strings.Contains(body, "USER: do a thing") || !strings.Contains(body, "ASSISTANT: first") {
		t.Fatalf("summary request %+v", sum)
	}
}

func TestCompactOnEmptyHistoryIsNoop(t *testing.T) {
	p := &scripted{}
	a := newAgent(p, AllowAll{}, DefaultAgentConfig())
	ev, err := a.Compact(context.Background())
	if err != nil || ev != nil || len(p.reqs()) != 0 {
		t.Fatal(err, ev)
	}
}

func TestCompactFailsOnEmptySummaryAndKeepsHistory(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{text("first"), text("   ")}}
	a := newAgent(p, AllowAll{}, cfgCompact(0))
	collect(a, "x")
	_, err := a.Compact(context.Background())
	if err == nil || !strings.Contains(err.Error(), "empty summary") {
		t.Fatal(err)
	}
	if len(a.History) != 2 {
		t.Fatal("history must be untouched on failure")
	}
}

func TestAutoCompactsBeforeNextRun(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{big("first", 1000), text("SUMMARY"), text("second")}}
	a := newAgent(p, AllowAll{}, cfgCompact(500))
	collect(a, "one")
	if a.LastInputTokens() != 1000 {
		t.Fatal(a.LastInputTokens())
	}
	err, evs := collect(a, "two")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := evs[0].(EvCompacted); !ok || c.BeforeTokens != 1000 || c.MessagesDropped != 2 {
		t.Fatalf("%#v", evs[0])
	}
	if len(a.History) != 4 || !strings.Contains(a.History[0].Text(), "SUMMARY") || a.History[2].Text() != "two" || a.History[3].Text() != "second" {
		t.Fatalf("%+v", a.History)
	}
	reqs := p.reqs()
	if len(reqs) != 3 || len(reqs[2].Messages) != 3 || !strings.Contains(reqs[2].Messages[0].Text(), "SUMMARY") {
		t.Fatal("request 3 not built from compacted history")
	}
}

func TestAutoCompactsMidLoopAfterToolResults(t *testing.T) {
	call := toolCall("t1", "echo", `{"msg":"a"}`)
	call.InputTokens = 900
	p := &scripted{responses: []ProviderResponse{call, text("SUMMARY"), text("finished")}}
	a := newAgent(p, AllowAll{}, cfgCompact(500))
	err, evs := collect(a, "go")
	if err != nil {
		t.Fatal(err)
	}
	idx := -1
	for i, e := range evs {
		if c, ok := e.(EvCompacted); ok && c.BeforeTokens == 900 {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("not compacted")
	}
	if !any_(evs[:idx], func(e Event) bool { _, ok := e.(EvToolResult); return ok }) {
		t.Fatal("compaction should follow the tool result")
	}
	if !any_(evs[idx:], func(e Event) bool { x, ok := e.(EvText); return ok && x.Text == "finished" }) {
		t.Fatal("final answer should follow compaction")
	}
	if len(a.History) != 4 || !strings.Contains(a.History[2].Text(), "Continue the task") || a.History[3].Text() != "finished" {
		t.Fatalf("%+v", a.History)
	}
	reqs := p.reqs()
	if len(reqs) != 3 {
		t.Fatal(len(reqs))
	}
	for _, m := range reqs[2].Messages {
		if len(m.ToolUses()) != 0 {
			t.Fatal("old tool_use blocks must be gone")
		}
	}
}

func TestCompactionDisabledWhenThresholdIsZero(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{big("first", 1_000_000), text("second")}}
	a := newAgent(p, AllowAll{}, cfgCompact(0))
	collect(a, "one")
	_, evs := collect(a, "two")
	if any_(evs, func(e Event) bool { _, ok := e.(EvCompacted); return ok }) || len(p.reqs()) != 2 {
		t.Fatal("should not compact")
	}
}

func TestTranscriptTrimsLongToolResults(t *testing.T) {
	history := []Message{
		UserText("hi"),
		{Role: RoleAssistant, Content: []ContentBlock{ToolUseBlock("t", "bash", json.RawMessage(`{"command": "ls"}`))}},
		ToolResults([]ContentBlock{ToolResultBlock("t", strings.Repeat("x", 5000), true)}),
	}
	tr := Transcript(history)
	for _, want := range []string{"USER: hi", `ASSISTANT → tool bash {"command":"ls"}`, "tool error: ", "…[3000 more bytes]"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("missing %q in %s", want, tr)
		}
	}
	if len(tr) >= 2500 {
		t.Fatal(len(tr))
	}
}

func TestLimitsComeFromTheProviderCatalog(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{text("ok"), text("ok")}}
	a := newAgent(p, AllowAll{}, cfgTurns(5))
	if a.Limits() != nil {
		t.Fatal("limits resolved before the first run")
	}
	if err, _ := collect(a, "hi"); err != nil {
		t.Fatal(err)
	}

	// max_tokens on the wire is the model's own cap; compaction at 80% of its window.
	l := a.Limits()
	if l == nil || l.MaxTokens != 128_000 || l.ContextWindow == nil || *l.ContextWindow != 1_000_000 || l.CompactAtTokens != 800_000 {
		t.Fatalf("limits: %+v", l)
	}
	if got := p.reqs()[0].MaxTokens; got != 128_000 {
		t.Fatal(got)
	}

	// Unknown model: limits are cleared and the fallbacks apply.
	a.SetModel("other")
	if a.Limits() != nil {
		t.Fatal("SetModel did not clear limits")
	}
	if err, _ := collect(a, "hi"); err != nil {
		t.Fatal(err)
	}
	l = a.Limits()
	if l == nil || l.MaxTokens != FallbackMaxTokens || l.ContextWindow != nil || l.CompactAtTokens != FallbackCompactAt {
		t.Fatalf("limits: %+v", l)
	}
	if got := p.reqs()[1].MaxTokens; got != FallbackMaxTokens {
		t.Fatal(got)
	}
}

func TestConfigOverridesBeatTheCatalog(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{text("ok")}}
	maxTokens, compactAt := 1_000, uint64(0)
	c := cfgTurns(5)
	c.MaxTokens, c.CompactAtTokens = &maxTokens, &compactAt
	a := newAgent(p, AllowAll{}, c)
	if err, _ := collect(a, "hi"); err != nil {
		t.Fatal(err)
	}
	l := a.Limits()
	if l == nil || l.MaxTokens != 1_000 || l.CompactAtTokens != 0 || l.ContextWindow == nil || *l.ContextWindow != 1_000_000 {
		t.Fatalf("limits: %+v", l)
	}
	if got := p.reqs()[0].MaxTokens; got != 1_000 {
		t.Fatal(got)
	}
}

func TestUsageAccumulatesAcrossRequestsAndSurvivesClear(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{toolCall("t1", "echo", `{"msg":"x"}`), text("done"), text("SUMMARY")}}
	a := newAgent(p, AllowAll{}, cfgTurns(5))
	if a.Usage() != (Usage{}) {
		t.Fatal(a.Usage())
	}
	if err, _ := collect(a, "go"); err != nil {
		t.Fatal(err)
	}
	if u := a.Usage(); u != (Usage{Requests: 2, InputTokens: 20, OutputTokens: 10}) {
		t.Fatalf("%+v", u)
	}
	// A compaction request is spent tokens too.
	if _, err := a.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u := a.Usage(); u != (Usage{Requests: 3, InputTokens: 30, OutputTokens: 15}) {
		t.Fatalf("%+v", u)
	}
	a.Clear()
	if a.Usage().Requests != 3 {
		t.Fatal("Clear must keep the running total")
	}
}

func writeHooksFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".agentiloop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agentiloop", "hooks.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentHooksBlockCallsAddFeedbackAndRunOnStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	dir := t.TempDir()
	writeHooksFile(t, dir, `{"hooks":{
		"PreToolUse":[{"matcher":"echo","command":"grep -q forbidden && { echo 'not that' >&2; exit 2; }; exit 0"}],
		"PostToolUse":[{"matcher":"echo","command":"echo looks good; exit 2"}],
		"Stop":[{"command":"cat > stop.json"}]
	}}`)
	hooks, errs := LoadHooks(HookPaths(dir, ""))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	p := &scripted{responses: []ProviderResponse{
		toolCall("t1", "echo", `{"msg":"forbidden"}`),
		toolCall("t2", "echo", `{"msg":"fine"}`),
		text("done"),
	}}
	cfg := cfgTurns(5)
	cfg.Hooks = hooks
	tools := NewToolRegistry()
	tools.Register(echo{})
	a := NewAgent(p, tools, AllowAll{}, cfg, ToolContext{Cwd: dir})
	err, evs := collect(a, "go")
	if err != nil {
		t.Fatal(err)
	}
	var results []EvToolResult
	for _, e := range evs {
		if r, ok := e.(EvToolResult); ok {
			results = append(results, r)
		}
	}
	if len(results) != 2 {
		t.Fatalf("results: %+v", results)
	}
	if !results[0].IsError || !strings.Contains(results[0].Output, "blocked by a PreToolUse hook") || !strings.Contains(results[0].Output, "not that") {
		t.Fatalf("first: %+v", results[0])
	}
	if results[1].IsError || !strings.HasPrefix(results[1].Output, "echo:fine") || !strings.Contains(results[1].Output, "[PostToolUse hook `echo looks good; exit 2`]: looks good") {
		t.Fatalf("second: %+v", results[1])
	}
	b, err := os.ReadFile(filepath.Join(dir, "stop.json"))
	if err != nil || !strings.Contains(string(b), `"stop_reason":"end_turn"`) {
		t.Fatal(string(b), err)
	}
}

func TestTaskToolRunsAChildAgentAndReturnsOnlyItsAnswer(t *testing.T) {
	// Parent asks for a task; the child (same scripted provider) calls echo, then answers;
	// the parent then finishes.
	p := &scripted{responses: []ProviderResponse{
		toolCall("t1", "task", `{"prompt":"look around","description":"explore"}`),
		toolCall("c1", "echo", `{"msg":"child"}`),
		text("child answer"),
		text("parent done"),
	}}
	tools := NewToolRegistry()
	tools.Register(echo{})
	cfg := cfgTurns(5)
	tools.Register(NewTaskTool(p, tools, AllowAll{}, cfg))
	a := NewAgent(p, tools, AllowAll{}, cfg, ToolContext{Cwd: t.TempDir()})
	err, evs := collect(a, "go")
	if err != nil {
		t.Fatal(err)
	}
	var result *EvToolResult
	for _, e := range evs {
		if r, ok := e.(EvToolResult); ok && r.Name == "task" {
			result = &r
		}
		if c, ok := e.(EvToolCall); ok && c.Name == "echo" {
			t.Fatal("the child's echo call leaked into the parent's events")
		}
	}
	if result == nil || result.IsError || !strings.HasPrefix(result.Output, "child answer\n\n[sub-agent: 2 turn(s), 1 tool call(s)") {
		t.Fatalf("%+v", result)
	}
	if len(a.History) != 4 {
		t.Fatal(len(a.History))
	}
	reqs := p.reqs()
	if len(reqs) != 4 || len(reqs[1].Messages) != 1 {
		t.Fatalf("%d requests; child messages %d", len(reqs), len(reqs[1].Messages))
	}
	has := func(r ProviderRequest, name string) bool {
		for _, s := range r.Tools {
			if s.Name == name {
				return true
			}
		}
		return false
	}
	if !has(reqs[0], "task") || has(reqs[1], "task") || !has(reqs[1], "echo") {
		t.Fatal("child must see echo but not task")
	}
	if !strings.HasPrefix(FormatTaskOutcome(TaskOutcome{Answer: "  "}), "(the sub-agent gave no final answer)") {
		t.Fatal(FormatTaskOutcome(TaskOutcome{}))
	}
}

// flaky fails `failures` times with errText, then answers like scripted.
type flaky struct {
	mu                    sync.Mutex
	failures, calls       int
	errText               string
	streamedBeforeFailing bool
	inner                 *scripted
}

func (p *flaky) Name() string                                        { return "flaky" }
func (p *flaky) DefaultModel() string                                { return "mock" }
func (p *flaky) ListModels(ctx context.Context) ([]ModelInfo, error) { return p.inner.ListModels(ctx) }
func (p *flaky) Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error) {
	p.mu.Lock()
	p.calls++
	fail := p.failures > 0
	if fail {
		p.failures--
	}
	p.mu.Unlock()
	if fail {
		return ProviderResponse{}, errors.New(p.errText)
	}
	return p.inner.Complete(ctx, req)
}
func (p *flaky) CompleteStream(ctx context.Context, req ProviderRequest, on func(string)) (ProviderResponse, error) {
	p.mu.Lock()
	partial := p.streamedBeforeFailing && p.failures > 0
	p.mu.Unlock()
	if partial {
		on("partial ")
	}
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	on(resp.Message.Text())
	return resp, nil
}
func (p *flaky) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newFlaky(failures int, errText string, streamedBeforeFailing bool, responses ...ProviderResponse) *flaky {
	return &flaky{failures: failures, errText: errText, streamedBeforeFailing: streamedBeforeFailing, inner: &scripted{responses: responses}}
}

func flakyAgent(p *flaky, maxRetries int) *Agent {
	cfg := DefaultAgentConfig()
	cfg.Model, cfg.MaxRetries = "mock", maxRetries
	return NewAgent(p, NewToolRegistry(), policyFunc(func(string, bool) Permission { return Allow }), cfg, ToolContext{Cwd: "."})
}

func retryEvents(evs []Event) (out []EvRetry) {
	for _, e := range evs {
		if r, ok := e.(EvRetry); ok {
			out = append(out, r)
		}
	}
	return out
}

func TestTransientErrorsAreRecognisedByStatusOrMessage(t *testing.T) {
	yes := []string{
		"Anthropic 529 Service Overloaded (overloaded_error): Overloaded",
		"Anthropic 429 Too Many Requests (rate_limit_error): slow down",
		"openai 503 Service Unavailable : upstream",
		"codex 500: internal",
		"Anthropic stream error (overloaded_error): Overloaded",
		"request to Anthropic failed: error sending request for url (https://x)",
		"reading Anthropic stream: connection reset by peer",
	}
	no := []string{
		"Anthropic 400 Bad Request (invalid_request_error): max_tokens 500 exceeds the limit",
		"Anthropic 401 Unauthorized (authentication_error): invalid x-api-key",
		"codex 429: ChatGPT plan usage limit reached; resets in ~5.0h",
		"request to oMLX failed: connection refused",
		"compaction produced an empty summary",
	}
	for _, s := range yes {
		if !IsTransient(errors.New(s)) {
			t.Fatalf("should be transient: %s", s)
		}
	}
	for _, s := range no {
		if IsTransient(errors.New(s)) {
			t.Fatalf("should not be transient: %s", s)
		}
	}
	if IsTransient(nil) {
		t.Fatal("nil")
	}
}

func TestRetryDelayDoublesFromOneSecondAndIsCapped(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 30 * time.Second} {
		if got := RetryDelay(attempt); got != want {
			t.Fatalf("attempt %d: got %v want %v", attempt, got, want)
		}
	}
}

func TestTransientProviderErrorsAreRetriedWithARetryEvent(t *testing.T) {
	p := newFlaky(1, "Anthropic 529 Service Overloaded (overloaded_error): Overloaded", false, text("after retry"))
	a := flakyAgent(p, 3)
	err, evs := collect(a, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if p.callCount() != 2 {
		t.Fatalf("calls %d", p.callCount())
	}
	rs := retryEvents(evs)
	if len(rs) != 1 || rs[0].Attempt != 1 || rs[0].MaxRetries != 3 || rs[0].DelayMs != 1000 || !strings.Contains(rs[0].Error, "529") {
		t.Fatalf("%+v", rs)
	}
	found := false
	for _, e := range evs {
		if x, ok := e.(EvText); ok && x.Text == "after retry" {
			found = true
		}
	}
	if !found {
		t.Fatalf("%+v", evs)
	}
	if a.Usage().Requests != 1 {
		t.Fatalf("a failed attempt is not counted as a request: %+v", a.Usage())
	}
}

func TestRetriesStopAtMaxRetriesAndTheErrorIsReported(t *testing.T) {
	p := newFlaky(2, "openai 503 Service Unavailable : down", false, text("never"))
	a := flakyAgent(p, 1)
	err, evs := collect(a, "hi")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatal(err)
	}
	if p.callCount() != 2 || len(retryEvents(evs)) != 1 {
		t.Fatalf("calls %d retries %d", p.callCount(), len(retryEvents(evs)))
	}
}

func TestPermanentErrorsAndZeroRetriesFailAtOnce(t *testing.T) {
	p := newFlaky(1, "Anthropic 401 Unauthorized (authentication_error): bad key", false, text("never"))
	if err, _ := collect(flakyAgent(p, 3), "hi"); err == nil || p.callCount() != 1 {
		t.Fatalf("%v calls %d", err, p.callCount())
	}
	p = newFlaky(1, "Anthropic 529 Service Overloaded", false, text("never"))
	if err, _ := collect(flakyAgent(p, 0), "hi"); err == nil || p.callCount() != 1 {
		t.Fatalf("%v calls %d", err, p.callCount())
	}
}

func TestAFailureAfterStreamedTextIsNotRetried(t *testing.T) {
	p := newFlaky(1, "reading Anthropic stream: connection reset by peer", true, text("never"))
	err, evs := collect(flakyAgent(p, 3), "hi")
	if err == nil || p.callCount() != 1 || len(retryEvents(evs)) != 0 {
		t.Fatalf("%v calls %d retries %d", err, p.callCount(), len(retryEvents(evs)))
	}
}

func TestBangLinesAreShellCommands(t *testing.T) {
	for line, want := range map[string]string{"!git status": "git status", "  ! ls -la  ": "ls -la", "!": "", "!   ": "", "fix it!": "", "/help": ""} {
		got, ok := ShellPassthrough(line)
		if got != want || ok != (want != "") {
			t.Fatalf("%q: got %q %v", line, got, ok)
		}
	}
}

func TestShellNoteHasTheCommandAndTheCutOutput(t *testing.T) {
	if got := ShellNote("ls", "a\nb\n"); got != "I ran this shell command myself in the project folder:\n$ ls\n\na\nb" {
		t.Fatal(got)
	}
	if got := ShellNote("true", "  \n"); !strings.HasSuffix(got, "(no output)") {
		t.Fatal(got)
	}
	if got := ShellNote("cat big", strings.Repeat("x", ShellNoteLimit+10)); !strings.HasSuffix(got, "…[10 more bytes]") {
		t.Fatal(got[len(got)-40:])
	}
	a := NewAgent(&scripted{}, NewToolRegistry(), policyFunc(func(string, bool) Permission { return Allow }), DefaultAgentConfig(), ToolContext{Cwd: "."})
	a.AddShellOutput("ls", "a")
	if len(a.History) != 1 || a.History[0].Role != RoleUser || !strings.Contains(a.History[0].Text(), "$ ls") {
		t.Fatalf("%+v", a.History)
	}
}

// peek is a read-only tool for the plan-mode test.
type peek struct{}

func (peek) Name() string        { return "peek" }
func (peek) Description() string { return "peek" }
func (peek) InputSchema() any    { return map[string]any{"type": "object"} }
func (peek) IsMutating() bool    { return false }
func (peek) Call(context.Context, ToolContext, json.RawMessage) (string, error) {
	return "peeked", nil
}

func TestPlanModeRefusesMutatingToolsAndTaskAndAddsThePlanPrompt(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{
		toolCall("1", "echo", `{"msg":"x"}`),
		toolCall("2", "peek", `{}`),
		toolCall("3", "task", `{"prompt":"explore"}`),
		text("here is the plan"),
	}}
	tools := NewToolRegistry()
	tools.Register(echo{})
	tools.Register(peek{})
	tools.Register(NewTaskTool(p, tools, AllowAll{}, cfgTurns(5)))
	a := NewAgent(p, tools, AllowAll{}, cfgTurns(5), ToolContext{Cwd: "."})
	if a.PlanMode() {
		t.Fatal("plan mode must start off")
	}
	a.SetPlanMode(true)
	err, evs := collect(a, "change x")
	if err != nil {
		t.Fatal(err)
	}
	var results []EvToolResult
	for _, e := range evs {
		if r, ok := e.(EvToolResult); ok {
			results = append(results, r)
		}
	}
	if len(results) != 3 {
		t.Fatalf("%+v", results)
	}
	if !results[0].IsError || !strings.Contains(results[0].Output, "plan mode is on") || !strings.Contains(results[0].Output, "`echo`") {
		t.Fatalf("%+v", results[0])
	}
	if results[1].IsError || results[1].Output != "peeked" {
		t.Fatalf("%+v", results[1])
	}
	if !results[2].IsError || !strings.Contains(results[2].Output, "`task`") {
		t.Fatalf("%+v", results[2])
	}
	for _, r := range p.reqs() {
		if !strings.HasSuffix(r.System, PlanModePrompt) || !strings.HasPrefix(r.System, DefaultSystemPrompt) {
			t.Fatalf("plan prompt missing: %q", r.System[:40])
		}
	}

	// Off again: the prompt is plain and mutating tools run.
	a.SetPlanMode(false)
	p.responses = []ProviderResponse{toolCall("4", "echo", `{"msg":"y"}`), text("done")}
	err, evs = collect(a, "now do it")
	if err != nil {
		t.Fatal(err)
	}
	if !any_(evs, func(e Event) bool { r, ok := e.(EvToolResult); return ok && !r.IsError && r.Output == "echo:y" }) {
		t.Fatalf("%+v", evs)
	}
	reqs := p.reqs()
	if strings.Contains(reqs[len(reqs)-1].System, PlanModePrompt) {
		t.Fatal("plan prompt still present")
	}
}

func TestChangeDirMovesTheAgentOnlyWhenItSucceeds(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(tmp, "project")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &scripted{responses: []ProviderResponse{
		toolCall("t1", "change_dir", `{"path":"no-such-dir-here"}`),
		toolCall("t2", "change_dir", `{"path":"project"}`),
		text("moved"),
	}}
	tools := NewToolRegistry()
	tools.Register(ChangeDir{})
	a := NewAgent(p, tools, AllowAll{}, cfgTurns(5), ToolContext{Cwd: tmp})
	err, evs := collect(a, "go")
	if err != nil {
		t.Fatal(err)
	}
	if !any_(evs, func(e Event) bool { x, ok := e.(EvToolResult); return ok && x.ID == "t1" && x.IsError }) {
		t.Fatal("expected the missing directory to be an error")
	}
	if a.Cwd() != sub {
		t.Fatalf("cwd = %s, want %s", a.Cwd(), sub)
	}
	if _, err := ResolveDir(sub, "~"); err != nil {
		t.Fatal(err)
	}
}
