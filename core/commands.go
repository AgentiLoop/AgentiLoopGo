package core

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Custom slash commands: every *.md file in <project>/.agentiloop/commands and in the user's
// ~/.agentiloop/commands becomes /<file name>. Typing it sends the file's text as the prompt,
// with $ARGUMENTS replaced by whatever follows the command (or appended when the file has no
// placeholder). A project file wins over a user file of the same name; built-in commands always win.

// BuiltinCommands are the commands built into the program; a file with one of these names is ignored.
var BuiltinCommands = []string{
	"clear", "usage", "export", "init", "diff", "todos", "undo", "model", "compact", "sessions", "resume", "mcp", "commands",
	"help", "setup", "exit", "quit",
}

type CustomCommand struct {
	Name     string
	Path     string
	Template string
}

// Description is the first non-blank line of the file, without leading '#', at most 70 characters.
func (c CustomCommand) Description() string {
	line := ""
	for _, l := range strings.Split(c.Template, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#")); l != "" {
			line = l
			break
		}
	}
	r := []rune(line)
	if len(r) > 70 {
		return string(r[:70]) + "…"
	}
	return line
}

// Expand is the prompt to send for "/name args". $ARGUMENTS is the whole text after the command and
// $1..$9 are its words; without any placeholder the text is appended after a blank line.
func (c CustomCommand) Expand(args string) string {
	words := strings.Fields(args)
	positional := false
	for n := 1; n <= 9; n++ {
		positional = positional || strings.Contains(c.Template, "$"+strconv.Itoa(n))
	}
	if !strings.Contains(c.Template, "$ARGUMENTS") && !positional {
		if args == "" {
			return strings.TrimSpace(c.Template)
		}
		return strings.TrimRight(c.Template, " \t\r\n") + "\n\n" + args
	}
	out := strings.ReplaceAll(c.Template, "$ARGUMENTS", args)
	for n := 1; n <= 9; n++ {
		w := ""
		if n <= len(words) {
			w = words[n-1]
		}
		out = strings.ReplaceAll(out, "$"+strconv.Itoa(n), w)
	}
	return strings.TrimSpace(out)
}

func validCommandName(n string) bool {
	if n == "" {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	for _, b := range BuiltinCommands {
		if b == n {
			return false
		}
	}
	return true
}

func readCommandDir(dir string, out map[string]CustomCommand) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		file := e.Name()
		if filepath.Ext(file) != ".md" {
			continue
		}
		name := strings.TrimSuffix(file, ".md")
		if !validCommandName(name) {
			continue
		}
		path := filepath.Join(dir, file)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.ToValidUTF8(string(data), "\uFFFD")
		if strings.TrimSpace(text) == "" {
			continue
		}
		out[name] = CustomCommand{Name: name, Path: path, Template: text}
	}
}

// LoadCommands returns all custom commands sorted by name. home is the ~/.agentiloop folder (may be "").
func LoadCommands(cwd, home string) []CustomCommand {
	m := map[string]CustomCommand{}
	if home != "" {
		readCommandDir(filepath.Join(home, "commands"), m)
	}
	readCommandDir(filepath.Join(cwd, ".agentiloop", "commands"), m)
	out := make([]CustomCommand, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResolveCommand is the prompt for a typed line like "/review src/main.rs"; ok is false when it is not a custom command.
func ResolveCommand(line, cwd, home string) (string, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(line), "/")
	if !found {
		return "", false
	}
	name, args := rest, ""
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		name, args = rest[:i], strings.TrimSpace(rest[i:])
	}
	for _, c := range LoadCommands(cwd, home) {
		if c.Name == name {
			return c.Expand(args), true
		}
	}
	return "", false
}

// CommandListing is the text for /commands.
func CommandListing(cmds []CustomCommand) string {
	if len(cmds) == 0 {
		return "no custom commands. Put a prompt in .agentiloop/commands/<name>.md (project) or ~/.agentiloop/commands/<name>.md, then type /<name> [arguments]"
	}
	lines := make([]string, len(cmds))
	for i, c := range cmds {
		lines[i] = "/" + c.Name + strings.Repeat(" ", max(1, 15-len(c.Name))) + c.Description()
	}
	return strings.Join(lines, "\n")
}
