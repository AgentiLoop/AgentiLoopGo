package main

// Persistent user settings at ~/.agentiloop/settings.json (JSON, like Claude
// Code's ~/.claude/settings.json). Override the location with AGENTILOOP_HOME.
// The file format is shared with the Rust AgentiLoop CLI.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

type Settings struct {
	// Model is the last model selected via /model (legacy, Anthropic-only); superseded by Models.
	Model *string `json:"model"`
	// Models is the last model selected, per provider name.
	Models map[string]string `json:"models"`
	// Last holds options from the last interactive launch, reused when not given on the command line.
	Last LastLaunch `json:"last"`
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
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}
