// Package tools holds the built-in tools: read_file, write_file, edit_file, list_dir, bash.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

// DefaultRegistry is pre-populated with every built-in tool.
func DefaultRegistry() *core.ToolRegistry {
	r := core.NewToolRegistry()
	r.Register(ReadFile{}).Register(WriteFile{}).Register(EditFile{}).Register(ListDir{}).Register(Bash{})
	return r
}

func resolve(tc core.ToolContext, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(tc.Cwd, p)
}

// parse decodes input into v, reporting missing required fields and type
// mismatches as invalid input (like serde does).
func parse(input json.RawMessage, v any, required ...string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return core.InvalidInput("%v", err)
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return core.InvalidInput("missing field `%s`", k)
		}
	}
	if err := json.Unmarshal(input, v); err != nil {
		return core.InvalidInput("%v", err)
	}
	return nil
}

func ioErr(err error) error { return core.Failed("%v", err) }

func schema(props map[string]any, required ...string) any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// ---------------------------------------------------------------- read_file

type ReadFile struct{}

func (ReadFile) Name() string { return "read_file" }
func (ReadFile) Description() string {
	return "Read a UTF-8 text file. Returns the full contents with 1-based line numbers."
}
func (ReadFile) InputSchema() any {
	return schema(map[string]any{"path": map[string]any{"type": "string", "description": "File path (absolute or relative to cwd)"}}, "path")
}
func (ReadFile) IsMutating() bool { return false }
func (ReadFile) Call(_ context.Context, tc core.ToolContext, input json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := parse(input, &a, "path"); err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolve(tc, a.Path))
	if err != nil {
		return "", ioErr(err)
	}
	if !utf8.Valid(data) {
		return "", core.Failed("stream did not contain valid UTF-8")
	}
	lines := splitLines(string(data))
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = fmt.Sprintf("%5d\u2502%s", i+1, l)
	}
	return strings.Join(out, "\n"), nil
}

// splitLines matches Rust's str::lines: "\n" or "\r\n" separators, no trailing empty line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// --------------------------------------------------------------- write_file

type WriteFile struct{}

func (WriteFile) Name() string { return "write_file" }
func (WriteFile) Description() string {
	return "Create or overwrite a file with the given content. Creates parent directories."
}
func (WriteFile) InputSchema() any {
	return schema(map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "path", "content")
}
func (WriteFile) IsMutating() bool { return true }
func (WriteFile) Call(_ context.Context, tc core.ToolContext, input json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := parse(input, &a, "path", "content"); err != nil {
		return "", err
	}
	path := resolve(tc, a.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", ioErr(err)
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return "", ioErr(err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), path), nil
}

// ---------------------------------------------------------------- edit_file

type EditFile struct{}

func (EditFile) Name() string { return "edit_file" }
func (EditFile) Description() string {
	return "Replace an exact string in a file. `old_string` must match exactly once unless `replace_all` is true."
}
func (EditFile) InputSchema() any {
	return schema(map[string]any{
		"path":        map[string]any{"type": "string"},
		"old_string":  map[string]any{"type": "string"},
		"new_string":  map[string]any{"type": "string"},
		"replace_all": map[string]any{"type": "boolean", "default": false},
	}, "path", "old_string", "new_string")
}
func (EditFile) IsMutating() bool { return true }
func (EditFile) Call(_ context.Context, tc core.ToolContext, input json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := parse(input, &a, "path", "old_string", "new_string"); err != nil {
		return "", err
	}
	path := resolve(tc, a.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ioErr(err)
	}
	text := string(data)
	n := 0
	if a.OldString != "" {
		n = strings.Count(text, a.OldString)
	} else {
		n = utf8.RuneCountInString(text) + 1 // Rust's matches("") semantics
	}
	if n == 0 {
		return "", core.Failed("old_string not found")
	}
	if n > 1 && !a.ReplaceAll {
		return "", core.Failed("old_string matched %d times; add context or set replace_all", n)
	}
	limit := 1
	if a.ReplaceAll {
		limit = -1
	}
	if err := os.WriteFile(path, []byte(strings.Replace(text, a.OldString, a.NewString, limit)), 0o644); err != nil {
		return "", ioErr(err)
	}
	return fmt.Sprintf("replaced %d occurrence(s) in %s", n, path), nil
}

// ----------------------------------------------------------------- list_dir

type ListDir struct{}

func (ListDir) Name() string { return "list_dir" }
func (ListDir) Description() string {
	return "List entries in a directory. Directories are suffixed with '/'."
}
func (ListDir) InputSchema() any {
	return schema(map[string]any{"path": map[string]any{"type": "string", "default": "."}})
}
func (ListDir) IsMutating() bool { return false }
func (ListDir) Call(_ context.Context, tc core.ToolContext, input json.RawMessage) (string, error) {
	a := struct {
		Path string `json:"path"`
	}{Path: "."}
	if err := parse(input, &a); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(resolve(tc, a.Path))
	if err != nil {
		return "", ioErr(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "\n"), nil
}

// --------------------------------------------------------------------- bash

const maxOutput = 30_000

type Bash struct{}

func (Bash) Name() string { return "bash" }
func (Bash) Description() string {
	return "Run a shell command in the project directory and return stdout+stderr. " +
		"Uses `sh -c` on Unix and `cmd /C` on Windows."
}
func (Bash) InputSchema() any {
	return schema(map[string]any{
		"command":      map[string]any{"type": "string"},
		"timeout_secs": map[string]any{"type": "integer", "default": 120},
	}, "command")
}
func (Bash) IsMutating() bool { return true }
func (Bash) Call(ctx context.Context, tc core.ToolContext, input json.RawMessage) (string, error) {
	a := struct {
		Command     string `json:"command"`
		TimeoutSecs uint64 `json:"timeout_secs"`
	}{TimeoutSecs: 120}
	if err := parse(input, &a, "command"); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.TimeoutSecs)*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", a.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", a.Command)
	}
	cmd.Dir = tc.Cwd
	killTreeOnCancel(cmd)
	cmd.WaitDelay = time.Second // don't hang on grandchildren holding the pipes
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", core.Failed("timed out after %ds", a.TimeoutSecs)
	}
	exitCode := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return "", ioErr(err)
		}
		exitCode = ee.ExitCode()
	}
	s := strings.ToValidUTF8(stdout.String(), "\uFFFD")
	if e := stderr.String(); e != "" {
		if s != "" {
			s += "\n"
		}
		s += strings.ToValidUTF8(e, "\uFFFD")
	}
	if len(s) > maxOutput {
		cut := maxOutput
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "\n…[truncated]"
	}
	if exitCode != 0 {
		s += fmt.Sprintf("\n[exit status: %d]", exitCode)
	}
	return s, nil
}
