package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// task: run a sub-agent on a self-contained job. It gets a fresh conversation, the same
// provider, model, system prompt, permission policy and tools (except task itself, so the
// depth is one), does the work, and only its final answer comes back as the tool result — the
// parent's context stays clean of the exploration.

const TaskToolName = "task"

type TaskTool struct {
	provider Provider
	tools    *ToolRegistry
	policy   PermissionPolicy
	config   AgentConfig
}

// TaskOutcome is what a sub-agent run produced, for the tool result and for tests.
type TaskOutcome struct {
	Answer       string
	Turns        uint64
	ToolCalls    int
	InputTokens  uint64
	OutputTokens uint64
}

// NewTaskTool: tools is the parent's registry; task is left out of the child's copy.
func NewTaskTool(provider Provider, tools *ToolRegistry, policy PermissionPolicy, config AgentConfig) *TaskTool {
	child := NewToolRegistry()
	for _, t := range tools.All() {
		if t.Name() != TaskToolName {
			child.Register(t)
		}
	}
	config.Hooks = config.Hooks.WithoutStop()
	return &TaskTool{provider: provider, tools: child, policy: policy, config: config}
}

func (t *TaskTool) Run(ctx context.Context, cwd, prompt string) (TaskOutcome, error) {
	agent := NewAgent(t.provider, t.tools, t.policy, t.config, ToolContext{Cwd: cwd})
	var out TaskOutcome
	err := agent.Run(ctx, prompt, func(ev Event) {
		switch e := ev.(type) {
		case EvText:
			out.Answer = e.Text
		case EvToolCall:
			out.ToolCalls++
			slog.Debug("sub-agent tool call", "tool", e.Name)
		}
	})
	if err != nil {
		return out, err
	}
	u := agent.Usage()
	out.Turns, out.InputTokens, out.OutputTokens = u.Requests, u.InputTokens, u.OutputTokens
	return out, nil
}

// FormatTaskOutcome is the tool result text: the answer, then one line of accounting.
func FormatTaskOutcome(o TaskOutcome) string {
	answer := strings.TrimSpace(o.Answer)
	if answer == "" {
		answer = "(the sub-agent gave no final answer)"
	}
	return fmt.Sprintf("%s\n\n[sub-agent: %d turn(s), %d tool call(s), %d input + %d output tokens]", answer, o.Turns, o.ToolCalls, o.InputTokens, o.OutputTokens)
}

func (t *TaskTool) Name() string { return TaskToolName }
func (t *TaskTool) Description() string {
	return "Run a sub-agent on a self-contained task and get back only its final answer. It starts with an empty " +
		"conversation and has the same tools as you (except task), so use it for exploration or research whose " +
		"intermediate steps would clutter this conversation: 'find every place X is used and summarize', 'check " +
		"whether the tests cover Y'. The prompt must contain everything it needs — it cannot see this conversation. " +
		"Say what to return. It cannot start further sub-agents."
}
func (t *TaskTool) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt":      map[string]any{"type": "string", "description": "The complete, self-contained task, including what to return"},
			"description": map[string]any{"type": "string", "description": "A few words naming the task, shown to the user"},
		},
		"required": []string{"prompt"},
	}
}
func (t *TaskTool) IsMutating() bool { return false }

func (t *TaskTool) Call(ctx context.Context, tc ToolContext, input json.RawMessage) (string, error) {
	var a struct {
		Prompt      string `json:"prompt"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", InvalidInput("%v", err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "", InvalidInput("prompt is empty")
	}
	slog.Info("sub-agent starting", "description", a.Description)
	out, err := t.Run(ctx, tc.Cwd, a.Prompt)
	if err != nil {
		return "", Failed("sub-agent failed: %v", err)
	}
	return FormatTaskOutcome(out), nil
}
