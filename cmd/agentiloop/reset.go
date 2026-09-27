package main

// agentiloop --reset: put the machine back to "brand new" so the first-run
// wizard can be tested again. Removes ~/.agentiloop, the marked block the
// wizard wrote to the shell profile, and Keychain items it created. Hand-written
// export lines are never deleted, only commented out (with permission).
// Mirrors reset.rs in the Rust AgentiLoop CLI.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// credentialVars is every environment variable the providers read for credentials.
var credentialVars = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_OAUTH_TOKEN",
	"OPENAI_API_KEY",
	"OPENAI_BASE_URL",
	"OMLX_BASE_URL",
	"OMLX_PORT",
	"OMLX_API_KEY",
}

const (
	blockStart    = "# >>> agentiloop >>>"
	blockEnd      = "# <<< agentiloop <<<"
	commentPrefix = "# agentiloop-reset: "
)

// candidateProfiles returns the shell profiles worth looking at:
// AGENTILOOP_SHELL_PROFILE alone when set (so tests never touch the real ones),
// else the usual zsh/bash/fish files that exist.
func candidateProfiles() []string {
	if p, ok := os.LookupEnv("AGENTILOOP_SHELL_PROFILE"); ok {
		return []string{p}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range []string{".zshrc", ".zprofile", ".bashrc", ".bash_profile", ".profile", ".config/fish/config.fish"} {
		p := filepath.Join(home, f)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// isStray reports whether a profile line sets a credential variable or the ~/.local/bin PATH entry.
func isStray(line string) bool {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, "#") {
		return false
	}
	sets := func(v string) bool {
		for _, pre := range []string{v + "=", "export " + v + "=", "set -gx " + v + " ", "set -x " + v + " ", "$env:" + v + " ", "$env:" + v + "="} {
			if strings.HasPrefix(l, pre) {
				return true
			}
		}
		return false
	}
	for _, v := range credentialVars {
		if sets(v) {
			return true
		}
	}
	return sets("PATH") && strings.Contains(l, ".local/bin")
}

type strayLine struct {
	n    int // 1-based
	text string
}

// strayLines lists hand-written lines outside the marked block.
func strayLines(text string) []strayLine {
	var out []strayLine
	inside := false
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		switch strings.TrimSpace(line) {
		case blockStart:
			inside = true
		case blockEnd:
			inside = false
		default:
			if !inside && isStray(line) {
				out = append(out, strayLine{i + 1, line})
			}
		}
	}
	return out
}

func hasBlock(text string) bool {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == blockStart {
			return true
		}
	}
	return false
}

// removeBlock drops everything from blockStart through blockEnd (inclusive).
func removeBlock(text string) string {
	var b strings.Builder
	inside := false
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		switch strings.TrimSpace(line) {
		case blockStart:
			inside = true
		case blockEnd:
			inside = false
		default:
			if !inside {
				b.WriteString(line + "\n")
			}
		}
	}
	return b.String()
}

// commentOut prefixes the given 1-based lines with "# agentiloop-reset: " so they can be restored by hand.
func commentOut(text string, lines []int) string {
	var b strings.Builder
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if slices.Contains(lines, i+1) {
			b.WriteString(commentPrefix)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func unsetHint() string {
	return "Variables already exported in this terminal stay until you open a new one, or run:\n  unset " +
		strings.Join(credentialVars, " ") + "\nthen run `agentiloop` to see the first-run wizard."
}

func deleteKeychainItem(service string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	cmd := exec.Command("security", "delete-generic-password", "-a", os.Getenv("USER"), "-s", service)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("security delete-generic-password -s %s failed", service)
	}
	return nil
}

// runReset is agentiloop --reset. in/out are stdin/stdout (parameters for tests).
func runReset(yes bool, in io.Reader, out io.Writer) error {
	saved := loadSettings()
	home := agentiloopHome()
	homeExists := false
	if home != "" {
		_, err := os.Stat(home)
		homeExists = err == nil
	}
	homeShown := home
	if homeShown == "" {
		homeShown = "~/.agentiloop"
	}
	rd := bufio.NewReader(in)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		line, err := rd.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("aborted")
		}
		return strings.TrimSpace(line), nil
	}

	// Profiles with a marked block: the one settings.json points at, plus any other candidate.
	profiles := candidateProfiles()
	if p := saved.Setup.Profile; p != nil && !slices.Contains(profiles, *p) {
		if st, err := os.Stat(*p); err == nil && !st.IsDir() {
			profiles = append(profiles, *p)
		}
	}
	var blocks []string
	type strays struct {
		path  string
		lines []strayLine
	}
	var found []strays
	for _, p := range profiles {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if hasBlock(string(data)) {
			blocks = append(blocks, p)
		}
		if s := strayLines(string(data)); len(s) > 0 {
			found = append(found, strays{p, s})
		}
	}
	keychain := saved.Setup.Keychain

	if !homeExists && len(blocks) == 0 && len(found) == 0 && len(keychain) == 0 {
		fmt.Fprintf(out, "Nothing to reset: no %s and no agentiloop lines in your shell profile.\n%s\n", homeShown, unsetHint())
		return nil
	}

	fmt.Fprintln(out, "This will:")
	if homeExists {
		fmt.Fprintf(out, "  • delete %s (settings.json, env, history.txt, sessions/, mcp.json — mcp.json is yours, back it up first)\n", home)
	}
	for _, p := range blocks {
		fmt.Fprintf(out, "  • remove the `%s` … `%s` block from %s\n", blockStart, blockEnd, p)
	}
	for _, k := range keychain {
		fmt.Fprintf(out, "  • delete the macOS Keychain item `%s`\n", k)
	}
	for _, f := range found {
		fmt.Fprintf(out, "  • found hand-written lines in %s (not deleted, see below):\n", f.path)
		for _, l := range f.lines {
			fmt.Fprintf(out, "      %s:%d: %s\n", f.path, l.n, strings.TrimSpace(l.text))
		}
	}

	if !yes {
		ans, err := ask("\nType `reset` to continue: ")
		if err != nil {
			return err
		}
		if ans != "reset" {
			fmt.Fprintln(out, "Cancelled; nothing changed.")
			return nil
		}
	}
	comment := false
	if len(found) > 0 {
		comment = yes
		if !yes {
			ans, err := ask("Comment out the hand-written lines above (prefix `# agentiloop-reset: `)? [y/N] ")
			if err != nil {
				return err
			}
			comment = strings.EqualFold(ans, "y")
		}
	}

	for _, p := range blocks {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(removeBlock(string(data))), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", p, err)
		}
		fmt.Fprintf(out, "removed block from %s\n", p)
	}
	if comment {
		for _, f := range found {
			// Line numbers were taken before the block was removed; recompute on the current text.
			data, err := os.ReadFile(f.path)
			if err != nil {
				return err
			}
			var nums []int
			for _, l := range strayLines(string(data)) {
				nums = append(nums, l.n)
			}
			if err := os.WriteFile(f.path, []byte(commentOut(string(data), nums)), 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", f.path, err)
			}
			fmt.Fprintf(out, "commented out %d line(s) in %s\n", len(f.lines), f.path)
		}
	}
	for _, k := range keychain {
		if err := deleteKeychainItem(k); err != nil {
			fmt.Fprintf(out, "warning: %v\n", err)
		} else {
			fmt.Fprintf(out, "deleted Keychain item %s\n", k)
		}
	}
	if homeExists {
		if err := os.RemoveAll(home); err != nil {
			return fmt.Errorf("removing %s: %w", home, err)
		}
		fmt.Fprintf(out, "deleted %s\n", home)
	}

	fmt.Fprintf(out, "\nDone. %s\n", unsetHint())
	return nil
}
