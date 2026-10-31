package core

import (
	"encoding/json"
	"testing"
)

func TestToolUseInputIsCanonicalJSON(t *testing.T) {
	b := ToolUseBlock("1", "write_file", json.RawMessage(`{ "path": "a<b>.txt", "content": "x & y", "n": 1.50, "o": {"z": 1, "a": [2, 1]} }`))
	if got, want := string(b.Input), `{"content":"x & y","n":1.50,"o":{"a":[2,1],"z":1},"path":"a<b>.txt"}`; got != want {
		t.Fatalf("input = %s, want %s", got, want)
	}
	if got := ToolUseBlock("1", "x", json.RawMessage(`not json`)).Input; string(got) != "not json" {
		t.Fatalf("invalid JSON should be kept, got %s", got)
	}
}

func TestNthReplyCountsBackOverTextRepliesOnly(t *testing.T) {
	asst := func(blocks ...ContentBlock) Message { return Message{Role: RoleAssistant, Content: blocks} }
	call := ToolUseBlock("1", "bash", json.RawMessage(`{}`))
	history := []Message{
		UserText("hi"),
		asst(TextBlock("first")),
		UserText("more"),
		asst(TextBlock("second "), call, TextBlock("part")),
		ToolResults(nil),
		asst(call),
		asst(TextBlock("  ")),
	}
	for _, c := range []struct {
		n    int
		want string
		ok   bool
	}{{1, "second part", true}, {2, "first", true}, {3, "", false}, {0, "", false}} {
		if got, ok := NthReply(history, c.n); got != c.want || ok != c.ok {
			t.Fatalf("NthReply(%d) = %q, %v; want %q, %v", c.n, got, ok, c.want, c.ok)
		}
	}
	if _, ok := NthReply(nil, 1); ok {
		t.Fatal("empty history has no reply")
	}
}
