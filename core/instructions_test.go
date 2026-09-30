package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstructionsFindNearestFileUpToRepoRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "repo", ".git", "HEAD"), "x")
	if err := os.MkdirAll(filepath.Join(root, "repo", "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "AGENTS.md"), "outside the repo")
	write(t, filepath.Join(root, "repo", "AGENTS.md"), "use tabs")
	got := LoadInstructions(filepath.Join(root, "repo", "a", "b"), "")
	if len(got) != 1 || got[0].Text != "use tabs" {
		t.Fatalf("%#v", got)
	}
	// A repo without its own file does not pick up one from above the repo.
	os.Remove(filepath.Join(root, "repo", "AGENTS.md"))
	if got := LoadInstructions(filepath.Join(root, "repo", "a", "b"), ""); len(got) != 0 {
		t.Fatalf("%#v", got)
	}
}

func TestInstructionsAgentsBeatsClaudeAndBlankIgnored(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "CLAUDE.md"), "from claude")
	if got := LoadInstructions(d, ""); got[0].Text != "from claude" {
		t.Fatalf("%#v", got)
	}
	write(t, filepath.Join(d, "AGENTS.md"), "from agents")
	if got := LoadInstructions(d, ""); got[0].Text != "from agents" {
		t.Fatalf("%#v", got)
	}
	write(t, filepath.Join(d, "AGENTS.md"), "  \n")
	if got := LoadInstructions(d, ""); got[0].Text != "from claude" {
		t.Fatalf("%#v", got)
	}
}

func TestInstructionsUserFirstAndLongFilesCut(t *testing.T) {
	proj, home := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "AGENTS.md"), "global")
	write(t, filepath.Join(proj, "AGENTS.md"), strings.Repeat("é", MaxInstructionBytes))
	got := LoadInstructions(proj, home)
	if len(got) != 2 || got[0].Text != "global" || !strings.HasSuffix(got[1].Text, "…[truncated]") ||
		len(got[1].Text) > MaxInstructionBytes+len("\n…[truncated]") {
		t.Fatalf("%d", len(got))
	}
	p := AppendInstructions("BASE", got)
	if !strings.HasPrefix(p, "BASE\n\n# Instructions from ") || strings.Index(p, "global") > strings.Index(p, "éé") {
		t.Fatal(p[:80])
	}
	if AppendInstructions("BASE", nil) != "BASE" {
		t.Fatal("base changed")
	}
}
