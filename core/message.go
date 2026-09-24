// Package core is the provider-agnostic message model, tool interface, and the agentic loop.
package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Block types for ContentBlock.Type.
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// ContentBlock is one piece of a message: text, a tool call, or a tool result.
// Only the fields for its Type are meaningful.
type ContentBlock struct {
	Type string
	// text
	Text string
	// tool_use
	ID    string
	Name  string
	Input json.RawMessage
	// tool_result
	ToolUseID string
	Content   string
	IsError   bool
}

func TextBlock(text string) ContentBlock { return ContentBlock{Type: BlockText, Text: text} }

func ToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: BlockToolUse, ID: id, Name: name, Input: input}
}

func ToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{Type: BlockToolResult, ToolUseID: toolUseID, Content: content, IsError: isError}
}

// MarshalJSON emits `{"type":"text","text":…}`, `{"type":"tool_use",…}` or
// `{"type":"tool_result",…}` — the same shape the Rust version stores in sessions.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case BlockText:
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.Type, b.Text})
	case BlockToolUse:
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{b.Type, b.ID, b.Name, input})
	case BlockToolResult:
		return json.Marshal(struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
			IsError   bool   `json:"is_error,omitempty"`
		}{b.Type, b.ToolUseID, b.Content, b.IsError})
	}
	return nil, fmt.Errorf("unknown content block type %q", b.Type)
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   string          `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch raw.Type {
	case BlockText, BlockToolUse, BlockToolResult:
	default:
		return fmt.Errorf("unknown content block type %q", raw.Type)
	}
	*b = ContentBlock{raw.Type, raw.Text, raw.ID, raw.Name, raw.Input, raw.ToolUseID, raw.Content, raw.IsError}
	return nil
}

type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

func UserText(text string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{TextBlock(text)}}
}

func ToolResults(results []ContentBlock) Message {
	return Message{Role: RoleUser, Content: results}
}

// ToolUses returns the tool_use blocks of the message.
func (m Message) ToolUses() []ContentBlock {
	var out []ContentBlock
	for _, b := range m.Content {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// Text concatenates the message's text blocks.
func (m Message) Text() string {
	var sb strings.Builder
	for _, b := range m.Content {
		if b.Type == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"
	StopToolUse      StopReason = "tool_use"
	StopMaxTokens    StopReason = "max_tokens"
	StopStopSequence StopReason = "stop_sequence"
	StopOther        StopReason = "other"
)
