package main

// First-run wizard (agentiloop --setup, /setup): pick a provider, enter a key,
// check the connection, choose a model, and save the credential to
// ~/.agentiloop/env (optionally also the shell profile or the macOS Keychain).
// Runs by itself when there are no credentials and nothing in ~/.agentiloop.
// Talks to the user through a prompter, so it works on the plain terminal and
// inside the TUI alike.
// Mirrors wizard.rs in the Rust AgentiLoop CLI.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"github.com/AgentiLoop/AgentiLoopGo/provider"
	"golang.org/x/term"
)

// noCredentials is true when nothing in the environment names a provider.
func noCredentials() bool {
	for _, k := range credentialVars {
		if os.Getenv(k) != "" {
			return false
		}
	}
	return true
}

// shouldRunWizard: only for a plain interactive launch on a terminal with nothing configured.
func shouldRunWizard(interactive, providerFlag bool) bool {
	return interactive && !providerFlag && noCredentials() &&
		term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

var errCancelled = errors.New("setup cancelled; nothing was saved")

// prompter is how the wizard talks to the user: the plain terminal
// (termPrompter) or the TUI's transcript and input line (setupPrompter).
type prompter interface {
	// say shows a line (may contain newlines; empty = blank line where that makes sense).
	say(line string)
	// ask shows prompt and waits for a line of input (trimmed).
	ask(prompt string) (string, error)
	// askSecret is like ask, but the answer is hidden as it is typed and not kept in history.
	askSecret(prompt string) (string, error)
}

// termPrompter is plain stdin/stdout, for the first run and the line REPL.
type termPrompter struct {
	in  *bufio.Reader
	out io.Writer
	tty bool // stdin is a terminal: hide secrets
}

func newTermPrompter(in io.Reader, out io.Writer) *termPrompter {
	return &termPrompter{in: bufio.NewReader(in), out: out, tty: term.IsTerminal(int(os.Stdin.Fd()))}
}

func (w *termPrompter) say(line string) { fmt.Fprintln(w.out, line) }

func (w *termPrompter) ask(prompt string) (string, error) {
	fmt.Fprint(w.out, prompt)
	line, err := w.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(w.out)
		return "", errCancelled
	}
	return strings.TrimSpace(line), nil
}

func (w *termPrompter) askSecret(prompt string) (string, error) {
	// Hidden input needs a terminal; scripted stdin (tests, pipes) falls back to a plain read.
	if !w.tty {
		return w.ask(prompt)
	}
	fmt.Fprint(w.out, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(w.out)
	if err != nil {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func askDefault(p prompter, prompt, def string) (string, error) {
	a, err := p.ask(fmt.Sprintf("%s [%s]: ", prompt, def))
	if a == "" {
		return def, err
	}
	return a, err
}

func askYes(p prompter, prompt string, defYes bool) (bool, error) {
	hint := "[y/N]"
	if defYes {
		hint = "[Y/n]"
	}
	a, err := p.ask(prompt + " " + hint + " ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(a) {
	case "":
		return defYes, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func choose(p prompter, prompt string, n, def int) (int, error) {
	for {
		a, err := p.ask(fmt.Sprintf("%s [1-%d, default %d]: ", prompt, n, def))
		if err != nil {
			return 0, err
		}
		if a == "" {
			return def, nil
		}
		if i, err := strconv.Atoi(a); err == nil && i >= 1 && i <= n {
			return i, nil
		}
		p.say(fmt.Sprintf("Please enter a number from 1 to %d.", n))
	}
}

// ---- shell profile ----

type shellKind int

const (
	shellPosix shellKind = iota
	shellFish
	shellPowerShell
)

// shellProfile is the profile the wizard may append to: AGENTILOOP_SHELL_PROFILE,
// else derived from $SHELL. "" on shells we don't know and on Windows (no $SHELL),
// where a user environment variable is offered instead (see windowsUserEnv).
func shellProfile() (string, shellKind) {
	if p, ok := os.LookupEnv("AGENTILOOP_SHELL_PROFILE"); ok {
		switch strings.ToLower(filepath.Ext(p)) {
		case ".fish":
			return p, shellFish
		case ".ps1":
			return p, shellPowerShell
		}
		return p, shellPosix
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", shellPosix
	}
	if sh, ok := os.LookupEnv("SHELL"); ok {
		return profileFor(sh, home, runtime.GOOS == "darwin")
	}
	return "", shellPosix
}

func profileFor(shell, home string, macos bool) (string, shellKind) {
	switch filepath.Base(shell) {
	case "zsh":
		return filepath.Join(home, ".zshrc"), shellPosix
	case "bash":
		if macos {
			return filepath.Join(home, ".bash_profile"), shellPosix
		}
		return filepath.Join(home, ".bashrc"), shellPosix
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish"), shellFish
	case "pwsh":
		return filepath.Join(home, ".config", "powershell", "profile.ps1"), shellPowerShell
	}
	return "", shellPosix
}

// windowsUserEnv: Windows without a Unix shell. Windows PowerShell 5 ships with
// ExecutionPolicy Restricted, so a profile.ps1 would silently never run, and
// ~\Documents may live in OneDrive. Persist as a *user environment variable*
// instead (the README's setx step); every new terminal window sees it.
func windowsUserEnv() bool {
	_, hasShell := os.LookupEnv("SHELL")
	_, hasProfile := os.LookupEnv("AGENTILOOP_SHELL_PROFILE")
	return runtime.GOOS == "windows" && !hasShell && !hasProfile
}

// userEnvSet runs `setx KEY value`: writes HKCU\Environment and broadcasts the change to Explorer.
func userEnvSet(key, value string) error {
	if err := exec.Command("setx", key, value).Run(); err != nil {
		return fmt.Errorf("setx %s failed: %w", key, err)
	}
	return nil
}

func exportLine(kind shellKind, key, value string) string {
	switch kind {
	case shellFish:
		return fmt.Sprintf("set -gx %s \"%s\"", key, value)
	case shellPowerShell:
		// Single quotes: PowerShell does not expand `$` or backticks inside them.
		return fmt.Sprintf("$env:%s = '%s'", key, strings.ReplaceAll(value, "'", "''"))
	}
	return fmt.Sprintf("export %s=\"%s\"", key, value)
}

func pathLine(kind shellKind, dir string) string {
	switch kind {
	case shellFish:
		return "fish_add_path " + dir
	case shellPowerShell:
		return fmt.Sprintf("$env:PATH = '%s' + [IO.Path]::PathSeparator + $env:PATH", dir)
	}
	return fmt.Sprintf("export PATH=\"%s:$PATH\"", dir)
}

// writeBlock replaces (or appends) the marked agentiloop block in path with lines.
func writeBlock(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing, _ := os.ReadFile(path)
	text := ""
	if len(existing) > 0 {
		text = removeBlock(string(existing))
	}
	if text != "" && !strings.HasSuffix(text, "\n\n") {
		text += "\n"
	}
	text += blockStart + "\n" + strings.Join(lines, "\n") + "\n" + blockEnd + "\n"
	text = matchLineEndings(string(existing), text)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// exeDirMissingFromPath returns the running binary's directory if it is not on PATH.
func exeDirMissingFromPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		// Windows paths compare case-insensitively.
		if d == dir || (runtime.GOOS == "windows" && strings.EqualFold(d, dir)) {
			return ""
		}
	}
	return dir
}

// ---- macOS Keychain ----

func keychainStore(service, value string) error {
	cmd := exec.Command("security", "add-generic-password", "-U", "-a", os.Getenv("USER"), "-s", service, "-w", value)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("security add-generic-password failed: %w", err)
	}
	return nil
}

func keychainLine(kind shellKind, key string) string {
	lookup := fmt.Sprintf("security find-generic-password -a \"$USER\" -s %s -w 2>/dev/null", key)
	switch kind {
	case shellFish:
		return fmt.Sprintf("set -gx %s (%s)", key, lookup)
	case shellPowerShell:
		ps := strings.NewReplacer(`"$USER"`, "$env:USER", "2>/dev/null", "2>$null").Replace(lookup)
		return fmt.Sprintf("$env:%s = (%s)", key, ps)
	}
	return fmt.Sprintf("export %s=\"$(%s)\"", key, lookup)
}

// ---- the wizard ----

type connected struct {
	name   string
	vars   [][2]string
	prov   core.Provider
	models []core.ModelInfo
}

func connect(ctx context.Context, p prompter) (*connected, error) {
	for {
		p.say("")
		p.say("Which model provider do you want to use?\n" +
			"  1  Claude (Anthropic) — API key from console.anthropic.com\n" +
			"  2  OpenAI — API key from platform.openai.com\n" +
			"  3  Ollama, LM Studio or another OpenAI-compatible server (local, usually no key)\n" +
			"  4  oMLX (local Apple Silicon server; reads ~/.omlx/settings.json)\n" +
			"  5  Codex — your ChatGPT Plus/Pro plan (sign in first with `codex login`)")
		choice, err := choose(p, "Provider", 5, 1)
		if err != nil {
			return nil, err
		}
		var name string
		var vars [][2]string
		switch choice {
		case 1:
			key, err := p.askSecret("Anthropic API key (starts with sk-ant-, input hidden): ")
			if err != nil {
				return nil, err
			}
			name, vars = "anthropic", [][2]string{{"ANTHROPIC_API_KEY", key}}
		case 2:
			key, err := p.askSecret("OpenAI API key (starts with sk-, input hidden): ")
			if err != nil {
				return nil, err
			}
			name, vars = "openai", [][2]string{{"OPENAI_API_KEY", key}}
		case 3:
			url, err := askDefault(p, "Server URL", "http://localhost:11434/v1")
			if err != nil {
				return nil, err
			}
			key, err := p.askSecret("API key (press Enter if the server needs none, input hidden): ")
			if err != nil {
				return nil, err
			}
			name, vars = "openai", [][2]string{{"OPENAI_BASE_URL", url}}
			if key != "" {
				vars = append(vars, [2]string{"OPENAI_API_KEY", key})
			}
		case 4:
			name = "omlx"
			home, _ := os.UserHomeDir()
			if _, err := os.Stat(filepath.Join(home, ".omlx", "settings.json")); err != nil {
				url, err := askDefault(p, "oMLX server URL", "http://localhost:8000/v1")
				if err != nil {
					return nil, err
				}
				vars = [][2]string{{"OMLX_BASE_URL", url}}
			}
		default:
			// Tokens come from ~/.codex/auth.json; nothing to store here.
			name = "codex"
		}
		empty := false
		for _, kv := range vars {
			if strings.HasSuffix(kv[0], "_KEY") && kv[1] == "" {
				empty = true
			}
		}
		if empty {
			p.say("The key is empty.")
			continue
		}
		for _, kv := range vars {
			os.Setenv(kv[0], kv[1])
		}
		p.say("Checking the connection…")
		prov, err := provider.FromEnv(name)
		var models []core.ModelInfo
		if err == nil {
			models, err = prov.ListModels(ctx)
		}
		if err == nil {
			p.say(fmt.Sprintf("Connected (%d model(s) available).", len(models)))
			return &connected{name, vars, prov, models}, nil
		}
		p.say(fmt.Sprintf("Connection failed.\n  %v", err))
		for _, kv := range vars {
			os.Unsetenv(kv[0])
		}
		again, err := askYes(p, "Try again?", true)
		if err != nil {
			return nil, err
		}
		if !again {
			return nil, errCancelled
		}
	}
}

func pickModel(p prompter, c *connected) (string, error) {
	def := c.prov.DefaultModel()
	if def == "" && len(c.models) > 0 {
		def = c.models[0].ID
	}
	if len(c.models) == 0 {
		p.say(fmt.Sprintf("The server lists no models; using `%s`. Change it later with /model.", def))
		return def, nil
	}
	p.say("")
	shown := c.models
	if len(shown) > 15 {
		shown = shown[:15]
	}
	list := "Pick a model (change it any time with /model):"
	for i, m := range shown {
		mark := ""
		if m.ID == def {
			mark = "  (default)"
		}
		list += fmt.Sprintf("\n  %2d  %s%s", i+1, m.ID, mark)
	}
	if len(c.models) > len(shown) {
		list += fmt.Sprintf("\n      … and %d more (type the id)", len(c.models)-len(shown))
	}
	p.say(list)
	for {
		a, err := p.ask(fmt.Sprintf("Model [1-%d, an id, or Enter for %s]: ", len(shown), def))
		if err != nil {
			return "", err
		}
		if a == "" {
			return def, nil
		}
		if i, err := strconv.Atoi(a); err == nil && i >= 1 && i <= len(shown) {
			return shown[i-1].ID, nil
		}
		for _, m := range c.models {
			if m.ID == a {
				return a, nil
			}
		}
		ok, err := askYes(p, fmt.Sprintf("`%s` is not in the list; use it anyway?", a), false)
		if err != nil {
			return "", err
		}
		if ok {
			return a, nil
		}
	}
}

// runWizard is agentiloop --setup / /setup. It mutates saved and writes settings.json.
func runWizard(ctx context.Context, saved *Settings, p prompter) error {
	home := agentiloopHome()
	if home == "" {
		return errors.New("no home directory")
	}
	p.say("Welcome to AgentiLoop! Let's set things up (about a minute).")
	p.say(fmt.Sprintf("Settings are kept in %s. Run `agentiloop --setup` or `/setup` to redo this, `agentiloop --reset` to start over.", home))

	c, err := connect(ctx, p)
	if err != nil {
		return err
	}
	model, err := pickModel(p, c)
	if err != nil {
		return err
	}

	// Where the credential lives. ~/.agentiloop/env is always the baseline unless the Keychain holds it.
	profile, kind := shellProfile()
	userEnv := windowsUserEnv()
	now := time.Now().UTC().Format(time.RFC3339)
	setup := Setup{CompletedAt: &now, Keychain: []string{}, UserEnv: []string{}}
	var block []string
	if len(c.vars) > 0 {
		p.say("")
		menu := fmt.Sprintf("Where should the credential be saved?\n  1  %s (recommended; only agentiloop reads it, file mode 600)", filepath.Join(home, "env"))
		n := 1
		if profile != "" {
			n = 2
			menu += fmt.Sprintf("\n  2  Also add it to %s so other tools in your terminal see it", profile)
			if runtime.GOOS == "darwin" {
				n = 3
				menu += fmt.Sprintf("\n  3  macOS Keychain, with a line in %s that reads it (nothing stored in plain text)", profile)
			}
		} else if userEnv {
			n = 2
			menu += "\n  2  Also save it as a Windows user environment variable (setx), so every new terminal window sees it"
		}
		p.say(menu)
		choice, err := choose(p, "Save to", n, 1)
		if err != nil {
			return err
		}
		switch choice {
		case 3:
			for _, kv := range c.vars {
				if strings.HasSuffix(kv[0], "_KEY") || strings.HasSuffix(kv[0], "_TOKEN") {
					if err := keychainStore(kv[0], kv[1]); err != nil {
						return err
					}
					setup.Keychain = append(setup.Keychain, kv[0])
					block = append(block, keychainLine(kind, kv[0]))
					p.say(fmt.Sprintf("stored %s in the Keychain", kv[0]))
				} else {
					block = append(block, exportLine(kind, kv[0], kv[1]))
				}
			}
		case 2:
			if _, err := saveEnvFile(c.vars); err != nil {
				return err
			}
			for _, kv := range c.vars {
				if userEnv {
					if err := userEnvSet(kv[0], kv[1]); err != nil {
						return err
					}
					setup.UserEnv = append(setup.UserEnv, kv[0])
					p.say(fmt.Sprintf("saved %s as a user environment variable (new terminal windows will see it)", kv[0]))
				} else {
					block = append(block, exportLine(kind, kv[0], kv[1]))
				}
			}
		default:
			path, err := saveEnvFile(c.vars)
			if err != nil {
				return err
			}
			p.say("saved to " + path)
		}
	}

	// PATH: offer once, only when a profile is available. On Windows the README's
	// install step already adds the folder to the user PATH, so just point there.
	if dir := exeDirMissingFromPath(); dir != "" {
		switch {
		case profile != "":
			p.say("")
			p.say(fmt.Sprintf("`%s` is not on your PATH, so `agentiloop` only works with its full path.", dir))
			ok, err := askYes(p, fmt.Sprintf("Add it to PATH in %s?", profile), true)
			if err != nil {
				return err
			}
			if ok {
				block = append(block, pathLine(kind, dir))
			}
		case userEnv:
			p.say("")
			p.say(fmt.Sprintf("`%s` is not on your PATH. To run `agentiloop` from any folder, add it once in PowerShell:\n"+
				"  [Environment]::SetEnvironmentVariable(\"Path\", [Environment]::GetEnvironmentVariable(\"Path\", \"User\") + \";%s\", \"User\")", dir, dir))
		}
	}
	if len(block) > 0 {
		if err := writeBlock(profile, block); err != nil {
			return err
		}
		setup.Profile = &profile
		p.say(fmt.Sprintf("updated %s (between `%s` and `%s`); it applies to new terminals", profile, blockStart, blockEnd))
	}

	saved.SetModel(c.name, model)
	name := c.name
	saved.Last.Provider = &name
	saved.Setup = setup
	if err := saveSettings(saved); err != nil {
		return err
	}

	p.say("")
	p.say(fmt.Sprintf("All set: %s / %s. Type a request at the prompt, /help for commands, /exit to leave.", c.name, model))
	p.say("")
	return nil
}
