package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Session is one persisted conversation: one JSON file per session so a run can be resumed.
type Session struct {
	ID string `json:"id"`
	// Unix seconds.
	Created  uint64    `json:"created"`
	Updated  uint64    `json:"updated"`
	Cwd      string    `json:"cwd"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	History  []Message `json:"history"`
}

func now() uint64 { return uint64(time.Now().Unix()) }

func NewSession(cwd, provider, model string) *Session {
	created := now()
	return &Session{
		ID:       fmt.Sprintf("%d-%d", created, os.Getpid()),
		Created:  created,
		Updated:  created,
		Cwd:      cwd,
		Provider: provider,
		Model:    model,
		History:  []Message{},
	}
}

// Title is the first user prompt, trimmed to one line of at most 60 chars.
func (s *Session) Title() string {
	first := ""
outer:
	for _, m := range s.History {
		if m.Role != RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == BlockText {
				first = strings.TrimSpace(strings.SplitN(b.Text, "\n", 2)[0])
				break outer
			}
		}
	}
	if utf8.RuneCountInString(first) <= 60 {
		return first
	}
	return string([]rune(first)[:60]) + "…"
}

func SessionPath(dir, id string) string { return filepath.Join(dir, id+".json") }

// Save writes <dir>/<id>.json (via a temp file + rename so a crash never leaves
// a half-written session) and bumps Updated. Like the Rust CLI (serde_json
// pretty), <, > and & are written as-is, not HTML-escaped.
func (s *Session) Save(dir string) (string, error) {
	s.Updated = now()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return "", err
	}
	data := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))

	path := SessionPath(dir, s.ID)
	tmp := filepath.Join(dir, "."+s.ID+".json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

func LoadSession(dir, id string) (*Session, error) {
	path := SessionPath(dir, id)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no session %s in %s: %w", id, dir, err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return &s, nil
}

// ListSessions returns all sessions in dir, most recently updated first. Malformed files are skipped.
func ListSessions(dir string) ([]*Session, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		var s Session
		if err == nil {
			err = json.Unmarshal(data, &s)
		}
		if err != nil {
			slog.Warn("skipping session", "path", path, "err", err)
			continue
		}
		out = append(out, &s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated != out[j].Updated {
			return out[i].Updated > out[j].Updated
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// LatestSessionFor returns the most recently updated session whose Cwd matches, or nil.
func LatestSessionFor(dir, cwd string) (*Session, error) {
	all, err := ListSessions(dir)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.Cwd == cwd {
			return s, nil
		}
	}
	return nil, nil
}

// exportResultChars: tool output longer than this is cut in an export.
const exportResultChars = 2000

// fenced is a code block whose fence is longer than any backtick run inside body.
func fenced(lang, body string) string {
	longest, run := 0, 0
	for _, c := range body {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	fence := strings.Repeat("`", n)
	return fence + lang + "\n" + body + "\n" + fence
}

// exportJSON re-encodes raw JSON with sorted keys and no HTML escaping, like serde_json::to_string.
func exportJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := marshalNoEscape(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// ToMarkdown renders the conversation as Markdown: a header, then "## You" / "## AgentiLoop"
// sections with tool calls and (cut) results in code blocks.
func (s *Session) ToMarkdown() string {
	title := s.Title()
	if title == "" {
		title = "AgentiLoop session"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n- Session: %s\n- Provider: %s · Model: %s\n- Directory: %s\n", title, s.ID, s.Provider, s.Model, s.Cwd)
	var section Role
	for _, m := range s.History {
		hasText := false
		for _, b := range m.Content {
			hasText = hasText || b.Type == BlockText
		}
		if hasText && section != m.Role {
			if m.Role == RoleUser {
				out.WriteString("\n## You\n")
			} else {
				out.WriteString("\n## AgentiLoop\n")
			}
			section = m.Role
		}
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				fmt.Fprintf(&out, "\n%s\n", strings.TrimRight(b.Text, " \t\r\n"))
			case BlockToolUse:
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				fmt.Fprintf(&out, "\n**Tool: `%s`**\n\n%s\n", b.Name, fenced("json", exportJSON(input)))
			case BlockToolResult:
				body := b.Content
				if utf8.RuneCountInString(body) > exportResultChars {
					body = string([]rune(body)[:exportResultChars]) + "\n…[truncated]"
				}
				label := "Result"
				if b.IsError {
					label = "Error"
				}
				fmt.Fprintf(&out, "\n**%s**\n\n%s\n", label, fenced("", body))
			}
		}
	}
	return out.String()
}
