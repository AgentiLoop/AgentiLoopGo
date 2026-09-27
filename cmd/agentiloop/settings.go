package main

// Persistent user settings at ~/.agentiloop/settings.json (JSON, like Claude
// Code's ~/.claude/settings.json). Override the location with AGENTILOOP_HOME.
// The file format is shared with the Rust AgentiLoop CLI.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// envPath holds credentials written by the first-run wizard, one KEY=value per line (mode 0600).
func envPath() string { return homeFile("env") }

// loadEnvFile loads ~/.agentiloop/env into the process environment. Variables
// that are already set (and non-empty) win, so a shell export always overrides the file.
func loadEnvFile() {
	p := envPath()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	for _, kv := range parseEnvFile(string(data)) {
		if os.Getenv(kv[0]) == "" {
			os.Setenv(kv[0], kv[1])
		}
	}
}

func parseEnvFile(text string) [][2]string {
	var out [][2]string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		out = append(out, [2]string{k, strings.Trim(strings.TrimSpace(v), `"`)})
	}
	return out
}

// saveEnvFile writes ~/.agentiloop/env (replacing it) with the given variables.
func saveEnvFile(vars [][2]string) (string, error) {
	p := envPath()
	if p == "" {
		return "", errors.New("no home directory")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Written by `agentiloop --setup`. Delete with `agentiloop --reset`.\n")
	for _, kv := range vars {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return p, os.Chmod(p, 0o600)
}

type Settings struct {
	// Model is the last model selected via /model (legacy, Anthropic-only); superseded by Models.
	Model *string `json:"model"`
	// Models is the last model selected, per provider name.
	Models map[string]string `json:"models"`
	// Last holds options from the last interactive launch, reused when not given on the command line.
	Last LastLaunch `json:"last"`
	// Setup records what the first-run wizard wrote outside ~/.agentiloop, so --reset can undo exactly that.
	Setup Setup `json:"setup"`
}

// Setup is the record of the first-run wizard (agentiloop --setup).
type Setup struct {
	// CompletedAt is an RFC 3339 timestamp; nil until the wizard has completed.
	CompletedAt *string `json:"completed_at"`
	// Profile is the shell profile that holds the "# >>> agentiloop >>>" block, if one was written.
	Profile *string `json:"profile"`
	// Keychain lists the macOS Keychain items (service names) the wizard created.
	Keychain []string `json:"keychain"`
	// UserEnv lists the Windows user environment variables (setx) the wizard created.
	UserEnv []string `json:"user_env"`
}

// LastLaunch is the remembered launch options. --yes and --no-mcp are deliberately never remembered.
type LastLaunch struct {
	Provider  *string `json:"provider"`
	TUI       bool    `json:"tui"`
	MaxTurns  *int    `json:"max_turns"`
	CompactAt *uint64 `json:"compact_at"`
}

func (s Settings) ModelFor(provider string) string {
	if m, ok := s.Models[provider]; ok {
		return m
	}
	if provider == "anthropic" && s.Model != nil {
		return *s.Model
	}
	return ""
}

func (s *Settings) SetModel(provider, model string) {
	if s.Models == nil {
		s.Models = map[string]string{}
	}
	s.Models[provider] = model
	if provider == "anthropic" {
		s.Model = &model
	}
}

func agentiloopHome() string {
	if h, ok := os.LookupEnv("AGENTILOOP_HOME"); ok {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agentiloop")
}

func homeFile(name string) string {
	h := agentiloopHome()
	if h == "" {
		return ""
	}
	return filepath.Join(h, name)
}

func settingsPath() string { return homeFile("settings.json") }

// historyPath is the prompt history (one entry per line), used for up/down arrow recall.
func historyPath() string { return homeFile("history.txt") }

// sessionsDir holds saved conversations, one JSON file per session.
func sessionsDir() string { return homeFile("sessions") }

// mcpConfigPath is the user-level MCP servers file; the project's .mcp.json is merged on top.
func mcpConfigPath() string { return homeFile("mcp.json") }

func loadSettings() Settings {
	s := Settings{Models: map[string]string{}}
	p := settingsPath()
	if p == "" {
		return s
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return s
	}
	var loaded Settings
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("ignoring malformed settings", "path", p, "err", err)
		return s
	}
	if loaded.Models == nil {
		loaded.Models = map[string]string{}
	}
	return loaded
}

func saveSettings(s *Settings) error {
	p := settingsPath()
	if p == "" {
		return errors.New("no home directory")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if s.Models == nil {
		s.Models = map[string]string{}
	}
	if s.Setup.Keychain == nil {
		s.Setup.Keychain = []string{} // Rust reads `null` as malformed; keep it `[]`
	}
	if s.Setup.UserEnv == nil {
		s.Setup.UserEnv = []string{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}
