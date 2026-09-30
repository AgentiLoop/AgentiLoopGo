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
