package main

// Line diffs for the files the agent writes or edits, like Claude Code:
// -/+ lines with line numbers on red/green tints in the transcript, and a
// running "files changed" list for the TUI's side pane.
//
// diffTracker snapshots a file when write_file/edit_file is called and diffs it
// once the tool succeeds. It must see events synchronously, before the tool
// runs, so it lives on the agent side of the TUI queue.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"golang.org/x/term"
)

const (
	// diffContext is the number of unchanged lines shown around each change.
	diffContext = 3
	// inlineMax is the number of diff lines shown inline per edit; the rest is summarized.
	inlineMax = 40
	// lcsMaxCells caps the LCS table; bigger rewrites show as remove-all/add-all.
	lcsMaxCells = 4_000_000
)

type diffTag int

const (
	tagContext diffTag = iota
	tagRemoved
	tagAdded
)

type diffLine struct {
	Tag diffTag
	// No is the old line number for removed lines, the new one otherwise.
	No   int
	Text string
}

type fileDiff struct {
	// Path is relative to the working directory when the file is inside it.
	Path           string
	Added, Removed int
	// Hunks are changed regions with context; drawn with a ⋮ between them.
	Hunks [][]diffLine
}

func (d fileDiff) empty() bool { return d.Added == 0 && d.Removed == 0 }

// summary is e.g. "Added 3 lines, removed 1 line".
func (d fileDiff) summary() string {
	n := func(k int) string {
		if k == 1 {
			return "1 line"
		}
		return fmt.Sprintf("%d lines", k)
	}
	switch {
	case d.Added == 0 && d.Removed == 0:
		return "No changes"
	case d.Removed == 0:
		return "Added " + n(d.Added)
	case d.Added == 0:
		return "Removed " + n(d.Removed)
	}
	return "Added " + n(d.Added) + ", removed " + n(d.Removed)
}

// ext is the file extension, for syntax highlighting.
func (d fileDiff) ext() string { return strings.TrimPrefix(filepath.Ext(d.Path), ".") }

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.ReplaceAll(strings.TrimSuffix(l, "\r"), "\t", "    ")
	}
	return lines
}

type diffOp struct {
	tag            diffTag
	oldIdx, newIdx int
}

// lineOps is an LCS line diff of a against b (removals before additions).
func lineOps(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []diffOp
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{tagContext, i, i})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(ma)*len(mb) > lcsMaxCells {
		for i := range ma {
			ops = append(ops, diffOp{tagRemoved, pre + i, -1})
		}
		for j := range mb {
			ops = append(ops, diffOp{tagAdded, -1, pre + j})
		}
	} else {
		// lcs[i][j] = LCS length of ma[i:], mb[j:].
		n, m := len(ma), len(mb)
		lcs := make([][]int, n+1)
		for i := range lcs {
			lcs[i] = make([]int, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && ma[i] == mb[j]:
				ops = append(ops, diffOp{tagContext, pre + i, pre + j})
				i, j = i+1, j+1
			case j == m || (i < n && lcs[i+1][j] >= lcs[i][j+1]):
				ops = append(ops, diffOp{tagRemoved, pre + i, -1})
				i++
			default:
				ops = append(ops, diffOp{tagAdded, -1, pre + j})
				j++
			}
		}
	}
	for k := 0; k < suf; k++ {
		ops = append(ops, diffOp{tagContext, len(a) - suf + k, len(b) - suf + k})
	}
	return ops
}

// computeDiff diffs before (nil = new file) against after.
func computeDiff(path string, before *string, after string) fileDiff {
	var a []string
	if before != nil {
		a = splitLines(*before)
	}
	b := splitLines(after)
	ops := lineOps(a, b)
	d := fileDiff{Path: path}
	// Keep context lines within diffContext of a change; gaps start new hunks.
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.tag == tagContext {
			continue
		}
		for k := max(i-diffContext, 0); k <= min(i+diffContext, len(ops)-1); k++ {
			keep[k] = true
		}
	}
	var hunk []diffLine
	for i, op := range ops {
		if !keep[i] {
			if hunk != nil {
				d.Hunks = append(d.Hunks, hunk)
				hunk = nil
			}
			continue
		}
		l := diffLine{Tag: op.tag}
		switch op.tag {
		case tagContext:
			l.No, l.Text = op.newIdx+1, b[op.newIdx]
		case tagRemoved:
			l.No, l.Text = op.oldIdx+1, a[op.oldIdx]
			d.Removed++
		case tagAdded:
			l.No, l.Text = op.newIdx+1, b[op.newIdx]
			d.Added++
		}
		hunk = append(hunk, l)
	}
	if hunk != nil {
		d.Hunks = append(d.Hunks, hunk)
	}
	return d
}

// fileChange is one successful write/edit: this edit's diff, and the file's
// total change since the agent first touched it this session.
type fileChange struct {
	Edit, Total fileDiff
}

type pendingEdit struct {
	abs    string
	before *string
}

type diffTracker struct {
	cwd     string
	pending map[string]pendingEdit
	// originals: content of each touched file before its first change (nil = didn't exist).
	originals map[string]*string
}

func newDiffTracker(cwd string) *diffTracker {
	return &diffTracker{cwd: cwd, pending: map[string]pendingEdit{}, originals: map[string]*string{}}
}

// observe must see every agent event, in order, before the UI does.
func (t *diffTracker) observe(ev core.Event) *fileChange {
	switch e := ev.(type) {
	case core.EvToolCall:
		if e.Name != "write_file" && e.Name != "edit_file" {
			return nil
		}
		var in struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(e.Input, &in) != nil || in.Path == "" {
			return nil
		}
		abs := in.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(t.cwd, abs)
		}
		var before *string
		if b, err := os.ReadFile(abs); err == nil {
			s := string(b)
			before = &s
		} else if !os.IsNotExist(err) {
			return nil
		}
		t.pending[e.ID] = pendingEdit{abs, before}
	case core.EvToolResult:
		p, ok := t.pending[e.ID]
		if !ok {
			return nil
		}
		delete(t.pending, e.ID)
		if e.IsError {
			return nil
		}
		b, err := os.ReadFile(p.abs)
		if err != nil {
			return nil
		}
		after := string(b)
		orig, seen := t.originals[p.abs]
		if !seen {
			orig = p.before
			t.originals[p.abs] = orig
		}
		shown := p.abs
		if rel, err := filepath.Rel(t.cwd, p.abs); err == nil && !strings.HasPrefix(rel, "..") {
			shown = rel
		}
		return &fileChange{Edit: computeDiff(shown, p.before, after), Total: computeDiff(shown, orig, after)}
	}
	return nil
}

// Tints shared with the TUI.
const (
	removedBG = 0x4B1212
	addedBG   = 0x123D16
	removedFG = 0xFF7B72
	addedFG   = 0x7EE787
)

// diffGutter is the number + sign for a line, e.g. "   15 - ".
func diffGutter(l diffLine) string {
	sign := " "
	switch l.Tag {
	case tagRemoved:
		sign = "-"
	case tagAdded:
		sign = "+"
	}
	return fmt.Sprintf("%5d %s ", l.No, sign)
}

// colorStderr: stderr is a terminal and NO_COLOR is unset.
func colorStderr() bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor && term.IsTerminal(int(os.Stderr.Fd()))
}

// ansiDiff renders a diff for the REPL and one-shot mode: tinted rows with
// color, plain -/+ text otherwise. At most max diff lines.
func ansiDiff(d fileDiff, maxLines int, color bool) string {
	rgbSeq := func(kind int, c int) string {
		return fmt.Sprintf("\x1b[%d;2;%d;%d;%dm", kind, c>>16, (c>>8)&0xFF, c&0xFF)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "   ⎿ %s: %s\n", d.Path, d.summary())
	shown, total := 0, 0
	for _, h := range d.Hunks {
		total += len(h)
	}
	for hi, hunk := range d.Hunks {
		if hi > 0 && shown < maxLines {
			fmt.Fprintf(&sb, "%7s\n", "⋮")
		}
		for _, l := range hunk {
			if shown == maxLines {
				break
			}
			shown++
			row := diffGutter(l) + l.Text
			switch {
			case !color:
				sb.WriteString(row)
			// \x1b[K paints the background to the end of the row.
			case l.Tag == tagRemoved:
				sb.WriteString(rgbSeq(48, removedBG) + rgbSeq(38, removedFG) + row + "\x1b[K\x1b[0m")
			case l.Tag == tagAdded:
				sb.WriteString(rgbSeq(48, addedBG) + rgbSeq(38, addedFG) + row + "\x1b[K\x1b[0m")
			default:
				sb.WriteString("\x1b[2m" + row + "\x1b[0m")
			}
			sb.WriteByte('\n')
		}
	}
	if total > shown {
		fmt.Fprintf(&sb, "      … %d more lines\n", total-shown)
	}
	return sb.String()
}
