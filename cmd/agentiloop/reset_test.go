package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testProfile = "export PATH=\"$HOME/.local/bin:$PATH\"\n# export OPENAI_API_KEY=old\nalias ll='ls -l'\n# >>> agentiloop >>>\nexport ANTHROPIC_API_KEY=\"sk-ant-1\"\n# <<< agentiloop <<<\nexport OPENAI_BASE_URL=http://localhost:11434/v1\nset -gx OMLX_PORT 7777\n"

func TestStrayLinesSkipCommentsAndBlock(t *testing.T) {
	var nums []int
	for _, l := range strayLines(testProfile) {
		nums = append(nums, l.n)
	}
	if len(nums) != 3 || nums[0] != 1 || nums[1] != 7 || nums[2] != 8 {
		t.Fatal(nums)
	}
}

func TestRemoveBlockKeepsEverythingElse(t *testing.T) {
	out := removeBlock(testProfile)
	if strings.Contains(out, "agentiloop") || strings.Contains(out, "sk-ant-1") || strings.Count(out, "\n") != 5 || hasBlock(out) || !hasBlock(testProfile) {
		t.Fatalf("%q", out)
	}
}

func TestCRLFProfilesStayCRLF(t *testing.T) {
	crlf := "a\r\n# >>> agentiloop >>>\r\nexport OPENAI_API_KEY=x\r\n# <<< agentiloop <<<\r\nb\r\n"
	if got := matchLineEndings(crlf, removeBlock(crlf)); got != "a\r\nb\r\n" {
		t.Fatalf("%q", got)
	}
	if got := matchLineEndings("a\nb\n", "a\nb\n"); got != "a\nb\n" {
		t.Fatalf("%q", got)
	}
}

func TestCommentOut(t *testing.T) {
	out := commentOut("a\nexport OPENAI_API_KEY=x\nb\n", []int{2})
	if out != "a\n# agentiloop-reset: export OPENAI_API_KEY=x\nb\n" || len(strayLines(out)) != 0 {
		t.Fatalf("%q", out)
	}
}

func TestRunResetEndToEnd(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	profile := filepath.Join(t.TempDir(), "zshrc")
	t.Setenv("AGENTILOOP_HOME", home)
	t.Setenv("AGENTILOOP_SHELL_PROFILE", profile)
	os.MkdirAll(filepath.Join(home, "sessions"), 0o755)
	os.WriteFile(settingsPath(), []byte(`{"setup":{"profile":"`+profile+`","keychain":[]}}`), 0o644)
	os.WriteFile(profile, []byte(testProfile), 0o644)

	var out strings.Builder
	if err := runReset(true, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("home still exists")
	}
	got, _ := os.ReadFile(profile)
	if hasBlock(string(got)) || !strings.Contains(string(got), "# agentiloop-reset: export OPENAI_BASE_URL") || !strings.Contains(string(got), "alias ll") {
		t.Fatalf("%s", got)
	}
	unsetHint := "unset ANTHROPIC_API_KEY"
	if runtime.GOOS == "windows" {
		unsetHint = "Remove-Item Env:ANTHROPIC_API_KEY"
	}
	if !strings.Contains(out.String(), unsetHint) {
		t.Fatal(out.String())
	}

	out.Reset()
	if err := runReset(true, strings.NewReader(""), &out); err != nil || !strings.HasPrefix(out.String(), "Nothing to reset") {
		t.Fatal(err, out.String())
	}
}
