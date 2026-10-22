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
