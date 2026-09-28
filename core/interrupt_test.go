package core_test

import (
	"context"
	"strings"
	"testing"

	. "github.com/AgentiLoop/AgentiLoopGo/core"
)

// Esc while a permission prompt is open: the run stops, the call is paired as
// interrupted, and the next prompt continues the same conversation.
func TestCancelDuringToolKeepsSessionAndPairsCall(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{toolCall("t1", "echo", `{"msg":"hi"}`), text("next answer")}}
	ctx, cancel := context.WithCancel(context.Background())
	a := newAgent(p, policyFunc(func(string, bool) Permission { cancel(); return Deny }), cfgTurns(5))
	var results int
	err := a.Run(ctx, "first", func(e Event) {
		if _, ok := e.(EvToolResult); ok {
			results++
		}
	})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("want cancellation error, got %v", err)
	}
	if results != 0 {
		t.Fatalf("a cancelled call must not report its result, got %d", results)
	}
	if len(a.History) != 3 {
		t.Fatalf("history = %d messages, want 3", len(a.History))
	}
	got := a.History[2].Content
	if len(got) != 1 || got[0].ToolUseID != "t1" || !got[0].IsError || !strings.HasPrefix(got[0].Content, "Interrupted by user") {
		t.Fatalf("unpaired call: %+v", got)
	}
	if err, _ := collect(a, "second"); err != nil {
		t.Fatal(err)
	}
	if len(a.History) != 5 || len(p.reqs()[1].Messages) != 4 {
		t.Fatalf("session not continued: history %d, request %d", len(a.History), len(p.reqs()[1].Messages))
	}
}

// streamer emits one delta, then the user cancels before the reply finishes.
type streamer struct {
	scripted
	cancel context.CancelFunc
}

func (s *streamer) CompleteStream(ctx context.Context, req ProviderRequest, on func(string)) (ProviderResponse, error) {
	if s.cancel == nil {
		return s.scripted.CompleteStream(ctx, req, on)
	}
	on("partial answer")
	s.cancel()
	s.cancel = nil
	return ProviderResponse{}, ctx.Err()
}

func TestCancelDuringStreamKeepsPartialText(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &streamer{scripted: scripted{responses: []ProviderResponse{text("done")}}, cancel: cancel}
	tools := NewToolRegistry()
	a := NewAgent(s, tools, policyFunc(func(string, bool) Permission { return Allow }), cfgTurns(5), ToolContext{Cwd: "."})
	if err := a.Run(ctx, "first", func(Event) {}); err == nil {
		t.Fatal("want cancellation error")
	}
	if len(a.History) != 2 || a.History[1].Role != RoleAssistant || a.History[1].Text() != "partial answer" {
		t.Fatalf("partial text not kept: %+v", a.History)
	}
	if err := a.Run(context.Background(), "second", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(a.History) != 4 {
		t.Fatalf("history = %d, want 4", len(a.History))
	}
}
