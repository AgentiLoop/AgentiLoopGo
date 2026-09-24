package mcp

// mcpServers config files — the same JSON shape Agent!, Claude Code and
// Claude Desktop use:
//
//	{ "mcpServers": {
//	    "HelloWorld": { "command": "mcp-server-hello", "args": [], "env": {} },
//	    "DemoHttp":   { "transport": "http", "url": "http://localhost:8085/mcp", "headers": {} }
//	} }

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ServerConfig struct {
	// Transport is "stdio", "http" / "streamable-http", or "sse" (legacy). Inferred when empty.
	// `type` is accepted as an alias.
	Transport string
	// stdio
	Command string
	Args    []string
	Env     map[string]string
	// HTTP
	URL     string
	Headers map[string]string
	// SSEEndpoint non-empty forces the legacy HTTP+SSE transport (as in AgentMCP).
	SSEEndpoint string
	// HTTPEndpoint is a path appended to URL for Streamable HTTP POSTs.
	HTTPEndpoint string
	// Agent! metadata / Claude-style flag
	Enabled   *bool
	AutoStart *bool
	Disabled  bool
}

func (c *ServerConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Transport    *string           `json:"transport"`
		Type         *string           `json:"type"`
		Command      string            `json:"command"`
		Args         []string          `json:"args"`
		Env          map[string]string `json:"env"`
		URL          string            `json:"url"`
		Headers      map[string]string `json:"headers"`
		SSEEndpoint  string            `json:"sseEndpoint"`
		HTTPEndpoint string            `json:"httpEndpoint"`
		Enabled      *bool             `json:"enabled"`
		AutoStart    *bool             `json:"autoStart"`
		Disabled     bool              `json:"disabled"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = ServerConfig{
		Command: raw.Command, Args: raw.Args, Env: raw.Env, URL: raw.URL, Headers: raw.Headers,
		SSEEndpoint: raw.SSEEndpoint, HTTPEndpoint: raw.HTTPEndpoint,
		Enabled: raw.Enabled, AutoStart: raw.AutoStart, Disabled: raw.Disabled,
	}
	if raw.Transport != nil {
		c.Transport = *raw.Transport
	} else if raw.Type != nil {
		c.Transport = *raw.Type
	}
	return nil
}

func (c *ServerConfig) IsHTTP() bool { return c.URL != "" }

func (c *ServerConfig) ShouldStart() bool {
	on := func(b *bool) bool { return b == nil || *b }
	return on(c.Enabled) && on(c.AutoStart) && !c.Disabled
}

// DefaultPaths are the standard config locations, lowest precedence first: the
// user file, then the project's .mcp.json (same name overrides).
func DefaultPaths(userFile, cwd string) []string {
	var out []string
	if userFile != "" {
		out = append(out, userFile)
	}
	return append(out, filepath.Join(cwd, ".mcp.json"))
}

// LoadConfig merges every existing file. Unparsable files are reported, not fatal.
func LoadConfig(paths []string) (map[string]ServerConfig, []string) {
	servers := map[string]ServerConfig{}
	var errs []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var file struct {
			MCPServers map[string]ServerConfig `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		for k, v := range file.MCPServers {
			servers[k] = v
		}
	}
	return servers, errs
}

// ExpandEnv expands ${VAR} / ${VAR:-default} from the environment, so secrets
// can stay out of the config file.
func ExpandEnv(s string) string {
	var out strings.Builder
	rest := s
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			break
		}
		out.WriteString(rest[:start])
		after := rest[start+2:]
		end := strings.IndexByte(after, '}')
		if end < 0 {
			out.WriteString(rest[start:])
			return out.String()
		}
		expr := after[:end]
		name, def, _ := strings.Cut(expr, ":-")
		if v, ok := os.LookupEnv(name); ok {
			out.WriteString(v)
		} else {
			out.WriteString(def)
		}
		rest = after[end+1:]
	}
	out.WriteString(rest)
	return out.String()
}
