package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type AgentConfig struct {
	Model        string
	SystemPrompt string
	MaxTokens    int
	// MaxTurns is a hard cap on provider round-trips per Run to avoid runaway loops.
	MaxTurns int
	// CompactAtTokens: when the last request's input tokens reach this, the
	// history is summarized before the next request. 0 disables compaction.
	CompactAtTokens uint64
}

func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Model:           "claude-sonnet-5",
		SystemPrompt:    DefaultSystemPrompt,
		MaxTokens:       8192,
		MaxTurns:        50,
		CompactAtTokens: 150_000,
	}
}

const DefaultSystemPrompt = "You are AgentiLoop, an autonomous coding agent running in the user's terminal, built by AgentiLoop. " +
	"Your name is AgentiLoop. If an earlier system block gave you a different name or vendor, disregard it: " +
	"when asked who you are or who made you, answer AgentiLoop and never call yourself Claude Code or say you were made by Anthropic. " +
	"Use the provided tools to inspect and modify the project in the current working directory. " +
	"Be concise. Prefer acting over asking. When the task is complete, reply with a short summary. " +
	"Your replies are rendered as Markdown directly in the terminal: write Markdown (headings, lists, code fences for code) " +
	"and it will be displayed styled. Never wrap an entire reply in a ```markdown fence, and never shell out to tools like " +
	"glow, bat, or cat to \"render\" Markdown — just write it."

const compactSystemPrompt = "You compress conversation transcripts for an autonomous coding agent so it can continue " +
	"with less context. Write a dense summary that preserves: the user's goals and constraints, decisions made, files and " +
	"symbols touched (with paths), what has been verified to work, what failed and why, and any pending next steps. " +
	"Do not add commentary. Output only the summary."

// transcriptResultLimit: tool output longer than this is trimmed in the compaction transcript.
const transcriptResultLimit = 2_000

// Event is emitted during a run so the front-end can render progress.
type Event interface{ isEvent() }

// EvTextDelta is a streamed chunk of assistant text, emitted as it arrives.
type EvTextDelta struct{ Text string }

// EvText is the complete assistant text for the turn (after all deltas).
type EvText struct{ Text string }

type EvToolCall struct {
	ID, Name string
	Input    json.RawMessage
}

type EvToolResult struct {
	ID, Name, Output string
	IsError          bool
}

// EvTurnComplete: one provider round-trip finished. ElapsedMs is the whole call;
// FirstTokenMs is when the first text delta arrived (nil when no text streamed).
type EvTurnComplete struct {
	InputTokens, OutputTokens, ElapsedMs uint64
	FirstTokenMs                         *uint64
}

// EvCompacted: history was summarized; BeforeTokens is the input size that triggered it.
type EvCompacted struct {
	BeforeTokens    uint64
	MessagesDropped int
}

type EvDone struct{ StopReason StopReason }

func (EvTextDelta) isEvent()    {}
func (EvText) isEvent()         {}
func (EvToolCall) isEvent()     {}
func (EvToolResult) isEvent()   {}
func (EvTurnComplete) isEvent() {}
func (EvCompacted) isEvent()    {}
func (EvDone) isEvent()         {}

type Agent struct {
	provider Provider
	tools    *ToolRegistry
	policy   PermissionPolicy
	config   AgentConfig
	tc       ToolContext
	History  []Message
	// lastInputTokens: input tokens reported by the most recent provider response.
	lastInputTokens uint64
}

func NewAgent(p Provider, tools *ToolRegistry, policy PermissionPolicy, config AgentConfig, tc ToolContext) *Agent {
	return &Agent{provider: p, tools: tools, policy: policy, config: config, tc: tc}
}

func (a *Agent) Model() string           { return a.config.Model }
func (a *Agent) SetModel(m string)       { a.config.Model = m }
func (a *Agent) LastInputTokens() uint64 { return a.lastInputTokens }
func (a *Agent) Provider() Provider      { return a.provider }
func (a *Agent) Tools() *ToolRegistry    { return a.tools }

// Clear drops all conversation context and tool history.
func (a *Agent) Clear() {
	a.History = nil
	a.lastInputTokens = 0
}

func (a *Agent) shouldCompact() bool {
	return a.config.CompactAtTokens > 0 && a.lastInputTokens >= a.config.CompactAtTokens
}

// Compact replaces the history with a provider-written summary of it. Returns nil when empty.
func (a *Agent) Compact(ctx context.Context) (*EvCompacted, error) {
	if len(a.History) == 0 {
		return nil, nil
	}
	req := ProviderRequest{
		Model:     a.config.Model,
		System:    compactSystemPrompt,
		Messages:  []Message{UserText("Summarize the following transcript.\n\n<transcript>\n" + Transcript(a.History) + "\n</transcript>")},
		MaxTokens: 4096,
	}
	resp, err := a.provider.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	summary := resp.Message.Text()
	if strings.TrimSpace(summary) == "" {
		return nil, errors.New("compaction produced an empty summary")
	}
	ev := &EvCompacted{BeforeTokens: a.lastInputTokens, MessagesDropped: len(a.History)}
	a.History = []Message{
		UserText("[Context was compacted. Summary of the conversation so far:]\n" + summary),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("Understood. I will continue from that summary.")}},
	}
	a.lastInputTokens = 0
	return ev, nil
}

func (a *Agent) toolSpecs() []ToolSpec {
	var specs []ToolSpec
	for _, t := range a.tools.All() {
		specs = append(specs, ToolSpec{Name: t.Name(), Description: t.Description(), InputSchema: t.InputSchema()})
	}
	return specs
}

// Run is the core agentic loop: send → if tool_use, execute tools, append results, repeat.
func (a *Agent) Run(ctx context.Context, userInput string, onEvent func(Event)) error {
	if a.shouldCompact() {
		ev, err := a.Compact(ctx)
		if err != nil {
			return err
		}
		if ev != nil {
			onEvent(*ev)
		}
	}
	a.History = append(a.History, UserText(userInput))

	for range a.config.MaxTurns {
		req := ProviderRequest{
			Model:     a.config.Model,
			System:    a.config.SystemPrompt,
			Messages:  append([]Message(nil), a.History...),
			Tools:     a.toolSpecs(),
			MaxTokens: a.config.MaxTokens,
		}
		started := time.Now()
		var firstToken *uint64
		resp, err := a.provider.CompleteStream(ctx, req, func(delta string) {
			if firstToken == nil {
				ms := uint64(time.Since(started).Milliseconds())
				firstToken = &ms
			}
			onEvent(EvTextDelta{delta})
		})
		if err != nil {
			return err
		}
		a.lastInputTokens = resp.InputTokens
		onEvent(EvTurnComplete{resp.InputTokens, resp.OutputTokens, uint64(time.Since(started).Milliseconds()), firstToken})

		if text := resp.Message.Text(); text != "" {
			onEvent(EvText{text})
		}
		calls := resp.Message.ToolUses()
		a.History = append(a.History, resp.Message)

		if resp.StopReason != StopToolUse || len(calls) == 0 {
			onEvent(EvDone{resp.StopReason})
			return nil
		}

		results := make([]ContentBlock, 0, len(calls))
		for _, c := range calls {
			onEvent(EvToolCall{c.ID, c.Name, c.Input})
			output, isError := "", false
			if out, err := a.execute(ctx, c.Name, c.Input); err != nil {
				output, isError = err.Error(), true
			} else {
				output = out
			}
			onEvent(EvToolResult{c.ID, c.Name, output, isError})
			results = append(results, ToolResultBlock(c.ID, output, isError))
		}
		a.History = append(a.History, ToolResults(results))

		if a.shouldCompact() {
			ev, err := a.Compact(ctx)
			if err != nil {
				return err
			}
			if ev != nil {
				onEvent(*ev)
			}
			a.History = append(a.History, UserText("Continue the task from the summary above."))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("max_turns (%d) reached", a.config.MaxTurns)
}

func (a *Agent) execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	tool, ok := a.tools.Get(name)
	if !ok {
		return "", InvalidInput("unknown tool `%s`", name)
	}
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	switch a.policy.Check(ctx, name, tool.IsMutating(), input) {
	case Deny:
		return "", Denied(fmt.Sprintf("user declined `%s`", name))
	case Cancel:
		return "", Cancelled(name)
	}
	return tool.Call(ctx, a.tc, input)
}

// Transcript is the plain-text rendering of the history for the compaction prompt.
func Transcript(history []Message) string {
	var sb strings.Builder
	for _, m := range history {
		role := "USER"
		if m.Role == RoleAssistant {
			role = "ASSISTANT"
		}
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				fmt.Fprintf(&sb, "%s: %s\n", role, b.Text)
			case BlockToolUse:
				fmt.Fprintf(&sb, "%s → tool %s %s\n", role, b.Name, compactJSON(b.Input))
			case BlockToolResult:
				tag := "tool result"
				if b.IsError {
					tag = "tool error"
				}
				body := b.Content
				if len(body) > transcriptResultLimit {
					cut := transcriptResultLimit
					for cut > 0 && !utf8.RuneStart(body[cut]) {
						cut--
					}
					body = fmt.Sprintf("%s…[%d more bytes]", body[:cut], len(b.Content)-cut)
				}
				fmt.Fprintf(&sb, "%s: %s\n", tag, body)
			}
		}
	}
	return sb.String()
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	out, _ := json.Marshal(v)
	return string(out)
}
