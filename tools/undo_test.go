package tools

import (
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
