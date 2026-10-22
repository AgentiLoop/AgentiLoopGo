package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJournalRestoresModifiedCreatedAndDeletedFilesPerTurn(t *testing.T) {
	d := t.TempDir()
	a, b, c := filepath.Join(d, "a.txt"), filepath.Join(d, "sub", "b.txt"), filepath.Join(d, "c.txt")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(a, []byte("orig"), 0o644))
	must(os.WriteFile(c, []byte("keep me"), 0o644))
	j := &UndoJournal{}
	if j.Undo() != nil {
		t.Fatal("empty journal should have nothing to undo")
	}

	// Turn 1: modify a twice (only the first snapshot counts), create b.
	j.BeginTurn()
	j.Record(a)
	must(os.WriteFile(a, []byte("v1"), 0o644))
	j.Record(a)
	must(os.WriteFile(a, []byte("v2"), 0o644))
	j.Record(b)
	must(os.MkdirAll(filepath.Dir(b), 0o755))
	must(os.WriteFile(b, []byte("new"), 0o644))

	// Turn 2: delete c. A turn that records nothing is skipped by undo.
	j.BeginTurn()
	j.Record(c)
	must(os.Remove(c))
	j.BeginTurn()
	j.BeginTurn()

	lines := j.Undo()
	if len(lines) != 1 || lines[0] != "restored "+c {
		t.Fatalf("%v", lines)
	}
	if got, _ := os.ReadFile(c); string(got) != "keep me" {
		t.Fatalf("%q", got)
	}
	if got, _ := os.ReadFile(a); string(got) != "v2" {
		t.Fatalf("%q", got)
	}

	lines = j.Undo()
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "removed ") || !strings.HasPrefix(lines[1], "restored ") {
		t.Fatalf("%v", lines)
	}
	if got, _ := os.ReadFile(a); string(got) != "orig" {
		t.Fatalf("%q", got)
	}
	if _, err := os.Stat(b); err == nil {
		t.Fatal("b should be removed")
	}
	if j.Undo() != nil {
		t.Fatal("nothing left")
	}
}

// Uses the shared journal, so this is the only test that does.
func TestFileToolsAreUndoablePerTurn(t *testing.T) {
	d := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(d, "a.txt"), []byte("one\ntwo\n"), 0o644))
	must(os.WriteFile(filepath.Join(d, "gone.txt"), []byte("bye\n"), 0o644))
	ClearUndo()
	defer ClearUndo()

	BeginUndoTurn()
	_, err := call(t, WriteFile{}, d, `{"path":"new/n.txt","content":"hi"}`)
	must(err)
	_, err = call(t, EditFile{}, d, `{"path":"a.txt","old_string":"two","new_string":"2"}`)
	must(err)

	BeginUndoTurn()
	patch := "*** Begin Patch\n*** Update File: a.txt\n@@\n one\n-2\n+TWO\n*** Delete File: gone.txt\n*** End Patch"
	_, err = call(t, ApplyPatch{}, d, `{"patch":`+quote(patch)+`}`)
	must(err)
	if read(t, d, "a.txt") != "one\nTWO\n" {
		t.Fatal(read(t, d, "a.txt"))
	}

	if lines := UndoLast(); len(lines) != 2 {
		t.Fatalf("%v", lines)
	}
	if read(t, d, "a.txt") != "one\n2\n" || read(t, d, "gone.txt") != "bye\n" {
		t.Fatal("patch turn not undone")
	}
	if UndoLast() == nil {
		t.Fatal("first turn missing")
	}
	if read(t, d, "a.txt") != "one\ntwo\n" {
		t.Fatal("first turn not undone")
	}
	if _, err := os.Stat(filepath.Join(d, "new", "n.txt")); err == nil {
		t.Fatal("created file should be removed")
	}
	if UndoLast() != nil {
		t.Fatal("nothing left")
	}
}

func TestRewindRestoresThePromptAndEveryLaterOneWithinItsEpoch(t *testing.T) {
	d := t.TempDir()
	a, b := filepath.Join(d, "a.txt"), filepath.Join(d, "b.txt")
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "orig")
	j := &UndoJournal{}

	j.BeginPrompt(0, 0, "old conversation")
	j.Record(a)
	write(a, "old")
	// Compaction: the history is replaced, so a new epoch starts.
	j.BeginPrompt(1, 2, "edit a")
	j.Record(a)
	write(a, "v1")
	j.BeginPrompt(1, 6, "just a question")
	j.BeginPrompt(1, 8, "create b")
	j.Record(b)
	write(b, "new")

	want := []RewindPrompt{{2, "edit a", 1}, {6, "just a question", 0}, {8, "create b", 1}}
	if got := j.Prompts(1); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("prompts = %v, want %v", got, want)
	}
	if len(j.Prompts(0)) != 1 {
		t.Fatal("epoch 0 should keep its prompt")
	}

	// /undo empties the newest turn but keeps its prompt for /rewind.
	if lines := j.Undo(); len(lines) != 1 {
		t.Fatalf("undo = %v", lines)
	}
	if _, err := os.Stat(b); err == nil || len(j.Prompts(1)) != 3 {
		t.Fatal("undo should remove b and keep the prompts")
	}

	if lines := j.Rewind(1, 99); lines != nil {
		t.Fatalf("unknown index should be a no-op, got %v", lines)
	}
	if lines := j.Rewind(1, 2); fmt.Sprint(lines) != fmt.Sprint([]string{"restored " + a}) {
		t.Fatalf("rewind = %v", lines)
	}
	if got, _ := os.ReadFile(a); string(got) != "old" {
		t.Fatalf("a = %q, the earlier epoch's change should stay", got)
	}
	if len(j.Prompts(1)) != 0 || len(j.Prompts(0)) != 1 {
		t.Fatal("rewind should forget only the rewound prompts")
	}
}
