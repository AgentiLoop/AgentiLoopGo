package core

import (
	"context"
	"encoding/json"
	"testing"
)

type askPolicy struct{}

func (askPolicy) Check(context.Context, string, bool, json.RawMessage) Permission { return Cancel }

func TestToolPatternsMatchExactPrefixAndAllAndSplitCommas(t *testing.T) {
	p := NewToolPatterns([]string{"bash", "mcp_*, edit_file", " "})
	for _, name := range []string{"bash", "mcp_Local_echo", "edit_file"} {
		if !p.Matches(name) {
			t.Fatal("should match", name)
		}
	}
	for _, name := range []string{"bash2", "write_file", "mcp"} {
		if p.Matches(name) {
			t.Fatal("should not match", name)
		}
	}
	if !NewToolPatterns([]string{"*"}).Matches("anything") || NewToolPatterns(nil).Matches("bash") {
		t.Fatal("star / empty")
	}
}

func TestRulesDenyBeatsAllowAndUnlistedToolsReachInnerPolicy(t *testing.T) {
	r := &Rules{Allow: NewToolPatterns([]string{"write_file", "bash"}), Deny: NewToolPatterns([]string{"bash", "web_*"}), Inner: askPolicy{}}
	for tool, want := range map[string]Permission{"bash": Deny, "web_fetch": Deny, "write_file": Allow, "edit_file": Cancel} {
		if got := r.Check(context.Background(), tool, true, nil); got != want {
			t.Fatalf("%s: got %v want %v", tool, got, want)
		}
	}
}
