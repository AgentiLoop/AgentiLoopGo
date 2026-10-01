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

func TestInitWritesTemplateWithDetectedCommandsAndNeverOverwrites(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "go.mod"), "module x")
	write(t, filepath.Join(d, "Makefile"), "")
	path, err := InitInstructions(d)
	if err != nil || path != filepath.Join(d, "AGENTS.md") {
		t.Fatal(path, err)
	}
	b, _ := os.ReadFile(path)
	text := string(b)
	if !strings.Contains(text, "`go test ./...`") || !strings.Contains(text, "- `make`") || strings.Contains(text, "cargo") ||
		!strings.HasPrefix(text, "# "+filepath.Base(d)+"\n") {
		t.Fatal(text)
	}
	if _, err := InitInstructions(d); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	if got := LoadInstructions(d, ""); len(got) != 1 || got[0].Text != text {
		t.Fatalf("%#v", got)
	}
	if !strings.Contains(InitTemplate(t.TempDir()), "(add the commands") {
		t.Fatal("empty project template")
	}
}

func TestAtImportsInlineFilesAndLeaveUnresolvableLinesAlone(t *testing.T) {
	d := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(d, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/style.md", "use tabs\n@more.md\n")
	write("docs/more.md", "and snake_case\n")
	write("loop.md", "@AGENTS.md\n")
	write("AGENTS.md", "# Rules\n@docs/style.md\n@missing.md\n@loop.md\nemail me @ home\n```\n@docs/style.md\n```\n")
	got := LoadInstructions(d, "")
	if len(got) != 1 {
		t.Fatal(got)
	}
	text := got[0].Text
	for _, want := range []string{"use tabs\n[imported from", "and snake_case", "\n@missing.md\n", "email me @ home", "```\n@docs/style.md\n```", "\n@AGENTS.md"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Count(text, "use tabs") != 1 {
		t.Fatalf("fenced or cyclic import was expanded:\n%s", text)
	}
}
