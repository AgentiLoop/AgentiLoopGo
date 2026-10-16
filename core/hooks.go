package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Hooks: shell commands the user runs at fixed points of the agent loop, configured in
// <project>/.agentiloop/hooks.json and ~/.agentiloop/hooks.json (both are loaded):
//
//	{
//	  "hooks": {
//	    "PreToolUse":  [{ "matcher": "bash,write_file", "command": "sh .agentiloop/check.sh" }],
//	    "PostToolUse": [{ "matcher": "edit_file", "command": "cargo fmt" }],
//	    "Stop":        [{ "command": "say done" }]
//	  }
//	}
//
// "matcher" uses the --allow-tool syntax (names, mcp_* prefixes, *); missing = every tool.
// Each command runs through `sh -c` (`cmd /C` on Windows) in the project folder with one JSON
// object on stdin (event, cwd, tool, input, and for PostToolUse output + is_error; for Stop
// stop_reason). AGENTILOOP_HOOK_EVENT and AGENTILOOP_TOOL are set too.
//
// Exit code 2 means "talk to the model": a PreToolUse hook blocks the call and its stderr becomes
// the tool error; a PostToolUse hook's stderr is appended to the tool result. Any other exit code
// is ignored (logged), and a hook that fails to start or exceeds HookTimeout is logged and skipped.

const (
	// HookTimeout: a hook that runs longer than this is killed and ignored.
	HookTimeout = 60 * time.Second
	// HookTalkExitCode makes a hook's stderr visible to the model.
	HookTalkExitCode = 2
	// hookOutputLimit: tool output longer than this is cut before it is handed to a PostToolUse hook.
	hookOutputLimit = 20_000
	// hookFeedbackLimit: hook stderr/stdout longer than this is cut before it reaches the model.
	hookFeedbackLimit = 4_000
)

type HookEvent string

const (
	PreToolUse  HookEvent = "PreToolUse"
	PostToolUse HookEvent = "PostToolUse"
	StopHook    HookEvent = "Stop"
)

type Hook struct {
	Event HookEvent
	// Matcher says which tools it applies to (ignored for Stop). Empty = all.
	Matcher ToolPatterns
	Command string
	// Source is the file it came from.
	Source string
}

func (h Hook) applies(event HookEvent, tool string) bool {
	return h.Event == event && (event == StopHook || len(h.Matcher) == 0 || h.Matcher.Matches(tool))
}

// HookFeedback is what one hook said back, for callers that render it.
type HookFeedback struct {
	Command string
	Message string
}

type Hooks []Hook

type hooksFile struct {
	Hooks map[string][]hookEntry `json:"hooks"`
}

type hookEntry struct {
	Matcher string `json:"matcher"`
	Command string `json:"command"`
}

// HookPaths are the file names searched, user first so project hooks run last. home is the
// ~/.agentiloop folder ("" = none).
func HookPaths(cwd, home string) []string {
	var out []string
	if home != "" {
		out = append(out, filepath.Join(home, "hooks.json"))
	}
	return append(out, filepath.Join(cwd, ".agentiloop", "hooks.json"))
}

// LoadHooks parses the given files (missing ones are skipped). Problems are returned as messages,
// one per file or entry, and never stop the rest from loading.
func LoadHooks(paths []string) (Hooks, []string) {
	var hooks Hooks
	var errs []string
	for _, path := range paths {
		text, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		var file hooksFile
		if err := json.Unmarshal(text, &file); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		names := make([]string, 0, len(file.Hooks))
		for name := range file.Hooks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			event := HookEvent(name)
			if event != PreToolUse && event != PostToolUse && event != StopHook {
				errs = append(errs, fmt.Sprintf("%s: unknown hook event `%s` (PreToolUse, PostToolUse, Stop)", path, name))
				continue
			}
			for _, e := range file.Hooks[name] {
				if strings.TrimSpace(e.Command) == "" {
					errs = append(errs, fmt.Sprintf("%s: %s hook with an empty command", path, name))
					continue
				}
				hooks = append(hooks, Hook{Event: event, Matcher: NewToolPatterns([]string{e.Matcher}), Command: e.Command, Source: path})
			}
		}
	}
	return hooks, errs
}

// WithoutStop is the same hooks minus the Stop ones (a sub-agent finishing is not the end of the request).
func (hs Hooks) WithoutStop() Hooks {
	var out Hooks
	for _, h := range hs {
		if h.Event != StopHook {
			out = append(out, h)
		}
	}
	return out
}

// Listing is the text for /hooks.
func (hs Hooks) Listing() string {
	if len(hs) == 0 {
		return "no hooks configured (add .agentiloop/hooks.json or ~/.agentiloop/hooks.json)"
	}
	lines := make([]string, 0, len(hs))
	for _, h := range hs {
		m := ""
		switch {
		case h.Event == StopHook:
		case len(h.Matcher) == 0:
			m = "*"
		default:
			m = strings.Join(h.Matcher, ",")
		}
		lines = append(lines, fmt.Sprintf("%-12s %-20s %s  (%s)", h.Event, m, h.Command, h.Source))
	}
	return strings.Join(lines, "\n")
}

// RunPre runs the PreToolUse hooks for tool; a non-nil result is the one that blocked the call.
func (hs Hooks) RunPre(ctx context.Context, cwd, tool string, input json.RawMessage) *HookFeedback {
	for _, h := range hs {
		if !h.applies(PreToolUse, tool) {
			continue
		}
		payload := map[string]any{"event": "PreToolUse", "cwd": cwd, "tool": tool, "input": rawOrEmpty(input)}
		if msg, ok := runHook(ctx, h, cwd, tool, payload); ok {
			return &HookFeedback{h.Command, msg}
		}
	}
	return nil
}

// RunPost runs the PostToolUse hooks for tool; the messages of those that exited 2.
func (hs Hooks) RunPost(ctx context.Context, cwd, tool string, input json.RawMessage, output string, isError bool) []HookFeedback {
	var out []HookFeedback
	for _, h := range hs {
		if !h.applies(PostToolUse, tool) {
			continue
		}
		payload := map[string]any{
			"event": "PostToolUse", "cwd": cwd, "tool": tool, "input": rawOrEmpty(input),
			"output": cutHook(output, hookOutputLimit), "is_error": isError,
		}
		if msg, ok := runHook(ctx, h, cwd, tool, payload); ok {
			out = append(out, HookFeedback{h.Command, msg})
		}
	}
	return out
}

// RunStop runs the Stop hooks; what they say is ignored.
func (hs Hooks) RunStop(ctx context.Context, cwd string, stopReason StopReason) {
	for _, h := range hs {
		if h.Event != StopHook {
			continue
		}
		runHook(ctx, h, cwd, "", map[string]any{"event": "Stop", "cwd": cwd, "stop_reason": string(stopReason)})
	}
}

func rawOrEmpty(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return json.RawMessage("{}")
	}
	return input
}

func cutHook(s string, max int) string {
	if len(s) <= max {
		return s
	}
	at := max
	for at > 0 && !utf8.RuneStart(s[at]) {
		at--
	}
	return fmt.Sprintf("%s…[%d more bytes]", s[:at], len(s)-at)
}

// runHook runs one hook. (message, true) when it exited with HookTalkExitCode (stderr, else
// stdout, else a default line); ("", false) otherwise. Failures to start or finish are logged.
func runHook(ctx context.Context, h Hook, cwd, tool string, payload map[string]any) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, HookTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", h.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", h.Command)
	}
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "AGENTILOOP_HOOK_EVENT="+string(h.Event), "AGENTILOOP_TOOL="+tool)
	body, _ := json.Marshal(payload)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		slog.Warn("hook timed out", "event", h.Event, "command", h.Command, "after", HookTimeout)
		return "", false
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return "", false
	case errors.As(err, &exit) && exit.ExitCode() == HookTalkExitCode:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = fmt.Sprintf("hook `%s` exited 2", h.Command)
		}
		return cutHook(msg, hookFeedbackLimit), true
	case errors.As(err, &exit):
		slog.Warn("hook exited with an error (ignored)", "event", h.Event, "command", h.Command, "code", exit.ExitCode(), "stderr", strings.TrimSpace(stderr.String()))
	default:
		slog.Warn("hook could not start", "event", h.Event, "command", h.Command, "err", err)
	}
	return "", false
}
