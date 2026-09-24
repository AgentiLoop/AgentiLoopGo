package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

func newPolicy(yes bool) core.PermissionPolicy {
	if yes {
		return core.AllowAll{}
	}
	return &interactivePolicy{always: map[string]bool{}, in: os.Stdin, out: os.Stderr}
}

// interactivePolicy prompts on stderr for every mutating tool call. `a` = always
// allow this tool for the session.
type interactivePolicy struct {
	mu     sync.Mutex
	always map[string]bool
	in     io.Reader
	out    io.Writer
	// ask, when set (REPL), reads the answer through the line editor that owns stdin.
	ask func(prompt string) (string, error)
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return core.MarshalString(v, "  ")
}

func (p *interactivePolicy) Check(_ context.Context, tool string, mutating bool, input json.RawMessage) core.Permission {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !mutating || p.always[tool] {
		return core.Allow
	}
	fmt.Fprintf(p.out, "\n\u26a0 %s wants to run:\n%s\n", tool, prettyJSON(input))
	question := fmt.Sprintf("Allow? [y]es / [n]o / [a]lways for `%s`: ", tool)
	var line string
	if p.ask != nil {
		line, _ = p.ask(question)
	} else {
		fmt.Fprint(p.out, question)
		line, _ = readLineFrom(p.in)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return core.Allow
	case "a", "always":
		p.always[tool] = true
		return core.Allow
	}
	return core.Deny
}

// readLineFrom reads one line byte by byte so no input is buffered away from
// the line editor that owns stdin.
func readLineFrom(r io.Reader) (string, error) {
	var sb strings.Builder
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n > 0 {
			if b[0] == '\n' {
				return sb.String(), nil
			}
			sb.WriteByte(b[0])
		}
		if err != nil {
			return sb.String(), err
		}
	}
}

// ---- prompt history (shared by the REPL and the TUI) -----------------------------

// historyMax entries are kept in the history file.
const historyMax = 1000

// loadHistory reads rustyline's "#V2" history format (`\\` and `\n` escaped);
// plain one-entry-per-line files are accepted too. Missing file → empty.
func loadHistory(path string) []string {
	f, err := os.Open(path)
	if path == "" || err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var out []string
	first, v2 := true, false
	for sc.Scan() {
		l := strings.TrimSuffix(sc.Text(), "\r")
		if first {
			first = false
			if l == "#V2" {
				v2 = true
				continue
			}
		}
		if l == "" {
			continue
		}
		if v2 {
			l = unescapeHistory(l)
		}
		out = append(out, l)
	}
	return out
}

// saveHistory writes the last historyMax entries in rustyline's "#V2" format
// so the Rust CLI can read the same file.
func saveHistory(path string, history []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("#V2\n")
	for _, h := range history[max(0, len(history)-historyMax):] {
		sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(h, `\`, `\\`), "\n", `\n`))
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func unescapeHistory(l string) string {
	var sb strings.Builder
	rs := []rune(l)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\\' {
			sb.WriteRune(rs[i])
			continue
		}
		if i+1 >= len(rs) {
			sb.WriteRune('\\')
			break
		}
		i++
		if rs[i] == 'n' {
			sb.WriteRune('\n')
		} else {
			sb.WriteRune(rs[i])
		}
	}
	return sb.String()
}
