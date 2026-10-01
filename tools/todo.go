package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

// todo_write: the model's task checklist for multi-step work. Each call replaces the whole list;
// /todos shows it to the user. The list lives in memory for the process (cleared by /clear).

type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

var (
	todoMu   sync.Mutex
	todoList []todoItem
)

func renderTodos(items []todoItem) string {
	if len(items) == 0 {
		return "todo list is empty"
	}
	done := 0
	for _, i := range items {
		if i.Status == "completed" {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "todo list (%d/%d done):", done, len(items))
	for _, i := range items {
		mark := "[ ]"
		switch i.Status {
		case "in_progress":
			mark = "[~]"
		case "completed":
			mark = "[x]"
		}
		fmt.Fprintf(&b, "\n%s %s", mark, i.Content)
	}
	return b.String()
}

// validateTodos checks a proposed list: known statuses, non-empty text and at most one item in progress.
func validateTodos(items []todoItem) error {
	inProgress := 0
	for _, i := range items {
		switch i.Status {
		case "pending", "completed":
		case "in_progress":
			inProgress++
		default:
			return core.InvalidInput("unknown status %q (use pending, in_progress or completed)", i.Status)
		}
		if strings.TrimSpace(i.Content) == "" {
			return core.InvalidInput("every todo needs non-empty `content`")
		}
	}
	if inProgress > 1 {
		return core.InvalidInput("only one todo may be in_progress at a time")
	}
	return nil
}

// CurrentTodos is the current list as text for /todos; "" when it is empty.
func CurrentTodos() string {
	todoMu.Lock()
	defer todoMu.Unlock()
	if len(todoList) == 0 {
		return ""
	}
	return renderTodos(todoList)
}

// ClearTodos forgets the list (used by /clear).
func ClearTodos() {
	todoMu.Lock()
	todoList = nil
	todoMu.Unlock()
}

type TodoWrite struct{}

func (TodoWrite) Name() string { return "todo_write" }
func (TodoWrite) Description() string {
	return "Keep a checklist for multi-step tasks. Each call REPLACES the whole list, so send every item every time. " +
		"Mark an item in_progress before starting it (at most one at a time) and completed as soon as it is done. " +
		"Use it for work with three or more steps; skip it for simple requests."
}
func (TodoWrite) InputSchema() any {
	return schema(map[string]any{
		"todos": map[string]any{"type": "array", "description": "The complete todo list", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "What needs doing, in the imperative"},
				"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
			},
			"required": []string{"content", "status"},
		}},
	}, "todos")
}
func (TodoWrite) IsMutating() bool { return false }
func (TodoWrite) Call(_ context.Context, _ core.ToolContext, input json.RawMessage) (string, error) {
	var a struct {
		Todos []struct {
			Content *string `json:"content"`
			Status  *string `json:"status"`
		} `json:"todos"`
	}
	if err := parse(input, &a, "todos"); err != nil {
		return "", err
	}
	items := make([]todoItem, 0, len(a.Todos))
	for _, t := range a.Todos {
		if t.Content == nil {
			return "", core.InvalidInput("missing field `content`")
		}
		if t.Status == nil {
			return "", core.InvalidInput("missing field `status`")
		}
		items = append(items, todoItem{*t.Content, *t.Status})
	}
	if err := validateTodos(items); err != nil {
		return "", err
	}
	text := renderTodos(items)
	todoMu.Lock()
	todoList = items
	todoMu.Unlock()
	return text, nil
}
