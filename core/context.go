package core

// /context: what the next request would send, split into parts, as a rough token estimate
// (about 4 bytes of UTF-8 text per token — not a tokenizer).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ContextBreakdown holds the byte sizes (and counts) of each part of a request.
type ContextBreakdown struct {
	System          uint64
	Tools           uint64
	ToolCount       int
	Prompts         uint64
	Replies         uint64
	ToolCalls       uint64
	ToolCallCount   int
	ToolResults     uint64
	ToolResultCount int
	Images          int
}

func (b ContextBreakdown) Total() uint64 {
	return b.System + b.Tools + b.Prompts + b.Replies + b.ToolCalls + b.ToolResults
}

// compactJSONLen is the length of v as compact JSON without HTML escaping (like serde_json).
func compactJSONLen(v any) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return 0
	}
	return len(bytes.TrimRight(buf.Bytes(), "\n"))
}

// Breakdown measures a request made of system, tools and history.
func Breakdown(system string, tools []ToolSpec, history []Message) ContextBreakdown {
	b := ContextBreakdown{System: uint64(len(system)), ToolCount: len(tools)}
	for _, t := range tools {
		b.Tools += uint64(len(t.Name) + len(t.Description) + compactJSONLen(t.InputSchema))
	}
	for _, m := range history {
		for _, block := range m.Content {
			switch block.Type {
			case BlockText:
				if m.Role == RoleUser {
					b.Prompts += uint64(len(block.Text))
				} else {
					b.Replies += uint64(len(block.Text))
				}
			case BlockToolUse:
				input := len(block.Input)
				if input == 0 {
					input = len("null")
				}
				b.ToolCalls += uint64(len(block.Name) + input)
				b.ToolCallCount++
			case BlockToolResult:
				b.ToolResults += uint64(len(block.Content))
				b.ToolResultCount++
			case BlockImage:
				b.Images++
			}
		}
	}
	return b
}

// EstimateTokens is the estimated token count for n bytes of text.
func EstimateTokens(n uint64) uint64 { return (n + 3) / 4 }

// RenderContext is the /context report. window is the model's context window (nil = unknown),
// lastInput the input tokens the provider reported for the latest request (0 = none yet).
func RenderContext(b ContextBreakdown, window *uint64, lastInput uint64) string {
	row := func(label string, n uint64) string {
		return fmt.Sprintf("  %-20s ~%d tokens", label, EstimateTokens(n))
	}
	lines := []string{
		"context estimate (about 4 bytes of text per token):",
		row("system prompt", b.System),
		row(fmt.Sprintf("tools (%d)", b.ToolCount), b.Tools),
		row("your messages", b.Prompts),
		row("replies", b.Replies),
		row(fmt.Sprintf("tool calls (%d)", b.ToolCallCount), b.ToolCalls),
		row(fmt.Sprintf("tool results (%d)", b.ToolResultCount), b.ToolResults),
	}
	if b.Images > 0 {
		lines = append(lines, fmt.Sprintf("  %-20s %d (not counted)", "images", b.Images))
	}
	total := row("total", b.Total())
	if window != nil && *window > 0 {
		total += fmt.Sprintf(" (%d%% of %d)", EstimateTokens(b.Total())*100 / *window, *window)
	}
	lines = append(lines, total)
	if lastInput > 0 {
		lines = append(lines, fmt.Sprintf("last request: %d input tokens as counted by the provider", lastInput))
	}
	return strings.Join(lines, "\n")
}
