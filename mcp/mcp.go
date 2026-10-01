// Package mcp is the Model Context Protocol client (stdio, Streamable HTTP and
// legacy HTTP+SSE), ported from Agent!'s AgentMCP. Every tool a connected
// server exposes is registered as mcp_<server>_<tool>.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

// ToolName returns an API-safe name: tool names must match ^[a-zA-Z0-9_-]{1,64}$.
func ToolName(server, tool string) string {
	var sb strings.Builder
	n := 0
	for _, c := range "mcp_" + server + "_" + tool {
		if n == 64 {
			break
		}
		if c < 128 && (c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			sb.WriteRune(c)
		} else {
			sb.WriteByte('_')
		}
		n++
	}
	return sb.String()
}

// Tool is one MCP server tool exposed through the agent's tool registry.
type Tool struct {
	server      *Server
	info        ToolInfo
	name        string
	description string
}

func (t *Tool) Name() string        { return t.name }
func (t *Tool) Description() string { return t.description }
func (t *Tool) InputSchema() any    { return t.info.InputSchema }

// IsMutating: external tools can do anything, so they're gated unless marked read-only.
func (t *Tool) IsMutating() bool { return !t.info.ReadOnly }

func (t *Tool) Call(ctx context.Context, _ core.ToolContext, input json.RawMessage) (string, error) {
	var args any
	if json.Unmarshal(input, &args) != nil {
		args = nil
	}
	if _, ok := args.(map[string]any); !ok {
		args = map[string]any{}
	}
	if len(input) > 1024*1024 {
		return "", core.InvalidInput("arguments exceed 1 MB limit")
	}
	text, isErr, err := t.server.CallTool(ctx, t.info.Name, args)
	switch {
	case err != nil:
		return "", core.Failed("MCP error: %v", err)
	case isErr:
		return "", core.Failed("%s", text)
	}
	return text, nil
}

// ReadResourceTool is mcp_read_resource: read a resource from any connected server.
type ReadResourceTool struct {
	servers     []*Server
	description string
}

func (*ReadResourceTool) Name() string          { return "mcp_read_resource" }
func (r *ReadResourceTool) Description() string { return r.description }
func (*ReadResourceTool) IsMutating() bool      { return false }
func (*ReadResourceTool) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{"type": "string", "description": "MCP server name"},
			"uri":    map[string]any{"type": "string", "description": "Resource URI"},
		},
		"required": []string{"server", "uri"},
	}
}

func (r *ReadResourceTool) Call(ctx context.Context, _ core.ToolContext, input json.RawMessage) (string, error) {
	var in map[string]any
	json.Unmarshal(input, &in)
	get := func(k string) (string, error) {
		if v, ok := in[k].(string); ok {
			return v, nil
		}
		return "", core.InvalidInput("`%s` is required", k)
	}
	server, err := get("server")
	if err != nil {
		return "", err
	}
	uri, err := get("uri")
	if err != nil {
		return "", err
	}
	for _, s := range r.servers {
		if s.Name == server {
			text, err := s.ReadResource(ctx, uri)
			if err != nil {
				return "", core.Failed("%v", err)
			}
			return text, nil
		}
	}
	return "", core.InvalidInput("no connected MCP server named `%s`", server)
}

// ServerError records why a configured server didn't connect.
type ServerError struct{ Name, Err string }

// Manager holds all configured servers: the ones that connected, and why the others didn't.
type Manager struct {
	Servers []*Server
	// Errors: config-file parse errors use the name "config".
	Errors []ServerError
}

// Start loads mcpServers from paths and connects every enabled server concurrently.
func Start(ctx context.Context, paths []string, cwd string) *Manager {
	configs, parseErrs := LoadConfig(paths)
	m := &Manager{}
	for _, e := range parseErrs {
		m.Errors = append(m.Errors, ServerError{"config", e})
	}
	names := make([]string, 0, len(configs))
	for n, c := range configs {
		if c.ShouldStart() {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	servers := make([]*Server, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			servers[i], errs[i] = Connect(ctx, n, configs[n], cwd)
		}()
	}
	wg.Wait()
	for i, n := range names {
		if errs[i] != nil {
			m.Errors = append(m.Errors, ServerError{n, errs[i].Error()})
		} else {
			m.Servers = append(m.Servers, servers[i])
		}
	}
	return m
}

func (m *Manager) IsEmpty() bool { return len(m.Servers) == 0 && len(m.Errors) == 0 }

func (m *Manager) ToolCount() int {
	n := 0
	for _, s := range m.Servers {
		n += len(s.Tools)
	}
	return n
}

// RegisterTools adds every discovered tool (plus mcp_read_resource when any server has resources).
func (m *Manager) RegisterTools(reg *core.ToolRegistry) {
	var resources []string
	for _, s := range m.Servers {
		for _, info := range s.Tools {
			desc := fmt.Sprintf("`%s` tool from MCP server `%s`", info.Name, s.Name)
			if info.Description != "" {
				desc = fmt.Sprintf("%s (MCP server `%s`)", info.Description, s.Name)
			}
			reg.Register(&Tool{server: s, info: info, name: ToolName(s.Name, info.Name), description: desc})
		}
		for _, r := range s.Resources {
			if len(resources) < 50 {
				resources = append(resources, fmt.Sprintf("- %s %s (%s)", s.Name, r.URI, r.Name))
			}
		}
	}
	if len(resources) > 0 {
		reg.Register(&ReadResourceTool{
			servers:     m.Servers,
			description: "Read a resource from a connected MCP server. Available:\n" + strings.Join(resources, "\n"),
		})
	}
}

// PromptCommandName is the slash-command name for a server prompt: mcp__<server>__<prompt>.
func PromptCommandName(server, prompt string) string { return "mcp__" + server + "__" + prompt }

// ParsePromptArgs maps the words typed after a prompt command onto its declared arguments. name=value sets
// that argument; other words fill the remaining arguments in order, and the last one takes the rest of the
// text. The error names a missing required argument.
func ParsePromptArgs(info PromptInfo, text string) (map[string]string, error) {
	out := map[string]string{}
	declared := func(n string) bool {
		for _, a := range info.Arguments {
			if a.Name == n {
				return true
			}
		}
		return false
	}
	var positional []string
	for _, w := range strings.Fields(text) {
		if k, v, ok := strings.Cut(w, "="); ok && declared(k) {
			out[k] = v
		} else {
			positional = append(positional, w)
		}
	}
	var free []PromptArg
	for _, a := range info.Arguments {
		if _, set := out[a.Name]; !set {
			free = append(free, a)
		}
	}
	for i, a := range free {
		if i >= len(positional) {
			break
		}
		if i == len(free)-1 {
			out[a.Name] = strings.Join(positional[i:], " ")
		} else {
			out[a.Name] = positional[i]
		}
	}
	for _, a := range info.Arguments {
		if _, set := out[a.Name]; a.Required && !set {
			usage := make([]string, len(info.Arguments))
			for i, x := range info.Arguments {
				usage[i] = "[" + x.Name + "]"
				if x.Required {
					usage[i] = "<" + x.Name + ">"
				}
			}
			return nil, fmt.Errorf("missing required argument `%s`. Usage: %s", a.Name, strings.Join(usage, " "))
		}
	}
	return out, nil
}

// PromptCommand handles a typed line like "/mcp__docs__summarize some text": the prompt text fetched from that
// server. ok is false when the line is not an MCP prompt command; err is set when it is but cannot be run.
func (m *Manager) PromptCommand(ctx context.Context, line string) (text string, ok bool, err error) {
	rest, found := strings.CutPrefix(strings.TrimSpace(line), "/")
	if !found {
		return "", false, nil
	}
	cmd, args := rest, ""
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		cmd, args = rest[:i], strings.TrimSpace(rest[i:])
	}
	for _, s := range m.Servers {
		for _, p := range s.Prompts {
			if PromptCommandName(s.Name, p.Name) != cmd {
				continue
			}
			a, err := ParsePromptArgs(p, args)
			if err != nil {
				return "", true, err
			}
			t, err := s.GetPrompt(ctx, p.Name, a)
			if err != nil {
				return "", true, fmt.Errorf("MCP prompt failed: %v", err)
			}
			if strings.TrimSpace(t) == "" {
				return "", true, fmt.Errorf("the MCP prompt returned no text")
			}
			return t, true, nil
		}
	}
	return "", false, nil
}

// StatusLines is the human-readable status for /mcp.
func (m *Manager) StatusLines() []string {
	var out []string
	for _, s := range m.Servers {
		state := "disconnected"
		if s.IsAlive() {
			state = "connected"
		}
		out = append(out, fmt.Sprintf("● %s [%s] %s — %s (%d tools, %d resources)",
			s.Name, s.TransportKind, s.ServerInfo, state, len(s.Tools), len(s.Resources)))
		for _, t := range s.Tools {
			first, _, _ := strings.Cut(t.Description, "\n")
			out = append(out, fmt.Sprintf("    %s  %s", ToolName(s.Name, t.Name), truncRunes(first, 80)))
		}
		for _, p := range s.Prompts {
			first, _, _ := strings.Cut(p.Description, "\n")
			args := ""
			for _, a := range p.Arguments {
				if a.Required {
					args += " <" + a.Name + ">"
				} else {
					args += " [" + a.Name + "]"
				}
			}
			out = append(out, fmt.Sprintf("    /%s%s  %s", PromptCommandName(s.Name, p.Name), args, truncRunes(first, 60)))
		}
	}
	for _, e := range m.Errors {
		out = append(out, fmt.Sprintf("✗ %s: %s", e.Name, e.Err))
	}
	return out
}

func (m *Manager) Shutdown() {
	var wg sync.WaitGroup
	for _, s := range m.Servers {
		wg.Add(1)
		go func() { defer wg.Done(); s.Close() }()
	}
	wg.Wait()
}
