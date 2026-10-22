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
// anything. Interactive prompts also remember where they start in the conversation, so /rewind
// can restore their files and cut the conversation back to before one of them. Changes made by
// shell commands (bash) are not tracked.

// maxUndoTurns is how many turns are kept; older ones are forgotten.
const maxUndoTurns = 50

type undoFile struct {
	path    string
	before  []byte
	existed bool
}

// undoMark is where an interactive prompt starts: the agent's history epoch (bumped when the
// history is replaced by /clear, /resume or compaction), its index in the history, and the prompt.
type undoMark struct {
	epoch  uint64
	index  int
	prompt string
}

type undoTurn struct {
	files []undoFile
	mark  *undoMark
}

// RewindPrompt is a prompt /rewind can go back to: its history index, text and how many files it changed.
type RewindPrompt struct {
	Index int
	Text  string
	Files int
}

// UndoJournal holds per-turn file snapshots. The package-level functions use one shared instance.
type UndoJournal struct {
	mu    sync.Mutex
	turns []*undoTurn
}

var journal = &UndoJournal{}

func (j *UndoJournal) push(t *undoTurn) {
	j.turns = append(j.turns, t)
	if len(j.turns) > maxUndoTurns {
		j.turns = j.turns[1:]
	}
}

// BeginTurn starts a new turn; call once per user prompt. A previous turn that recorded nothing is reused.
func (j *UndoJournal) BeginTurn() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n := len(j.turns); n > 0 && len(j.turns[n-1].files) == 0 && j.turns[n-1].mark == nil {
		return
	}
	j.push(&undoTurn{})
}

// BeginPrompt starts a new turn for an interactive prompt that /rewind can go back to: index is
// the agent's history length before the prompt and epoch the agent's history epoch.
func (j *UndoJournal) BeginPrompt(epoch uint64, index int, prompt string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.push(&undoTurn{mark: &undoMark{epoch: epoch, index: index, prompt: prompt}})
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
// It returns one line per file, or nil when there is nothing to undo. The turn itself stays (with
// no files), so its prompt can still be rewound to.
func (j *UndoJournal) Undo() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := len(j.turns) - 1; i >= 0; i-- {
		if t := j.turns[i]; len(t.files) > 0 {
			files := t.files
			t.files = nil
			return restoreFiles(files)
		}
	}
	return nil
}

// Prompts returns the prompts of history epoch that /rewind can go back to, oldest first.
func (j *UndoJournal) Prompts(epoch uint64) []RewindPrompt {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []RewindPrompt
	for _, t := range j.turns {
		if t.mark != nil && t.mark.epoch == epoch {
			out = append(out, RewindPrompt{Index: t.mark.index, Text: t.mark.prompt, Files: len(t.files)})
		}
	}
	return out
}

// Rewind restores the files changed by the prompt at history index (in epoch) and every later one,
// newest first, and forgets those prompts. It returns one line per file.
func (j *UndoJournal) Rewind(epoch uint64, index int) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	start := -1
	for i, t := range j.turns {
		if t.mark != nil && t.mark.epoch == epoch && t.mark.index == index {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var lines []string
	for i := len(j.turns) - 1; i >= start; i-- {
		lines = append(lines, restoreFiles(j.turns[i].files)...)
	}
	j.turns = j.turns[:start]
	return lines
}

// restoreFiles puts each file back the way it was before the turn, newest change first.
func restoreFiles(files []undoFile) []string {
	var lines []string
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
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

// Clear forgets everything (used by /clear).
func (j *UndoJournal) Clear() {
	j.mu.Lock()
	j.turns = nil
	j.mu.Unlock()
}

// BeginUndoTurn starts a new turn on the shared journal; call once per user prompt.
func BeginUndoTurn() { journal.BeginTurn() }

// BeginUndoPrompt starts a turn on the shared journal for an interactive prompt /rewind can go back to.
func BeginUndoPrompt(epoch uint64, index int, prompt string) {
	journal.BeginPrompt(epoch, index, prompt)
}

// RecordUndo remembers path's current state on the shared journal before it is changed.
func RecordUndo(path string) { journal.Record(path) }

// UndoLast restores the latest turn that changed files; nil when there is nothing to undo.
func UndoLast() []string { return journal.Undo() }

// RewindPrompts lists the shared journal's prompts of history epoch, oldest first.
func RewindPrompts(epoch uint64) []RewindPrompt { return journal.Prompts(epoch) }

// Rewind restores the files of the prompt at history index (in epoch) and every later one.
func Rewind(epoch uint64, index int) []string { return journal.Rewind(epoch, index) }

// ClearUndo forgets the shared journal.
func ClearUndo() { journal.Clear() }
