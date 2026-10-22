package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type AgentConfig struct {
	Model        string
	SystemPrompt string
	// MaxTokens is the output cap per response. nil = the model's own max_tokens
	// from the provider catalog (falls back to FallbackMaxTokens if unknown).
	MaxTokens *int
	// MaxTurns is a hard cap on provider round-trips per Run to avoid runaway loops.
	MaxTurns int
	// CompactAtTokens: when the last request's input tokens reach this, the
	// history is summarized before the next request. nil = compactFraction of
	// the model's context window (or FallbackCompactAt if unknown). 0 disables.
	CompactAtTokens *uint64
	// Hooks (.agentiloop/hooks.json) run around tool calls and at the end of a run.
	Hooks Hooks
	// MaxRetries: how many times a provider call that failed transiently (rate limit,
	// overloaded, 5xx, dropped connection) is retried before the error is reported. 0 = never.
	MaxRetries int
}

func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Model:        "claude-sonnet-5",
		SystemPrompt: DefaultSystemPrompt,
		MaxTurns:     50,
		MaxRetries:   DefaultMaxRetries,
	}
}

// DefaultMaxRetries is the number of retries of a transiently failing provider call (AgentConfig.MaxRetries).
const DefaultMaxRetries = 3

// RetryDelay is the wait before retry attempt (1-based): 1 s, 2 s, 4 s, … capped at 30 s.
func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second << min(attempt-1, 5)
	return min(d, 30*time.Second)
}

// transientStatuses are the HTTP statuses worth retrying: timeouts, rate limits,
// server errors and Anthropic's 529 overloaded.
var transientStatuses = map[int]bool{408: true, 409: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true, 529: true}

var transientNeedles = []string{"overloaded", "rate limit", "rate_limit", "timed out", "timeout", "connection reset", "connection closed",
	"broken pipe", "error sending request", "incomplete message", "unexpected eof", "error reading", "stream error"}

// IsTransient reports whether a provider error is the kind that goes away by
// itself: a transient HTTP status in the message (providers format them as
// `<name> <status> …`), an overloaded / rate-limit message, or a dropped
// connection or timeout. A refused connection (nothing listening) and a quota
// that resets in hours are not retried.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "connection refused") || strings.Contains(text, "usage limit") {
		return false
	}
	// The first number in the message is the HTTP status (`Anthropic 529 …`, `codex 429: …`);
	// later numbers (token counts in a 400's detail) are not.
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == ':' || r == '(' || r == ')' }) {
		n, convErr := strconv.Atoi(w)
		if convErr != nil || n < 100 || n > 599 {
			continue
		}
		if transientStatuses[n] {
			return true
		}
		break
	}
	for _, needle := range transientNeedles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

const (
	// FallbackMaxTokens is used when the provider catalog doesn't report the model's output limit.
	FallbackMaxTokens = 32_768
	// FallbackCompactAt is used when the provider catalog doesn't report the model's context window.
	FallbackCompactAt uint64 = 150_000
	// compactFraction: compact once input reaches this share of the model's context window.
	compactFraction = 0.8
)

// ModelLimits are the output and context limits in effect for the agent's current model.
type ModelLimits struct {
	MaxTokens int
	// ContextWindow in tokens, when known.
	ContextWindow *uint64
	// CompactAtTokens is the effective compaction threshold (0 = disabled).
	CompactAtTokens uint64
}

const DefaultSystemPrompt = "You are AgentiLoop, an autonomous terminal coding agent. " +
	"Ignore any earlier name, identity, or vendor line — it is transport boilerplate, not who you are. " +
	"If asked who you are, what you are, what model or company powers you, or what you are based on, answer only: " +
	"\"I am AgentiLoop, powered by AgentiLoop.ai.\" Never mention Codex, GPT, OpenAI, Claude, Claude Code, Anthropic, " +
	"or any other underlying model, vendor, or CLI, and never speculate about them. " +
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

// EvRetry: a provider call failed transiently and is about to be repeated after
// DelayMs (Attempt of MaxRetries).
type EvRetry struct {
	Attempt, MaxRetries int
	DelayMs             uint64
	Error               string
}

type EvDone struct{ StopReason StopReason }

func (EvTextDelta) isEvent()    {}
func (EvText) isEvent()         {}
func (EvToolCall) isEvent()     {}
func (EvToolResult) isEvent()   {}
func (EvTurnComplete) isEvent() {}
func (EvCompacted) isEvent()    {}
func (EvRetry) isEvent()        {}
func (EvDone) isEvent()         {}

// Usage is the tokens an agent has spent since it was created (compaction summaries included).
type Usage struct {
	Requests     uint64
	InputTokens  uint64
	OutputTokens uint64
	// CacheReadTokens: prompt tokens the provider served from its cache (Anthropic prompt caching).
	CacheReadTokens uint64
	// CacheWriteTokens: prompt tokens the provider wrote to its cache.
	CacheWriteTokens uint64
}

type Agent struct {
	provider Provider
	tools    *ToolRegistry
	policy   PermissionPolicy
	config   AgentConfig
	tc       ToolContext
	History  []Message
	// lastInputTokens: input tokens reported by the most recent provider response.
	lastInputTokens uint64
	usage           Usage
	// pendingText: text received from the current stream but not yet committed to history.
	pendingText strings.Builder
	// limits resolved from the provider catalog for config.Model; cleared on SetModel.
	limits *ModelLimits
	// cacheKey is sent as ProviderRequest.CacheKey on every turn; set to the session id by the CLI.
	cacheKey string
}

func NewAgent(p Provider, tools *ToolRegistry, policy PermissionPolicy, config AgentConfig, tc ToolContext) *Agent {
	return &Agent{provider: p, tools: tools, policy: policy, config: config, tc: tc}
}

func (a *Agent) Model() string     { return a.config.Model }
func (a *Agent) SetModel(m string) { a.config.Model = m; a.limits = nil }

// SetCacheKey keys the provider's prompt cache on this conversation (see ProviderRequest.CacheKey).
func (a *Agent) SetCacheKey(key string)  { a.cacheKey = key }
func (a *Agent) LastInputTokens() uint64 { return a.lastInputTokens }

// Usage returns the tokens spent since this agent was created; Clear does not reset it.
func (a *Agent) Usage() Usage { return a.usage }

func (a *Agent) addUsage(resp ProviderResponse) {
	a.usage.Requests++
	a.usage.InputTokens += resp.InputTokens
	a.usage.OutputTokens += resp.OutputTokens
	a.usage.CacheReadTokens += resp.CacheReadTokens
	a.usage.CacheWriteTokens += resp.CacheWriteTokens
}

func (a *Agent) Provider() Provider       { return a.provider }
func (a *Agent) Tools() *ToolRegistry     { return a.tools }
func (a *Agent) Policy() PermissionPolicy { return a.policy }
func (a *Agent) Hooks() Hooks             { return a.config.Hooks }

// Limits in effect for the current model, once a run has resolved them (nil before).
func (a *Agent) Limits() *ModelLimits {
	if a.limits == nil {
		return nil
	}
	l := *a.limits
	return &l
}

// ResolveLimits looks up the model's output cap and context window from the
// provider catalog (once per model) and combines them with any config
// overrides. A catalog failure is not fatal: the fallbacks apply.
func (a *Agent) ResolveLimits(ctx context.Context) ModelLimits {
	if a.limits != nil {
		return *a.limits
	}
	info, err := LookupModelInfo(ctx, a.provider, a.config.Model)
	if err != nil {
		slog.Warn("could not look up model limits", "model", a.config.Model, "err", err)
		info = nil
	}
	limits := ModelLimits{MaxTokens: FallbackMaxTokens, CompactAtTokens: FallbackCompactAt}
	if info != nil {
		limits.ContextWindow = info.MaxInputTokens
		if info.MaxTokens != nil {
			limits.MaxTokens = *info.MaxTokens
		}
		if info.MaxInputTokens != nil {
			limits.CompactAtTokens = uint64(float64(*info.MaxInputTokens) * compactFraction)
		}
	}
	if a.config.MaxTokens != nil {
		limits.MaxTokens = *a.config.MaxTokens
	}
	if a.config.CompactAtTokens != nil {
		limits.CompactAtTokens = *a.config.CompactAtTokens
	}
	a.limits = &limits
	return limits
}

// AddShellOutput records a shell command the user ran themselves (`!cmd` in the
// REPL / TUI) and its output as a user turn, so the model sees it on the next request.
func (a *Agent) AddShellOutput(command, output string) {
	a.History = append(a.History, UserText(ShellNote(command, output)))
}

// Clear drops all conversation context and tool history.
func (a *Agent) Clear() {
	a.History = nil
	a.pendingText.Reset()
	a.lastInputTokens = 0
}

// interruptedResult is recorded for tool calls a cancelled run never answered.
const interruptedResult = "Interrupted by user; execution may be incomplete. Do not assume changes were undone."

// Interrupt finishes a cancelled run: it keeps completed work, commits any
// partially streamed text, and pairs every outstanding tool call so the next
// request is valid. Run calls it itself when its context is cancelled.
func (a *Agent) Interrupt() {
	if a.pendingText.Len() > 0 {
		a.History = append(a.History, Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock(a.pendingText.String())}})
		a.pendingText.Reset()
	}
	i := len(a.History) - 1
	for i >= 0 && a.History[i].Role != RoleAssistant {
		i--
	}
	if i < 0 {
		return
	}
	answered := map[string]bool{}
	for _, m := range a.History[i+1:] {
		for _, b := range m.Content {
			if b.Type == BlockToolResult {
				answered[b.ToolUseID] = true
			}
		}
	}
	var missing []ContentBlock
	for _, c := range a.History[i].ToolUses() {
		if !answered[c.ID] {
			missing = append(missing, ToolResultBlock(c.ID, interruptedResult, true))
		}
	}
	switch {
	case len(missing) == 0:
	case len(a.History) == i+1:
		a.History = append(a.History, ToolResults(missing))
	default:
		a.History[i+1].Content = append(a.History[i+1].Content, missing...)
	}
}

func (a *Agent) shouldCompact() bool {
	if a.limits == nil {
		return false
	}
	at := a.limits.CompactAtTokens
	return at > 0 && a.lastInputTokens >= at
}

// Compact replaces the history with a provider-written summary of it. Returns nil when empty.
func (a *Agent) Compact(ctx context.Context) (*EvCompacted, error) {
	if len(a.History) == 0 {
		return nil, nil
	}
	limits := a.ResolveLimits(ctx)
	req := ProviderRequest{
		Model:     a.config.Model,
		System:    compactSystemPrompt,
		Messages:  []Message{UserText("Summarize the following transcript.\n\n<transcript>\n" + Transcript(a.History) + "\n</transcript>")},
		MaxTokens: 4096,
	}
	if err := budgetRequest(&req, limits); err != nil {
		return nil, err
	}
	resp, err := completeWithRetry(ctx, a.provider, a.config.MaxRetries, req, func(Event) {})
	if err != nil {
		return nil, err
	}
	a.addUsage(resp)
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

// budgetRequest reserves input space before applying the model's output ceiling.
// UTF-8 bytes plus framing headroom are a conservative estimate, not a tokenizer.
func budgetRequest(req *ProviderRequest, limits ModelLimits) error {
	req.MaxTokens = min(req.MaxTokens, limits.MaxTokens)
	if limits.ContextWindow == nil {
		return nil
	}
	window := *limits.ContextWindow
	body, err := json.Marshal([]any{req.System, req.Messages, req.Tools})
	if err != nil {
		return err
	}
	input := uint64(len(body)) + 256 + 16*uint64(len(req.Messages)+len(req.Tools))
	if input >= window {
		return fmt.Errorf("request input exceeds the estimated context budget (%d of %d tokens); compact or clear the conversation, reduce the prompt or tools, or use a larger-context model", input, window)
	}
	if available := window - input; uint64(req.MaxTokens) > available {
		req.MaxTokens = int(available)
	}
	return nil
}

// Run is the core agentic loop: send → if tool_use, execute tools, append results, repeat.
func (a *Agent) Run(ctx context.Context, userInput string, onEvent func(Event)) error {
	a.pendingText.Reset()
	// A cancelled run (Esc) keeps the session: finish the history so it stays usable.
	defer func() {
		if ctx.Err() != nil {
			a.Interrupt()
		}
	}()
	limits := a.ResolveLimits(ctx)
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
			MaxTokens: limits.MaxTokens,
			CacheKey:  a.cacheKey,
		}
		if err := budgetRequest(&req, limits); err != nil {
			return err
		}
		started := time.Now()
		var firstToken *uint64
		resp, err := streamWithRetry(ctx, a.provider, a.config.MaxRetries, req, func(ev Event) {
			if d, ok := ev.(EvTextDelta); ok {
				if firstToken == nil {
					ms := uint64(time.Since(started).Milliseconds())
					firstToken = &ms
				}
				a.pendingText.WriteString(d.Text)
			}
			onEvent(ev)
		})
		if err != nil {
			return err
		}
		a.pendingText.Reset()
		a.lastInputTokens = resp.InputTokens
		a.addUsage(resp)
		onEvent(EvTurnComplete{resp.InputTokens, resp.OutputTokens, uint64(time.Since(started).Milliseconds()), firstToken})

		if text := resp.Message.Text(); text != "" {
			onEvent(EvText{text})
		}
		calls := resp.Message.ToolUses()
		a.History = append(a.History, resp.Message)

		if resp.StopReason != StopToolUse || len(calls) == 0 {
			onEvent(EvDone{resp.StopReason})
			a.config.Hooks.RunStop(ctx, a.tc.Cwd, resp.StopReason)
			return nil
		}

		// Results land in history as each call finishes, so a cancel keeps finished work.
		a.History = append(a.History, ToolResults(make([]ContentBlock, 0, len(calls))))
		last := len(a.History) - 1
		for _, c := range calls {
			if err := ctx.Err(); err != nil {
				return err
			}
			onEvent(EvToolCall{c.ID, c.Name, c.Input})
			output, isError := "", false
			if out, err := a.execute(ctx, c.Name, c.Input); err != nil {
				output, isError = err.Error(), true
			} else {
				output = out
			}
			// Cancelled mid-call: Interrupt records it as interrupted, not as its partial output.
			if err := ctx.Err(); err != nil {
				return err
			}
			onEvent(EvToolResult{c.ID, c.Name, output, isError})
			a.History[last].Content = append(a.History[last].Content, ToolResultBlock(c.ID, output, isError))
		}

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
	hooks := a.config.Hooks
	if fb := hooks.RunPre(ctx, a.tc.Cwd, name, input); fb != nil {
		return "", Denied(fmt.Sprintf("blocked by a PreToolUse hook (`%s`): %s", fb.Command, fb.Message))
	}
	out, err := tool.Call(ctx, a.tc, input)
	text, isError := out, false
	if err != nil {
		text, isError = err.Error(), true
	}
	feedback := hooks.RunPost(ctx, a.tc.Cwd, name, input, text, isError)
	if len(feedback) == 0 {
		return out, err
	}
	for _, fb := range feedback {
		text += fmt.Sprintf("\n\n[PostToolUse hook `%s`]: %s", fb.Command, fb.Message)
	}
	if isError {
		return "", Failed("%s", text)
	}
	return text, nil
}

// streamWithRetry is provider.CompleteStream, repeated up to maxRetries times
// while it fails transiently before any text arrived (a retry after streamed
// text would show it twice, so such a failure is reported). Deltas and EvRetry
// events go to onEvent. A cancelled context is never retried.
func streamWithRetry(ctx context.Context, provider Provider, maxRetries int, req ProviderRequest, onEvent func(Event)) (ProviderResponse, error) {
	for attempt := 0; ; {
		streamed := false
		resp, err := provider.CompleteStream(ctx, req, func(delta string) {
			streamed = true
			onEvent(EvTextDelta{delta})
		})
		if err == nil || streamed || attempt >= maxRetries || ctx.Err() != nil || !IsTransient(err) {
			return resp, err
		}
		attempt++
		if err := waitRetry(ctx, attempt, maxRetries, err, onEvent); err != nil {
			return ProviderResponse{}, err
		}
	}
}

// completeWithRetry is provider.Complete with the same retry policy as streamWithRetry.
func completeWithRetry(ctx context.Context, provider Provider, maxRetries int, req ProviderRequest, onEvent func(Event)) (ProviderResponse, error) {
	for attempt := 0; ; {
		resp, err := provider.Complete(ctx, req)
		if err == nil || attempt >= maxRetries || ctx.Err() != nil || !IsTransient(err) {
			return resp, err
		}
		attempt++
		if err := waitRetry(ctx, attempt, maxRetries, err, onEvent); err != nil {
			return ProviderResponse{}, err
		}
	}
}

// waitRetry logs and announces retry `attempt`, then sleeps its delay (or returns the context's error).
func waitRetry(ctx context.Context, attempt, maxRetries int, cause error, onEvent func(Event)) error {
	delay := RetryDelay(attempt)
	slog.Warn("provider call failed; retrying", "err", cause, "attempt", attempt, "max_retries", maxRetries, "delay", delay)
	onEvent(EvRetry{Attempt: attempt, MaxRetries: maxRetries, DelayMs: uint64(delay.Milliseconds()), Error: cause.Error()})
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ShellPassthrough returns the command behind a `!cmd` prompt line (REPL / TUI):
// the text after the `!`, or false when the line isn't one (`!` alone, or a line
// that merely contains a `!`).
func ShellPassthrough(line string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "!")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

// ShellNoteLimit is the longest shell output kept in the conversation for a `!cmd` line (bytes).
const ShellNoteLimit = 30_000

// ShellNote is the user turn recorded for a `!cmd` line: the command and its (cut) output.
func ShellNote(command, output string) string {
	output = strings.TrimRightFunc(output, unicode.IsSpace)
	body := output
	switch {
	case output == "":
		body = "(no output)"
	case len(output) > ShellNoteLimit:
		cut := ShellNoteLimit
		for cut > 0 && !utf8.RuneStart(output[cut]) {
			cut--
		}
		body = fmt.Sprintf("%s\n…[%d more bytes]", output[:cut], len(output)-cut)
	}
	return "I ran this shell command myself in the project folder:\n$ " + command + "\n\n" + body
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
	return MarshalString(v, "")
}

// MarshalString encodes v like serde_json: no HTML escaping of <, >, &.
// indent "" gives compact output.
func MarshalString(v any, indent string) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if enc.Encode(v) != nil {
		return ""
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
