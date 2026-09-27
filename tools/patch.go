package tools

// apply_patch: the Codex patch grammar GPT models are trained on.
//
//	*** Begin Patch
//	*** Add File: path        (+ lines = new content)
//	*** Delete File: path
//	*** Update File: path
//	*** Move to: new/path     (optional rename)
//	@@ optional anchor line
//	 context
//	-removed
//	+added
//	*** End Patch
//
// Every change is worked out in memory first; nothing is written unless the
// whole patch applies.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

type ApplyPatch struct{}

func (ApplyPatch) Name() string { return "apply_patch" }
func (ApplyPatch) Description() string {
	return "Apply a patch in Codex's format: '*** Begin Patch', then '*** Add File: <path>' (+ lines), " +
		"'*** Delete File: <path>', or '*** Update File: <path>' (optional '*** Move to: <path>', then " +
		"hunks starting with '@@' made of ' ' context, '-' removed and '+' added lines), then '*** End Patch'. " +
		"Prefer it for multi-hunk or multi-file edits. All-or-nothing."
}
func (ApplyPatch) InputSchema() any {
	return schema(map[string]any{"patch": map[string]any{"type": "string", "description": "The full patch, '*** Begin Patch' to '*** End Patch'."}}, "patch")
}
func (ApplyPatch) IsMutating() bool { return true }
func (ApplyPatch) Call(_ context.Context, tc core.ToolContext, input json.RawMessage) (string, error) {
	var a struct {
		Patch string `json:"patch"`
	}
	if err := parse(input, &a, "patch"); err != nil {
		return "", err
	}
	out, err := Apply(a.Patch, tc.Cwd)
	if err != nil {
		return "", core.Failed("%v", err)
	}
	return out, nil
}

// patchChange is one file change, fully resolved before anything touches the disk.
type patchChange struct {
	path, text, from string // from: source of a move
	delete           bool
}

// lines splits like Rust's str::lines: no trailing empty line, "\r\n" accepted.
func lines(s string) []string {
	if s == "" {
		return nil
	}
	out := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range out {
		out[i] = strings.TrimSuffix(l, "\r")
	}
	return out
}

// Apply applies a Codex-format patch relative to cwd and returns a one-line-per-file summary.
func Apply(patch, cwd string) (string, error) {
	resolve := func(p string) string {
		p = strings.TrimSpace(p)
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(cwd, p)
	}
	ls := lines(patch)
	i := -1
	for k, l := range ls {
		if strings.TrimSpace(l) == "*** Begin Patch" {
			i = k + 1
			break
		}
	}
	if i < 0 {
		return "", errors.New("patch must start with '*** Begin Patch'")
	}
	isHeader := func(l string) bool { return strings.HasPrefix(l, "*** ") && strings.TrimSpace(l) != "*** End of File" }
	var changes []patchChange
	var summary []string

	for i < len(ls) && strings.TrimSpace(ls[i]) != "*** End Patch" {
		line := ls[i]
		i++
		if p, ok := strings.CutPrefix(line, "*** Add File: "); ok {
			var body []string
			for i < len(ls) && !isHeader(ls[i]) {
				body = append(body, strings.TrimPrefix(ls[i], "+"))
				i++
			}
			summary = append(summary, "A "+strings.TrimSpace(p))
			changes = append(changes, patchChange{path: resolve(p), text: strings.Join(body, "\n") + "\n"})
		} else if p, ok := strings.CutPrefix(line, "*** Delete File: "); ok {
			path := resolve(p)
			if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
				return "", fmt.Errorf("cannot delete %s: no such file", strings.TrimSpace(p))
			}
			summary = append(summary, "D "+strings.TrimSpace(p))
			changes = append(changes, patchChange{path: path, delete: true})
		} else if p, ok := strings.CutPrefix(line, "*** Update File: "); ok {
			src := resolve(p)
			old, err := os.ReadFile(src)
			if err != nil {
				return "", fmt.Errorf("cannot read %s: %v", strings.TrimSpace(p), err)
			}
			dest := ""
			if i < len(ls) {
				if to, ok := strings.CutPrefix(ls[i], "*** Move to: "); ok {
					dest = strings.TrimSpace(to)
					i++
				}
			}
			start := i
			for i < len(ls) && !isHeader(ls[i]) {
				i++
			}
			text, err := updateFile(string(old), ls[start:i])
			if err != nil {
				return "", fmt.Errorf("%s: %v", strings.TrimSpace(p), err)
			}
			if dest != "" {
				summary = append(summary, fmt.Sprintf("R %s -> %s", strings.TrimSpace(p), dest))
				changes = append(changes, patchChange{path: resolve(dest), text: text, from: src})
			} else {
				summary = append(summary, "M "+strings.TrimSpace(p))
				changes = append(changes, patchChange{path: src, text: text})
			}
		} else if strings.TrimSpace(line) != "" {
			return "", fmt.Errorf("unexpected line in patch: %s", line)
		}
	}
	if len(changes) == 0 {
		return "", errors.New("patch contains no file changes")
	}

	for _, c := range changes {
		if c.delete {
			if err := os.Remove(c.path); err != nil {
				return "", fmt.Errorf("deleting %s: %v", c.path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(c.path, []byte(c.text), 0o644); err != nil {
			return "", fmt.Errorf("writing %s: %v", c.path, err)
		}
		if c.from != "" && c.from != c.path {
			if err := os.Remove(c.from); err != nil {
				return "", fmt.Errorf("removing %s: %v", c.from, err)
			}
		}
	}
	return strings.Join(summary, "\n"), nil
}

type hunkLine struct {
	op   byte // ' ', '-' or '+'
	text string
}

// updateFile applies @@ hunks in order: each hunk's context + '-' lines must
// appear (in order, at or after the previous hunk) and are replaced by
// context + '+' lines. An "@@ text" header first moves past the line containing text.
func updateFile(old string, body []string) (string, error) {
	file := lines(old)
	cursor := 0
	type hunk struct {
		anchor string
		ops    []hunkLine
	}
	var hunks []hunk
	for _, l := range body {
		if anchor, ok := strings.CutPrefix(l, "@@"); ok {
			hunks = append(hunks, hunk{anchor: strings.TrimSpace(anchor)})
			continue
		}
		if strings.TrimSpace(l) == "*** End of File" {
			continue
		}
		if len(hunks) == 0 {
			hunks = append(hunks, hunk{})
		}
		h := &hunks[len(hunks)-1]
		switch {
		case l == "":
			// A bare empty line is an empty context line.
			h.ops = append(h.ops, hunkLine{' ', ""})
		case l[0] == '+' || l[0] == '-' || l[0] == ' ':
			h.ops = append(h.ops, hunkLine{l[0], l[1:]})
		default:
			return "", fmt.Errorf("bad hunk line (needs ' ', '-' or '+' prefix): %s", l)
		}
	}
	for _, h := range hunks {
		if h.anchor != "" {
			at := -1
			for k := cursor; k < len(file); k++ {
				if strings.Contains(file[k], h.anchor) {
					at = k
					break
				}
			}
			if at < 0 {
				return "", fmt.Errorf("anchor not found: %s", h.anchor)
			}
			// Hunk context usually repeats the anchor line itself, so search from it.
			cursor = at
		}
		var before []string
		for _, o := range h.ops {
			if o.op != '+' {
				before = append(before, o.text)
			}
		}
		at := len(file)
		if len(before) > 0 {
			if at = findLines(file, before, cursor); at < 0 {
				return "", fmt.Errorf("hunk does not match:\n%s", strings.Join(before, "\n"))
			}
		}
		// Context lines keep the file's own text (it may differ in trailing whitespace).
		j := at
		var after []string
		for _, o := range h.ops {
			switch o.op {
			case ' ':
				after = append(after, file[j])
				j++
			case '-':
				j++
			default:
				after = append(after, o.text)
			}
		}
		cursor = at + len(after)
		file = append(file[:at], append(after, file[j:]...)...)
	}
	out := strings.Join(file, "\n")
	if len(file) > 0 {
		out += "\n"
	}
	return out, nil
}

// findLines returns the first index at or after from where needle matches;
// exact first, then ignoring trailing whitespace. -1 when absent.
func findLines(hay, needle []string, from int) int {
	fits := func(eq func(a, b string) bool) int {
		for s := from; s+len(needle) <= len(hay); s++ {
			ok := true
			for k, n := range needle {
				if !eq(hay[s+k], n) {
					ok = false
					break
				}
			}
			if ok {
				return s
			}
		}
		return -1
	}
	if at := fits(func(a, b string) bool { return a == b }); at >= 0 {
		return at
	}
	trim := func(s string) string { return strings.TrimRight(s, " \t\r") }
	return fits(func(a, b string) bool { return trim(a) == trim(b) })
}
