package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBreakdownSortsBytesIntoParts(t *testing.T) {
	tools := []ToolSpec{{Name: "bash", Description: "run", InputSchema: map[string]any{"type": "object"}}}
	history := []Message{
		UserText("hello"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("héllo"), ToolUseBlock("1", "bash", json.RawMessage(`{"c": "ls"}`))}},
		ToolResults([]ContentBlock{ToolResultBlock("1", "a\nb", false)}),
		User([]ContentBlock{ImageBlock("image/png", "AAAA")}),
	}
	b := Breakdown("sys", tools, history)
	want := ContextBreakdown{System: 3, Tools: 4 + 3 + 17, ToolCount: 1, Prompts: 5, Replies: 6, ToolCalls: 4 + 10, ToolCallCount: 1, ToolResults: 3, ToolResultCount: 1, Images: 1}
	if b != want {
		t.Fatalf("breakdown = %+v, want %+v", b, want)
	}
	if b.Total() != 3+24+5+6+14+3 {
		t.Fatalf("total = %d", b.Total())
	}
}

func TestRenderContextRoundsUpAndShowsWindowImagesAndLastRequest(t *testing.T) {
	b := ContextBreakdown{System: 400, Tools: 801, ToolCount: 2, Prompts: 3, Images: 2}
	if EstimateTokens(0) != 0 || EstimateTokens(5) != 2 {
		t.Fatal("EstimateTokens rounds up")
	}
	window := uint64(1000)
	want := "context estimate (about 4 bytes of text per token):\n" +
		"  system prompt        ~100 tokens\n" +
		"  tools (2)            ~201 tokens\n" +
		"  your messages        ~1 tokens\n" +
		"  replies              ~0 tokens\n" +
		"  tool calls (0)       ~0 tokens\n" +
		"  tool results (0)     ~0 tokens\n" +
		"  images               2 (not counted)\n" +
		"  total                ~301 tokens (30% of 1000)\n" +
		"last request: 512 input tokens as counted by the provider"
	if got := RenderContext(b, &window, 512); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	bare := RenderContext(ContextBreakdown{}, nil, 0)
	if !strings.HasSuffix(bare, "total                ~0 tokens") || strings.Contains(bare, "images") || strings.Contains(bare, "last request") {
		t.Fatalf("bare = %q", bare)
	}
}
