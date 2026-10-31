package core

import (
	"fmt"
	"strings"
	"testing"
)

func TestHistoryQuery(t *testing.T) {
	cases := []struct {
		line, want string
		ok         bool
	}{
		{"/history", "", true},
		{"  /history   fix the  build ", "fix the  build", true},
		{"/historyx", "", false},
		{"history", "", false},
		{"/help", "", false},
	}
	for _, c := range cases {
		if got, ok := HistoryQuery(c.line); got != c.want || ok != c.ok {
			t.Errorf("HistoryQuery(%q) = %q, %v; want %q, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestHistoryListingEmptyAndNoMatch(t *testing.T) {
	if got := HistoryListing(nil, ""); got != "no saved prompts yet" {
		t.Errorf("got %q", got)
	}
	if got := HistoryListing([]string{"/history"}, ""); got != "no saved prompts yet" {
		t.Errorf("got %q", got)
	}
	if got := HistoryListing([]string{"hello"}, "bye"); got != `no saved prompt contains "bye"` {
		t.Errorf("got %q", got)
	}
}

func TestHistoryListingFilters(t *testing.T) {
	entries := []string{"Fix the build", "/history fix", "write tests", "fix lint\nand more"}
	if got, want := HistoryListing(entries, "FIX"), "2 saved prompt(s) containing \"FIX\", oldest first (↑ recalls them):\n  Fix the build\n  fix lint…"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := HistoryListing(entries, ""), "3 saved prompt(s), oldest first (↑ recalls them):\n  Fix the build\n  write tests\n  fix lint…"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHistoryListingNewestAndLong(t *testing.T) {
	var entries []string
	for i := 1; i <= 25; i++ {
		entries = append(entries, fmt.Sprintf("prompt %d", i))
	}
	out := HistoryListing(entries, "")
	if !strings.HasPrefix(out, "last 20 of 25 saved prompt(s), oldest first") || !strings.Contains(out, "\n  prompt 6\n") ||
		!strings.HasSuffix(out, "\n  prompt 25") || strings.Contains(out, "\n  prompt 5\n") {
		t.Errorf("got %q", out)
	}
	out = HistoryListing([]string{strings.Repeat("é", 150)}, "")
	if !strings.HasSuffix(out, "\n  "+strings.Repeat("é", 100)+"…") {
		t.Errorf("got %q", out)
	}
}
