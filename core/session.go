package core

import (
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
// a half-written session) and bumps Updated.
func (s *Session) Save(dir string) (string, error) {
	s.Updated = now()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
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
