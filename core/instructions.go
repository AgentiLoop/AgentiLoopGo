package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// InstructionFileNames are tried in each directory, in order.
var InstructionFileNames = []string{"AGENTS.md", "CLAUDE.md"}

// MaxInstructionBytes is the longest instructions file sent whole; the rest is cut.
const MaxInstructionBytes = 32 * 1024

// Instructions is one loaded AGENTS.md-style file, appended to the system prompt so the
// agent follows the conventions of the project it is working in.
type Instructions struct {
	Path string
	Text string
}

// maxImportDepth is the deepest chain of @file imports followed.
const maxImportDepth = 3

// expandImports replaces each line that is just "@path" with that file's text, so AGENTS.md can pull in
// other docs. Paths are relative to the importing file (or absolute, or ~/…). Lines inside code fences,
// missing files, import cycles and chains deeper than maxImportDepth are left as they are.
func expandImports(text, dir string, depth int, stack []string) string {
	lines := strings.Split(text, "\n")
	fenced := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
		}
		rel, ok := strings.CutPrefix(t, "@")
		if !ok || fenced || rel == "" || strings.ContainsAny(rel, " \t\r") || depth >= maxImportDepth {
			continue
		}
		path := filepath.Join(dir, rel)
		if r, ok := strings.CutPrefix(rel, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				continue
			}
			path = filepath.Join(home, r)
		} else if filepath.IsAbs(rel) {
			path = rel
		}
		canon, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		cycle := false
		for _, s := range stack {
			cycle = cycle || s == canon
		}
		data, err := os.ReadFile(canon)
		if cycle || err != nil {
			continue
		}
		body := expandImports(strings.ToValidUTF8(string(data), "\uFFFD"), filepath.Dir(canon), depth+1, append(stack, canon))
		lines[i] = "[imported from " + path + "]\n" + strings.TrimRight(body, " \t\r\n")
	}
	return strings.Join(lines, "\n")
}

func readInstructionsIn(dir string) *Instructions {
	for _, name := range InstructionFileNames {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.ToValidUTF8(string(data), "\uFFFD")
		if strings.TrimSpace(text) == "" {
			continue
		}
		self, err := filepath.EvalSymlinks(path)
		if err != nil {
			self = path
		}
		text = expandImports(text, dir, 0, []string{self})
		if len(text) > MaxInstructionBytes {
			cut := MaxInstructionBytes
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut] + "\n…[truncated]"
		}
		return &Instructions{Path: path, Text: text}
	}
	return nil
}

// LoadInstructions loads the user-level file from home (the ~/.agentiloop folder, may be "") and the
// nearest project file: cwd and then its parents, stopping after the first directory that holds .git.
// User-level comes first so project instructions have the last word.
func LoadInstructions(cwd, home string) []Instructions {
	var found []Instructions
	if home != "" {
		if i := readInstructionsIn(home); i != nil {
			found = append(found, *i)
		}
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if i := readInstructionsIn(dir); i != nil {
			dup := false
			for _, f := range found {
				dup = dup || f.Path == i.Path
			}
			if !dup {
				found = append(found, *i)
			}
			break
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return found
}

// AppendInstructions is base followed by the loaded instructions; base unchanged when there are none.
func AppendInstructions(base string, found []Instructions) string {
	var b strings.Builder
	b.WriteString(base)
	for _, i := range found {
		fmt.Fprintf(&b, "\n\n# Instructions from %s\nThe user keeps these instructions for their projects. Follow them.\n\n%s",
			i.Path, strings.TrimRight(i.Text, " \t\r\n"))
	}
	return b.String()
}

// InitTemplate is a starter AGENTS.md for the project in dir, with build/test commands guessed from the files present.
func InitTemplate(dir string) string {
	name := filepath.Base(dir)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "this project"
	}
	has := func(f string) bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }
	var cmds []string
	if has("Cargo.toml") {
		cmds = append(cmds, "cargo build", "cargo test", "cargo clippy")
	}
	if has("go.mod") {
		cmds = append(cmds, "go build ./...", "go test ./...", "go vet ./...")
	}
	if has("package.json") {
		cmds = append(cmds, "npm install", "npm test")
	}
	if has("pyproject.toml") || has("requirements.txt") {
		cmds = append(cmds, "python -m pytest")
	}
	if has("Makefile") {
		cmds = append(cmds, "make")
	}
	commands := "- (add the commands to build, test and lint this project)"
	if len(cmds) > 0 {
		lines := make([]string, len(cmds))
		for i, c := range cmds {
			lines[i] = "- `" + c + "`"
		}
		commands = strings.Join(lines, "\n")
	}
	return "# " + name + "\n\nInstructions for AgentiLoop and other coding agents working in this repository.\n\n" +
		"## Commands\n\n" + commands + "\n\n" +
		"## Conventions\n\n- (code style, naming, where new code goes)\n\n" +
		"## Do not\n\n- (things to avoid: generated files, secrets, risky commands)\n"
}

// InitInstructions creates AGENTS.md in dir from InitTemplate. It refuses to overwrite an existing file.
func InitInstructions(dir string) (string, error) {
	path := filepath.Join(dir, InstructionFileNames[0])
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", path)
	}
	if err := os.WriteFile(path, []byte(InitTemplate(dir)), 0o644); err != nil {
		return "", fmt.Errorf("could not write %s: %v", path, err)
	}
	return path, nil
}
