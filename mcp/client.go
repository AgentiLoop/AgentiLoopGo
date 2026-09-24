package mcp

// MCP client: connect + initialize handshake, capability discovery, tool
// calls and resource reads. Port of AgentMCP's MCPClient.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	protocolVersion = "2024-11-05"
	maxText         = 1024 * 1024
	maxImage        = 10 * 1024 * 1024
	maxBlocks       = 100
)

var initTimeout = 90 * time.Second

// Version is reported as clientInfo.version; set by the CLI.
var Version = "0.0.2"

type ToolInfo struct {
	Name        string
	Description string
	InputSchema map[string]any
	// ReadOnly is annotations.readOnlyHint — read-only tools skip the permission prompt.
	ReadOnly bool
}

type ResourceInfo struct {
	URI, Name, Description, MimeType string
}

type Server struct {
	Name string
	// ServerInfo is serverInfo.name / version reported by the server.
	ServerInfo    string
	TransportKind string
	Tools         []ToolInfo
	Resources     []ResourceInfo
	conn          Transport
}

// Connect launches / connects, then runs initialize → notifications/initialized
// → tools/list / resources/list, all within 90 s.
func Connect(ctx context.Context, name string, cfg ServerConfig, cwd string) (*Server, error) {
	var conn Transport
	var kind string
	if cfg.IsHTTP() {
		u, err := httpURL(cfg)
		if err != nil {
			return nil, err
		}
		headers := map[string]string{}
		for k, v := range cfg.Headers {
			headers[k] = ExpandEnv(v)
		}
		legacy := cfg.Transport == "sse" || strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/sse") || cfg.SSEEndpoint != ""
		if legacy {
			t, err := ConnectLegacySSE(ctx, u.String(), headers)
			if err != nil {
				return nil, err
			}
			conn, kind = t, "sse"
		} else {
			conn, kind = NewHTTPTransport(u.String(), headers), "http"
		}
	} else {
		if cfg.Command == "" {
			return nil, errors.New("no `command` or `url` configured")
		}
		args := make([]string, len(cfg.Args))
		for i, a := range cfg.Args {
			args[i] = ExpandEnv(a)
		}
		t, err := SpawnStdio(resolveCommand(ExpandEnv(cfg.Command)), args, serverEnv(cfg.Env), cwd)
		if err != nil {
			return nil, err
		}
		conn, kind = t, "stdio"
	}

	hctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	s := &Server{Name: name, TransportKind: kind, conn: conn}
	if err := s.handshake(hctx); err != nil {
		conn.Close()
		if hctx.Err() == context.DeadlineExceeded {
			return nil, errors.New("initialization timed out after 90 seconds")
		}
		return nil, err
	}
	return s, nil
}

func (s *Server) IsAlive() bool { return s.conn.IsAlive() }
func (s *Server) Close()        { s.conn.Close() }

// CallTool runs tools/call. Returns the flattened text output and the isError
// flag; a JSON-RPC error is reported as a tool error, not a transport failure.
func (s *Server) CallTool(ctx context.Context, name string, arguments any) (string, bool, error) {
	if !s.conn.IsAlive() {
		return "", false, fmt.Errorf("MCP server `%s` is no longer running", s.Name)
	}
	resp, err := s.conn.Request(ctx, "tools/call", Msg{"name": name, "arguments": arguments})
	if err != nil {
		return "", false, err
	}
	if e, ok := resp["error"]; ok && e != nil {
		msg := "Unknown error"
		if em, ok := e.(map[string]any); ok {
			if m, ok := em["message"].(string); ok {
				msg = m
			}
		}
		return truncRunes(strings.ReplaceAll(msg, "\n", " "), 512), true, nil
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return "", false, errors.New("invalid tools/call response")
	}
	isErr, _ := result["isError"].(bool)
	return FormatContent(result), isErr, nil
}

// ReadResource runs resources/read and returns the text of the first content item.
func (s *Server) ReadResource(ctx context.Context, uri string) (string, error) {
	resp, err := s.conn.Request(ctx, "resources/read", Msg{"uri": uri})
	if err != nil {
		return "", err
	}
	result, _ := resp["result"].(map[string]any)
	contents, _ := result["contents"].([]any)
	if len(contents) == 0 {
		return "", fmt.Errorf("resource not found: %s", uri)
	}
	first, _ := contents[0].(map[string]any)
	if t, ok := first["text"].(string); ok {
		return t, nil
	}
	mime, ok := first["mimeType"].(string)
	if !ok {
		mime = "unknown type"
	}
	return fmt.Sprintf("[binary resource %s, %s]", uri, mime), nil
}

func (s *Server) handshake(ctx context.Context) error {
	resp, err := s.conn.Request(ctx, "initialize", Msg{
		"protocolVersion": protocolVersion,
		"capabilities":    Msg{},
		"clientInfo":      Msg{"name": "AgentiLoop", "version": Version},
	})
	if err != nil {
		return err
	}
	if e, ok := resp["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok {
			return fmt.Errorf("initialize failed: %s", m)
		}
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return errors.New("invalid initialize response")
	}
	s.ServerInfo = s.Name
	if info, ok := result["serverInfo"].(map[string]any); ok {
		if n, ok := info["name"].(string); ok {
			s.ServerInfo = n
			if v, ok := info["version"].(string); ok {
				s.ServerInfo = n + " " + v
			}
		}
	}
	if err := s.conn.Notify(ctx, "notifications/initialized", nil); err != nil {
		return err
	}
	caps, _ := result["capabilities"].(map[string]any)
	has := func(k string) bool { v, ok := caps[k]; return ok && v != nil }
	// Discovery failures leave the list empty rather than failing the server (as in AgentMCP).
	if has("tools") {
		if items, err := listAll(ctx, s.conn, "tools/list", "tools"); err == nil {
			for _, it := range items {
				if t, ok := parseTool(it); ok {
					s.Tools = append(s.Tools, t)
				}
			}
		}
	}
	if has("resources") {
		if items, err := listAll(ctx, s.conn, "resources/list", "resources"); err == nil {
			str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
			for _, it := range items {
				r, _ := it.(map[string]any)
				s.Resources = append(s.Resources, ResourceInfo{str(r, "uri"), str(r, "name"), str(r, "description"), str(r, "mimeType")})
			}
		}
	}
	return nil
}

// listAll runs a paged list call; key is the result array field.
func listAll(ctx context.Context, conn Transport, method, key string) ([]any, error) {
	var out []any
	cursor := ""
	for range 20 {
		var params any
		if cursor != "" {
			params = Msg{"cursor": cursor}
		}
		resp, err := conn.Request(ctx, method, params)
		if err != nil {
			return nil, err
		}
		result, ok := resp["result"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid %s response", method)
		}
		items, _ := result[key].([]any)
		out = append(out, items...)
		cursor, _ = result["nextCursor"].(string)
		if cursor == "" {
			break
		}
	}
	return out, nil
}

func parseTool(v any) (ToolInfo, bool) {
	tool, _ := v.(map[string]any)
	name, _ := tool["name"].(string)
	if name == "" || len(name) > 128 {
		return ToolInfo{}, false
	}
	for _, c := range name {
		if !(c < 128 && (c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')) {
			return ToolInfo{}, false
		}
	}
	schema := map[string]any{}
	if m, ok := tool["inputSchema"].(map[string]any); ok {
		for k, v := range m {
			schema[k] = v
		}
	}
	if _, ok := schema["properties"].(map[string]any); !ok {
		schema["properties"] = map[string]any{}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if data, err := json.Marshal(schema); err != nil || len(data) > 100_000 {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	desc, _ := tool["description"].(string)
	ro := false
	if ann, ok := tool["annotations"].(map[string]any); ok {
		ro, _ = ann["readOnlyHint"].(bool)
	}
	return ToolInfo{Name: name, Description: truncRunes(desc, 2048), InputSchema: schema, ReadOnly: ro}, true
}

// FormatContent flattens a tools/call result's content blocks into text for the model.
func FormatContent(result map[string]any) string {
	blocks, _ := result["content"].([]any)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	var parts []string
	for i, b := range blocks {
		if i >= maxBlocks {
			break
		}
		item, _ := b.(map[string]any)
		kind, ok := item["type"].(string)
		if !ok {
			kind = "text"
		}
		var part string
		switch kind {
		case "text":
			part = str(item, "text")
		case "image", "audio":
			data := str(item, "data")
			if len(data) > maxImage {
				part = fmt.Sprintf("[%s too large: %d bytes]", kind, len(data))
			} else {
				part = fmt.Sprintf("[%s: %s, %d bytes base64]", kind, str(item, "mimeType"), len(data))
			}
		case "resource":
			r, _ := item["resource"].(map[string]any)
			if t, ok := r["text"].(string); ok {
				part = t
			} else {
				part = fmt.Sprintf("[resource: %s]", truncRunes(str(r, "uri"), 2048))
			}
		case "resource_link":
			part = fmt.Sprintf("[resource link: %s]", str(item, "uri"))
		default:
			if t, ok := item["text"].(string); ok {
				part = t
			} else {
				part = fmt.Sprintf("[%s]", kind)
			}
		}
		parts = append(parts, truncRunes(part, maxText))
	}
	if len(parts) == 0 {
		if sc, ok := result["structuredContent"]; ok {
			data, _ := json.Marshal(sc)
			return string(data)
		}
	}
	return strings.Join(parts, "\n")
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func httpURL(cfg ServerConfig) (*url.URL, error) {
	raw := ExpandEnv(cfg.URL)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid URL: %s", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		switch strings.ToLower(u.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
		default:
			return nil, errors.New("plain HTTP is only allowed for localhost; use HTTPS for remote servers")
		}
	default:
		return nil, fmt.Errorf("only HTTP/HTTPS URLs are supported, got: %s", u.Scheme)
	}
	if ep := strings.Trim(cfg.HTTPEndpoint, "/"); ep != "" {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + ep
	}
	return u, nil
}

// extraPathDirs are prepended to PATH so servers launched via npx/uvx/brew are found.
func extraPathDirs() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	home, _ := os.UserHomeDir()
	return []string{
		home + "/.local/bin", "/opt/homebrew/bin", "/usr/local/bin", home + "/.cargo/bin",
		home + "/.nvm/current/bin", "/usr/bin", "/bin",
	}
}

// resolveCommand resolves a bare command name (e.g. uvx) against common tool dirs, then PATH.
func resolveCommand(command string) string {
	if strings.ContainsAny(command, `/\`) {
		return command
	}
	dirs := append(extraPathDirs(), filepath.SplitList(os.Getenv("PATH"))...)
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = append(exts, strings.Split(strings.ToLower(os.Getenv("PATHEXT")), ";")...)
		if len(exts) == 2 && exts[1] == "" {
			exts = []string{"", ".exe", ".cmd", ".bat"}
		}
	}
	for _, d := range dirs {
		for _, e := range exts {
			p := filepath.Join(d, command+e)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p
			}
		}
	}
	return command
}

// blockedEnv: env vars a config may never inject into a server process.
var blockedEnv = []string{"LD_PRELOAD", "LD_LIBRARY_PATH"}

// serverEnv layers config env over the inherited environment, with PATH widened
// and library-injection variables refused.
func serverEnv(cfgEnv map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range cfgEnv {
		up := strings.ToUpper(k)
		blocked := strings.HasPrefix(up, "DYLD_")
		for _, b := range blockedEnv {
			blocked = blocked || up == b
		}
		if !blocked {
			env[k] = ExpandEnv(v)
		}
	}
	if _, ok := env["PATH"]; !ok {
		dirs := extraPathDirs()
		if p := os.Getenv("PATH"); p != "" {
			dirs = append(dirs, p)
		}
		if len(dirs) > 0 {
			env["PATH"] = strings.Join(dirs, string(os.PathListSeparator))
		}
	}
	return env
}
