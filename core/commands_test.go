package core

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandsLoadProjectOverUserAndSkipBuiltinsAndBadNames(t *testing.T) {
	d := t.TempDir()
	home, proj := filepath.Join(d, "home"), filepath.Join(d, "proj")
	cd := filepath.Join(proj, ".agentiloop", "commands")
	write(t, filepath.Join(home, "commands", "review.md"), "# User review\nlook at $ARGUMENTS")
	write(t, filepath.Join(home, "commands", "only-user.md"), "hello")
	write(t, filepath.Join(cd, "review.md"), "# Project review\nlook at $ARGUMENTS")
	write(t, filepath.Join(cd, "clear.md"), "builtin name")
	write(t, filepath.Join(cd, "bad name.md"), "x")
	write(t, filepath.Join(cd, "blank.md"), "  \n")
	write(t, filepath.Join(cd, "notes.txt"), "x")
	cmds := LoadCommands(proj, home)
	var names []string
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "only-user,review" {
		t.Fatal(names)
	}
	if cmds[1].Description() != "Project review" {
		t.Fatal(cmds[1].Description())
	}
}

func TestCommandsExpandReplacesOrAppendsArgumentsAndResolveOnlyMatchesCustom(t *testing.T) {
	d := t.TempDir()
	cd := filepath.Join(d, ".agentiloop", "commands")
	write(t, filepath.Join(cd, "fix.md"), "Fix this bug: $ARGUMENTS. Add a test.\n")
	write(t, filepath.Join(cd, "plain.md"), "Summarize the repo.\n")
	for line, want := range map[string]string{
		"/fix  the crash on exit ": "Fix this bug: the crash on exit. Add a test.",
		"/plain":                   "Summarize the repo.",
		"/plain in one line":       "Summarize the repo.\n\nin one line",
	} {
		if got, ok := ResolveCommand(line, d, ""); !ok || got != want {
			t.Fatalf("%q: got %q, %v", line, got, ok)
		}
	}
	write(t, filepath.Join(cd, "cmp.md"), "Compare $1 with $2, focus: $3.\nAll: $ARGUMENTS")
	write(t, filepath.Join(cd, "only1.md"), "Explain $1")
	for line, want := range map[string]string{
		"/cmp a.rs b.rs":       "Compare a.rs with b.rs, focus: .\nAll: a.rs b.rs",
		"/only1 main.rs extra": "Explain main.rs",
	} {
		if got, ok := ResolveCommand(line, d, ""); !ok || got != want {
			t.Fatalf("%q: got %q, %v", line, got, ok)
		}
	}
	for _, line := range []string{"/nothing", "not a command"} {
		if _, ok := ResolveCommand(line, d, ""); ok {
			t.Fatal("resolved", line)
		}
	}
	if !strings.Contains(CommandListing(LoadCommands(d, "")), "/fix") || !strings.Contains(CommandListing(nil), "no custom commands") {
		t.Fatal("listing")
	}
}
