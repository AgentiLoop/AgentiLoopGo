package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	. "github.com/AgentiLoop/AgentiLoopGo/core"
)

func estimatedInput(t *testing.T, req ProviderRequest) int {
	t.Helper()
	b, err := json.Marshal([]any{req.System, req.Messages, req.Tools})
	if err != nil {
		t.Fatal(err)
	}
	return len(b) + 256 + 16*(len(req.Messages)+len(req.Tools))
}

func TestSmallContextReservesInputAndToolsOnEveryTurn(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{toolCall("t1", "echo", `{"msg":"ping"}`), text("hello")}}
	c := cfgTurns(5)
	c.Model = "small"
	a := newAgent(p, AllowAll{}, c)
	if err, _ := collect(a, "hello"); err != nil {
		t.Fatal(err)
	}
	reqs := p.reqs()
	if len(reqs) != 2 {
		t.Fatal(len(reqs))
	}
	for _, r := range reqs {
		if r.MaxTokens <= 0 || r.MaxTokens+estimatedInput(t, r) > 8192 || r.MaxTokens >= 7800 {
			t.Fatalf("max_tokens %d does not leave room for input", r.MaxTokens)
		}
	}
	if reqs[1].MaxTokens >= reqs[0].MaxTokens {
		t.Fatal("tool history should consume context")
	}
}

func TestSmallContextClampsExplicitOutputAndCompaction(t *testing.T) {
	p := &scripted{responses: []ProviderResponse{text("hello"), text("SUMMARY")}}
	c := cfgTurns(5)
	c.Model = "small"
	maxTokens := 32_768
	c.MaxTokens = &maxTokens
	a := newAgent(p, AllowAll{}, c)
	if err, _ := collect(a, "hello"); err != nil {
		t.Fatal(err)
	}
	a.History = append(a.History, UserText(strings.Repeat("x", 4500)))
	if _, err := a.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := p.reqs()
	if reqs[0].MaxTokens >= 8192 || reqs[1].MaxTokens <= 0 || reqs[1].MaxTokens >= 4096 {
		t.Fatalf("%d %d", reqs[0].MaxTokens, reqs[1].MaxTokens)
	}
}

func TestOversizedInputFailsLocallyWithoutSendingRequest(t *testing.T) {
	p := &scripted{}
	c := cfgTurns(5)
	c.Model = "small"
	a := newAgent(p, AllowAll{}, c)
	err, _ := collect(a, strings.Repeat("界", 8192))
	if err == nil || !strings.Contains(err.Error(), "context budget") {
		t.Fatal(err)
	}
	if len(p.reqs()) != 0 {
		t.Fatal("request was sent")
	}
}
