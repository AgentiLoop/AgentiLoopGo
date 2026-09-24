// Package provider holds the model backends: Anthropic Messages API, OpenAI-compatible
// Chat Completions (OpenAI, Ollama, LM Studio, Groq, OpenRouter, …), and oMLX (local MLX server).
package provider

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

// FromEnv builds a provider by name ("anthropic" | "openai" | "omlx"), or picks
// one from the environment when name is empty: Anthropic if ANTHROPIC_API_KEY /
// ANTHROPIC_OAUTH_TOKEN is set, otherwise OpenAI if OPENAI_API_KEY or
// OPENAI_BASE_URL is set, otherwise oMLX if OMLX_BASE_URL / OMLX_PORT /
// OMLX_API_KEY is set.
func FromEnv(name string) (core.Provider, error) {
	has := func(k string) bool { return os.Getenv(k) != "" }
	switch {
	case name != "":
		name = strings.ToLower(name)
	case has("ANTHROPIC_API_KEY") || has("ANTHROPIC_OAUTH_TOKEN"):
		name = "anthropic"
	case has("OPENAI_API_KEY") || has("OPENAI_BASE_URL"):
		name = "openai"
	case has("OMLX_BASE_URL") || has("OMLX_PORT") || has("OMLX_API_KEY"):
		name = "omlx"
	default:
		return nil, errors.New("no provider credentials found: set ANTHROPIC_API_KEY, OPENAI_API_KEY / OPENAI_BASE_URL (e.g. http://localhost:11434/v1 for Ollama), or use `-p omlx` for a local oMLX server")
	}
	switch name {
	case "anthropic":
		return AnthropicFromEnv()
	case "openai":
		return OpenAIFromEnv()
	case "omlx":
		return OMLXFromEnv()
	}
	return nil, fmt.Errorf("unknown provider `%s` (expected anthropic, openai, or omlx)", name)
}
