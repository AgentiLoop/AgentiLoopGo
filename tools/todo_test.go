package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

func TestRenderTodosMarksAndProgress(t *testing.T) {
	if got := renderTodos(nil); got != "todo list is empty" {
		t.Fatal(got)
	}
	items := []todoItem{{"a", "completed"}, {"b", "in_progress"}, {"c", "pending"}}
	if got, want := renderTodos(items), "todo list (1/3 done):\n[x] a\n[~] b\n[ ] c"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestValidateTodosRejectsBlankAndDoubleInProgress(t *testing.T) {
	if validateTodos([]todoItem{{"a", "in_progress"}, {"b", "pending"}}) != nil {
		t.Fatal("one in_progress is fine")
	}
	if validateTodos([]todoItem{{"  ", "pending"}}) == nil {
		t.Fatal("blank content accepted")
	}
	if validateTodos([]todoItem{{"a", "in_progress"}, {"b", "in_progress"}}) == nil {
		t.Fatal("two in_progress accepted")
	}
	if validateTodos([]todoItem{{"a", "bogus"}}) == nil {
		t.Fatal("unknown status accepted")
	}
}

// The only test touching the shared list, so it cannot race with the others.
func TestTodoWriteReplacesTheListAndRejectsBadInput(t *testing.T) {
	call := func(s string) (string, error) {
		return TodoWrite{}.Call(context.Background(), core.ToolContext{Cwd: t.TempDir()}, json.RawMessage(s))
	}
	ClearTodos()
	if CurrentTodos() != "" {
		t.Fatal("list should start empty")
	}
	out, err := call(`{"todos":[{"content":"x","status":"in_progress"},{"content":"y","status":"pending"}]}`)
	if err != nil || !strings.Contains(out, "[~] x") || !strings.Contains(out, "[ ] y") {
		t.Fatal(out, err)
	}
	if CurrentTodos() != out {
		t.Fatal(CurrentTodos())
	}
	if _, err := call(`{"todos":[{"content":"z","status":"completed"}]}`); err != nil {
		t.Fatal(err)
	}
	want := "todo list (1/1 done):\n[x] z"
	if CurrentTodos() != want {
		t.Fatal(CurrentTodos())
	}
	for _, bad := range []string{
		`{"todos":[{"content":"z","status":"bogus"}]}`,
		`{}`,
		`{"todos":[{"content":"z"}]}`,
		`{"todos":[{"status":"pending"}]}`,
		`{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"in_progress"}]}`,
	} {
		if _, err := call(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if CurrentTodos() != want {
		t.Fatal("failed calls changed the list:", CurrentTodos())
	}
	ClearTodos()
}
