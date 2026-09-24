package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	. "github.com/AgentiLoop/AgentiLoopGo/core"
)

// scripted replays a fixed list of responses, recording every request it received.
type scripted struct {
	mu        sync.Mutex
	responses []ProviderResponse
	requests  []ProviderRequest
}

func (p *scripted) Name() string                                    { return "scripted" }
func (p *scripted) DefaultModel() string                            { return "mock" }
func (p *scripted) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
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
	c.CompactAtTokens = at
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
