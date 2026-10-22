package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"github.com/gdamore/tcell/v2"
)

func ptr(s string) *string { return &s }

func gutterRows(h []diffLine) []string {
	var out []string
	for _, l := range h {
		out = append(out, diffGutter(l)+l.Text)
	}
	return out
}

func TestDiffNumbersLinesAndCounts(t *testing.T) {
	d := computeDiff("a.rs", ptr("one\ntwo\nthree\n"), "one\nTWO\nthree\nfour\n")
	if d.Added != 2 || d.Removed != 1 {
		t.Fatalf("counts %d/%d", d.Added, d.Removed)
	}
	want := []string{"    1   one", "    2 - two", "    2 + TWO", "    3   three", "    4 + four"}
	if got := gutterRows(d.Hunks[0]); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows %q", got)
	}
	if d.summary() != "Added 2 lines, removed 1 line" || d.ext() != "rs" {
		t.Fatal(d.summary(), d.ext())
	}
}

func TestFarApartChangesMakeSeparateHunks(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		b.WriteString("l" + strconv.Itoa(i) + "\n")
	}
	before := b.String()
	after := strings.Replace(strings.Replace(before, "l2\n", "x\n", 1), "l28\n", "y\n", 1)
	d := computeDiff("f", &before, after)
	if len(d.Hunks) != 2 {
		t.Fatalf("hunks %d", len(d.Hunks))
	}
	found := false
	for _, l := range d.Hunks[1] {
		found = found || (l.Tag == tagAdded && l.No == 28 && l.Text == "y")
	}
	if !found {
		t.Fatal(gutterRows(d.Hunks[1]))
	}
}

func TestNewFileIsAllAddedAndAnsiIsCapped(t *testing.T) {
	d := computeDiff("n.txt", nil, strings.Repeat("x\n", 50))
	if d.Added != 50 || d.Removed != 0 || d.summary() != "Added 50 lines" {
		t.Fatal(d.summary())
	}
	s := ansiDiff(d, 5, false)
	if !strings.HasPrefix(s, "   ⎿ n.txt: Added 50 lines\n") || !strings.Contains(s, "    5 + x") || strings.Contains(s, "    6 + x") || !strings.Contains(s, "… 45 more lines") {
		t.Fatal(s)
	}
}

func TestTrackerDiffsEditsAndKeepsSessionTotal(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	tr := newDiffTracker(dir)
	in, _ := json.Marshal(map[string]string{"path": "a.txt"})
	call := func(id, name string) core.Event { return core.EvToolCall{ID: id, Name: name, Input: in} }
	ok := func(id string) core.Event { return core.EvToolResult{ID: id} }

	if tr.observe(call("1", "write_file")) != nil {
		t.Fatal("call should not produce a change")
	}
	os.WriteFile(file, []byte("a\nb\n"), 0o644)
	c := tr.observe(ok("1"))
	if c == nil || c.Edit.Path != "a.txt" || c.Edit.Added != 2 || c.Edit.Removed != 0 {
		t.Fatalf("%+v", c)
	}
	tr.observe(call("2", "edit_file"))
	os.WriteFile(file, []byte("a\nB\n"), 0o644)
	c = tr.observe(ok("2"))
	if c.Edit.Added != 1 || c.Edit.Removed != 1 || c.Total.Added != 2 || c.Total.Removed != 0 {
		t.Fatalf("%+v", c)
	}
	tr.observe(call("3", "edit_file"))
	if tr.observe(core.EvToolResult{ID: "3", IsError: true}) != nil {
		t.Fatal("failed call produced a change")
	}
	if tr.observe(call("4", "read_file")) != nil || tr.observe(ok("4")) != nil {
		t.Fatal("read_file produced a change")
	}
}

func change(path, before, after string) UiMsg {
	d := computeDiff(path, &before, after)
	return uiDiff{fileChange{Edit: d, Total: d}}
}

func TestEditDiffShowsNumberedTintedLines(t *testing.T) {
	a := NewApp("s")
	a.Apply(change("src/a.txt", "one\ntwo\nthree\n", "one\nTWO\nthree\n"))
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(60, 11)
	a.Draw(s)
	s.Show()
	cells, w, _ := s.(tcell.SimulationScreen).GetContents()
	row := func(y int) string {
		var b strings.Builder
		for x := 0; x < w; x++ {
			b.WriteString(string(cells[y*w+x].Runes))
		}
		return b.String()
	}
	bg := func(x, y int) tcell.Color { _, c, _ := cells[y*w+x].Style.Decompose(); return c }
	if !strings.HasPrefix(row(0), "⎿ src/a.txt: Added 1 line, removed 1 line") {
		t.Fatal(row(0))
	}
	if !strings.HasPrefix(row(2), "    2 - two") || !strings.HasPrefix(row(3), "    2 + TWO") {
		t.Fatal(row(2), "|", row(3))
	}
	// The tint runs to the right edge, not just under the text.
	if bg(59, 2) != rgb(removedBG) || bg(59, 3) != rgb(addedBG) || bg(59, 1) == rgb(addedBG) {
		t.Fatal("tints", bg(59, 2), bg(59, 3), bg(59, 1))
	}
}

func TestFilesPaneStacksAllDiffsAndToggles(t *testing.T) {
	a := NewApp("s")
	a.Apply(change("a.rs", "x\n", "y\n"))
	a.Apply(change("docs/b.md", "", "1\n2\n3\n"))
	s := screen(t, a, 120, 24)
	for _, want := range []string{"2 files changed +4 -1", "a.rs", "+1 -1", "docs/b.md", "+3 -0", "1 - x", "1 + y", "3 + 3"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
	// Narrow terminals and Ctrl-F hide it.
	if strings.Contains(screen(t, a, 80, 24), "files changed") {
		t.Fatal("pane shown on narrow screen")
	}
	a.HandleKey(key(tcell.KeyCtrlF))
	if strings.Contains(screen(t, a, 120, 24), "files changed") {
		t.Fatal("Ctrl-F didn't hide pane")
	}
	// Editing a file back to its original drops it from the list.
	a.HandleKey(key(tcell.KeyCtrlF))
	a.Apply(change("a.rs", "x\n", "x\n"))
	if !strings.Contains(screen(t, a, 120, 24), "1 file changed +3 -0") {
		t.Fatal(screen(t, a, 120, 24))
	}
	a.Apply(uiClear{})
	if strings.Contains(screen(t, a, 120, 24), "changed") {
		t.Fatal("clear kept pane")
	}
}

func TestWheelOverFilesPaneScrollsThePaneNotTheTranscript(t *testing.T) {
	a := NewApp("s")
	var after strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&after, "line%d\n", i)
	}
	a.Apply(change("big.txt", "", after.String()))
	// The transcript shows the same diff, so only look at the pane (right 48 columns).
	pane := func() string {
		var sb strings.Builder
		for _, l := range strings.Split(screen(t, a, 120, 16), "\n") {
			r := []rune(l)
			if len(r) > 72 {
				sb.WriteString(string(r[72:]))
			}
			sb.WriteByte('\n')
		}
		return sb.String()
	}
	wheel := func(b tcell.ButtonMask, x int) { a.HandleMouse(tcell.NewEventMouse(x, 2, b, tcell.ModNone)) }
	// 12 rows of pane can't hold 30 added lines, so the diff is cut off at the bottom.
	if p := pane(); !strings.Contains(p, "1 + line1") || strings.Contains(p, "+ line30") {
		t.Fatal(p)
	}
	for i := 0; i < 10; i++ {
		wheel(tcell.WheelDown, 100)
	}
	if p := pane(); !strings.Contains(p, "+ line30") || strings.Contains(p, "1 + line1") {
		t.Fatal(p)
	}
	if a.scroll != 0 {
		t.Fatal("transcript must not move", a.scroll)
	}
	// Clamped at the end, so one notch back up moves immediately.
	wheel(tcell.WheelUp, 100)
	if strings.Contains(pane(), "+ line30") {
		t.Fatal("wheel up didn't move the pane")
	}
	// Over the transcript the wheel still scrolls the transcript.
	wheel(tcell.WheelUp, 10)
	if a.scroll != 3 {
		t.Fatal(a.scroll)
	}
	// A new change snaps the pane back to the top of that file's diff.
	a.Apply(change("big.txt", "", after.String()+"line31\n"))
	if !strings.Contains(pane(), "1 + line1") {
		t.Fatal(pane())
	}
}

func TestDraggingTheDividerResizesTheFilesPane(t *testing.T) {
	a := NewApp("s")
	a.Apply(change("a.txt", "", "x\n"))
	ev := func(b tcell.ButtonMask, x int) Action {
		return a.HandleMouse(tcell.NewEventMouse(x, 3, b, tcell.ModNone))
	}
	// 120 cols → default pane is 48 wide, divider at column 72.
	screen(t, a, 120, 16)
	if a.filesArea.x != 72 {
		t.Fatal(a.filesArea.x)
	}
	if ev(tcell.Button1, 72) != nil || a.sel != nil {
		t.Fatal("a click on the divider starts a resize, not a selection")
	}
	ev(tcell.Button1, 60)
	screen(t, a, 120, 16)
	if a.filesArea.x != 60 {
		t.Fatal(a.filesArea.x)
	}
	// The transcript keeps at least 40 columns however far it's dragged.
	ev(tcell.Button1, 5)
	screen(t, a, 120, 16)
	if a.filesArea.x != 40 {
		t.Fatal(a.filesArea.x)
	}
	if ev(tcell.ButtonNone, 5) != nil || a.draggingDivider {
		t.Fatal("release should end the drag without copying")
	}
	// After release, dragging elsewhere no longer moves the divider.
	ev(tcell.Button1, 90)
	screen(t, a, 120, 16)
	if a.filesArea.x != 40 {
		t.Fatal(a.filesArea.x)
	}
}
