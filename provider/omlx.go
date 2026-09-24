package provider

// oMLX (https://omlx.ai) — macOS-native MLX inference server with paged SSD
// KV caching and continuous batching. It exposes OpenAI-compatible Chat
// Completions on http://localhost:<port>/v1, so this is the OpenAI backend
// under its own name with oMLX defaults.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// omlxSettings holds server.port and auth.api_key from oMLX's own settings
// file, so a locally installed server works with no env vars at all.
type omlxSettings struct {
	port   int // 0 = unset
	apiKey string
}

func omlxSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".omlx", "settings.json")
}

func readOMLXSettings(path string) omlxSettings {
	var s omlxSettings
	data, err := os.ReadFile(path)
	if path == "" || err != nil {
		return s
	}
	var v struct {
		Server struct {
			Port *float64 `json:"port"`
		} `json:"server"`
		Auth struct {
			APIKey  *string `json:"api_key"`
			SkipVer *bool   `json:"skip_api_key_verification"`
		} `json:"auth"`
	}
	if json.Unmarshal(data, &v) != nil {
		return s
	}
	if p := v.Server.Port; p != nil && *p > 0 && *p <= 65535 && *p == float64(int(*p)) {
		s.port = int(*p)
	}
	// A key is only required when verification is on.
	if !(v.Auth.SkipVer != nil && *v.Auth.SkipVer) && v.Auth.APIKey != nil {
		s.apiKey = *v.Auth.APIKey
	}
	return s
}

// omlxDefaultBaseURL: OMLX_PORT, else the port in ~/.omlx/settings.json, else 8000.
func omlxDefaultBaseURL(s omlxSettings) string {
	port := os.Getenv("OMLX_PORT")
	if port == "" {
		port = "8000"
		if s.port != 0 {
			port = fmt.Sprint(s.port)
		}
	}
	return "http://localhost:" + port + "/v1"
}

// OMLXFromEnv reads OMLX_BASE_URL (or OMLX_PORT) and OMLX_API_KEY; anything not
// set falls back to ~/.omlx/settings.json. No default model: oMLX serves
// whatever is in its model directory, so the first entry of /v1/models is used
// unless --model is given.
func OMLXFromEnv() (*OpenAI, error) {
	s := readOMLXSettings(omlxSettingsPath())
	base := os.Getenv("OMLX_BASE_URL")
	if base == "" {
		base = omlxDefaultBaseURL(s)
	}
	key := os.Getenv("OMLX_API_KEY")
	if key == "" {
		key = s.apiKey
	}
	if key == "" {
		key = "none"
	}
	return NewOpenAI(key, base).WithIdentity("omlx", ""), nil
}
