package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Undo journal for file changes made by write_file, edit_file and apply_patch.
//
// Before a tool touches a file it records the file's previous content (or that it did not exist).
// Changes are grouped per turn (one user prompt); /undo restores the most recent turn that changed
// anything. Changes made by shell commands (bash) are not tracked.

// maxUndoTurns is how many turns are kept; older ones are forgotten.
const maxUndoTurns = 20

type undoFile struct {
	path    string
	before  []byte
	existed bool
}

type undoTurn struct{ files []undoFile }

// UndoJournal holds per-turn file snapshots. The package-level functions use one shared instance.
type UndoJournal struct {
	mu    sync.Mutex
	turns []*undoTurn
}

var journal = &UndoJournal{}

// BeginTurn starts a new turn; call once per user prompt. A previous turn that recorded nothing is reused.
func (j *UndoJournal) BeginTurn() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n := len(j.turns); n > 0 && len(j.turns[n-1].files) == 0 {
		return
	}
	j.turns = append(j.turns, &undoTurn{})
	if len(j.turns) > maxUndoTurns {
		j.turns = j.turns[1:]
	}
}

// Record remembers path's current state before it is changed.
func (j *UndoJournal) Record(path string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.turns) == 0 {
		j.turns = append(j.turns, &undoTurn{})
	}
	t := j.turns[len(j.turns)-1]
	for _, f := range t.files {
		if f.path == path {
			return
		}
	}
	data, err := os.ReadFile(path)
	t.files = append(t.files, undoFile{path: path, before: data, existed: err == nil})
}

// Undo restores every file changed in the latest turn that changed anything, newest change first.
// It returns one line per file, or nil when there is nothing to undo.
func (j *UndoJournal) Undo() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	for len(j.turns) > 0 {
		t := j.turns[len(j.turns)-1]
		j.turns = j.turns[:len(j.turns)-1]
		if len(t.files) == 0 {
			continue
		}
		var lines []string
		for i := len(t.files) - 1; i >= 0; i-- {
			f := t.files[i]
			if f.existed {
				_ = os.MkdirAll(filepath.Dir(f.path), 0o755)
				if err := os.WriteFile(f.path, f.before, 0o644); err != nil {
					lines = append(lines, fmt.Sprintf("could not restore %s: %v", f.path, err))
				} else {
					lines = append(lines, "restored "+f.path)
				}
			} else if err := os.Remove(f.path); err == nil {
				lines = append(lines, fmt.Sprintf("removed %s (it did not exist before)", f.path))
			} else if !errors.Is(err, fs.ErrNotExist) {
				lines = append(lines, fmt.Sprintf("could not remove %s: %v", f.path, err))
			}
		}
		return lines
	}
	return nil
}

// Clear forgets everything (used by /clear).
func (j *UndoJournal) Clear() {
	j.mu.Lock()
	j.turns = nil
	j.mu.Unlock()
}

// BeginUndoTurn starts a new turn on the shared journal; call once per user prompt.
func BeginUndoTurn() { journal.BeginTurn() }

// RecordUndo remembers path's current state on the shared journal before it is changed.
func RecordUndo(path string) { journal.Record(path) }

// UndoLast restores the latest turn that changed files; nil when there is nothing to undo.
func UndoLast() []string { return journal.Undo() }

// ClearUndo forgets the shared journal.
func ClearUndo() { journal.Clear() }
