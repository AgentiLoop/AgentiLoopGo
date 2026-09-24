package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ToolSpec is what the model sees for one tool.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

type ProviderRequest struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
}

type ProviderResponse struct {
	Message      Message
	StopReason   StopReason
	InputTokens  uint64
	OutputTokens uint64
}

// ModelInfo is one entry from a provider's model catalog.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// Provider is a model backend. Implementations live in package provider.
type Provider interface {
	Name() string
	// DefaultModel is used when the user hasn't picked one.
	DefaultModel() string
	Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error)
	// ListModels returns the live model catalog, newest first where the backend supports ordering.
	ListModels(ctx context.Context) ([]ModelInfo, error)
	// CompleteStream delivers text deltas through onText as they arrive and returns
	// the assembled response once the stream ends. Providers without streaming can
	// use CompleteAsStream.
	CompleteStream(ctx context.Context, req ProviderRequest, onText func(string)) (ProviderResponse, error)
}

// CompleteAsStream is the non-streaming fallback: it calls Complete and emits the
// full text as one delta.
func CompleteAsStream(ctx context.Context, p Provider, req ProviderRequest, onText func(string)) (ProviderResponse, error) {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	if t := resp.Message.Text(); t != "" {
		onText(t)
	}
	return resp, nil
}

// ---- tools -----------------------------------------------------------------

type ToolErrorKind int

const (
	ErrInvalidInput ToolErrorKind = iota
	ErrDenied
	// ErrCancelled: the user skipped this one call; worded so the model carries on.
	ErrCancelled
	ErrFailed
)

type ToolError struct {
	Kind ToolErrorKind
	Msg  string
}

func (e *ToolError) Error() string {
	switch e.Kind {
	case ErrInvalidInput:
		return "invalid input: " + e.Msg
	case ErrDenied:
		return "permission denied: " + e.Msg
	case ErrCancelled:
		return fmt.Sprintf("cancelled: the user skipped this `%s` call. Continue with the rest of the task without it.", e.Msg)
	}
	return e.Msg
}

func InvalidInput(format string, a ...any) error {
	return &ToolError{ErrInvalidInput, fmt.Sprintf(format, a...)}
}
func Denied(msg string) error     { return &ToolError{ErrDenied, msg} }
func Cancelled(tool string) error { return &ToolError{ErrCancelled, tool} }
func Failed(format string, a ...any) error {
	return &ToolError{ErrFailed, fmt.Sprintf(format, a...)}
}

// IsToolError reports whether err is a ToolError of the given kind.
func IsToolError(err error, kind ToolErrorKind) bool {
	var te *ToolError
	return errors.As(err, &te) && te.Kind == kind
}

// ToolContext is handed to every tool invocation.
type ToolContext struct {
	Cwd string
}

type Tool interface {
	Name() string
	Description() string
	// InputSchema is the JSON Schema for the tool's input object.
	InputSchema() any
	// IsMutating drives the permission gate.
	IsMutating() bool
	Call(ctx context.Context, tc ToolContext, input json.RawMessage) (string, error)
}

type ToolRegistry struct {
	tools map[string]Tool
}

func NewToolRegistry() *ToolRegistry { return &ToolRegistry{tools: map[string]Tool{}} }

func (r *ToolRegistry) Register(t Tool) *ToolRegistry {
	r.tools[t.Name()] = t
	return r
}

func (r *ToolRegistry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// All returns the tools sorted by name.
func (r *ToolRegistry) All() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func (r *ToolRegistry) Len() int { return len(r.tools) }

// ---- permissions -------------------------------------------------------------

type Permission int

const (
	Allow Permission = iota
	Deny
	// Cancel skips just this call (e.g. Esc in the TUI); the agent keeps working.
	Cancel
)

// PermissionPolicy decides whether a tool call may run. The CLI supplies an
// interactive implementation; tests/CI can use AllowAll.
type PermissionPolicy interface {
	Check(ctx context.Context, tool string, isMutating bool, input json.RawMessage) Permission
}

type AllowAll struct{}

func (AllowAll) Check(context.Context, string, bool, json.RawMessage) Permission { return Allow }
