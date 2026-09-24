package mcp_test

// End-to-end tests of all three MCP transports (stdio, Streamable HTTP,
// legacy HTTP+SSE) against the bundled examples/mcp-example-server.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"github.com/AgentiLoop/AgentiLoopGo/mcp"
)

var example string

// TestMain builds examples/mcp-example-server once for the whole package.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agl-mcp-example")
	if err != nil {
		panic(err)
	}
	example = filepath.Join(dir, "mcp-example-server")
	if runtime.GOOS == "windows" {
		example += ".exe"
	}
	build := exec.Command("go", "build", "-o", example, "../examples/mcp-example-server")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building example server:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ctx gives every test a hard deadline so a transport bug fails instead of hanging CI.
func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// startHTTP starts the example server in an HTTP mode; returns its port. Killed at test end.
func startHTTP(t *testing.T, mode string) (int, *exec.Cmd) {
	t.Helper()
	cmd := exec.Command(example, mode, "0")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	var port int
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "listening on %d", &port); err != nil {
		t.Fatalf("port line %q: %v", line, err)
	}
	return port, cmd
}

func stdioCfg() mcp.ServerConfig {
	return mcp.ServerConfig{Command: example, Args: []string{"--stdio"}}
}

func urlCfg(u string) mcp.ServerConfig { return mcp.ServerConfig{URL: u} }

func call(t *testing.T, s *mcp.Server, name string, args any) (string, bool) {
	t.Helper()
	out, isErr, err := s.CallTool(ctx(t), name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out, isErr
}

// exercise runs the same checks for every transport.
func exercise(t *testing.T, s *mcp.Server, transport string) {
	if s.TransportKind != transport || s.ServerInfo != "example-"+transport+" 1.0.0" {
		t.Fatalf("%s %s", s.TransportKind, s.ServerInfo)
	}
	// Two tools/list pages were merged and the invalid name dropped.
	var names []string
	byName := map[string]mcp.ToolInfo{}
	for _, ti := range s.Tools {
		names = append(names, ti.Name)
		byName[ti.Name] = ti
	}
	if !reflect.DeepEqual(names, []string{"echo", "add", "fail", "slow"}) {
		t.Fatal(names)
	}
	if !byName["add"].ReadOnly || byName["echo"].ReadOnly {
		t.Fatal("readOnly")
	}
	if !reflect.DeepEqual(byName["fail"].InputSchema, map[string]any{"type": "object", "properties": map[string]any{}}) {
		t.Fatal(byName["fail"].InputSchema)
	}
	if len(s.Resources) != 1 || s.Resources[0].URI != "example://greeting" {
		t.Fatal(s.Resources)
	}

	if out, isErr := call(t, s, "echo", map[string]any{"message": "hi ✓"}); out != "hi ✓" || isErr {
		t.Fatal(out)
	}
	if out, _ := call(t, s, "add", map[string]any{"a": 2, "b": 3}); out != "5" {
		t.Fatal(out)
	}
	// isError result, and a JSON-RPC error, both surface as tool errors.
	if out, isErr := call(t, s, "fail", map[string]any{}); out != "this tool always fails" || !isErr {
		t.Fatal(out)
	}
	if out, isErr := call(t, s, "nope", map[string]any{}); !isErr || !strings.Contains(out, "unknown tool: nope") {
		t.Fatal(out)
	}

	if text, err := s.ReadResource(ctx(t), "example://greeting"); err != nil || text != "Hello from "+transport+"!" {
		t.Fatal(text, err)
	}
	if _, err := s.ReadResource(ctx(t), "example://missing"); err == nil {
		t.Fatal("missing resource should fail")
	}

	// Concurrent requests: a slow call must not block or steal the others' responses.
	var wg sync.WaitGroup
	results := make([]string, 10)
	var slow string
	wg.Add(11)
	go func() { defer wg.Done(); slow, _, _ = s.CallTool(ctx(t), "slow", map[string]any{"ms": 300}) }()
	for i := range 10 {
		go func() {
			defer wg.Done()
			results[i], _, _ = s.CallTool(ctx(t), "echo", map[string]any{"message": fmt.Sprintf("m%d", i)})
		}()
	}
	wg.Wait()
	if slow != "slept 300ms" {
		t.Fatal(slow)
	}
	for i, r := range results {
		if r != fmt.Sprintf("m%d", i) {
			t.Fatalf("result %d = %q", i, r)
		}
	}

	if !s.IsAlive() {
		t.Fatal("should be alive")
	}
	s.Close()
	if s.IsAlive() {
		t.Fatal("should be closed")
	}
	if _, _, err := s.CallTool(ctx(t), "echo", map[string]any{"message": "x"}); err == nil {
		t.Fatal("call after close should fail")
	}
}

func TestStdioTransport(t *testing.T) {
	s, err := mcp.Connect(ctx(t), "Stdio", stdioCfg(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s, "stdio")
}

func TestStreamableHTTPTransport(t *testing.T) {
	port, _ := startHTTP(t, "--http")
	s, err := mcp.Connect(ctx(t), "Http", urlCfg(fmt.Sprintf("http://127.0.0.1:%d/mcp", port)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s, "http")
}

func TestStreamableHTTPEndpointPathIsAppended(t *testing.T) {
	port, _ := startHTTP(t, "--http")
	cfg := urlCfg(fmt.Sprintf("http://localhost:%d/", port))
	cfg.HTTPEndpoint = "/mcp"
	s, err := mcp.Connect(ctx(t), "Http", cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if out, _ := call(t, s, "echo", map[string]any{"message": "ok"}); out != "ok" {
		t.Fatal(out)
	}
}

func TestLegacySSETransportAutodetectedFromURL(t *testing.T) {
	port, _ := startHTTP(t, "--sse")
	s, err := mcp.Connect(ctx(t), "Sse", urlCfg(fmt.Sprintf("http://127.0.0.1:%d/sse", port)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s, "sse")
}

func TestLegacySSETransportForcedByConfig(t *testing.T) {
	port, _ := startHTTP(t, "--sse")
	// URL doesn't end in /sse: "transport": "sse" or a non-empty sseEndpoint forces legacy.
	u := fmt.Sprintf("http://127.0.0.1:%d/events", port)
	byTransport, byEndpoint := urlCfg(u), urlCfg(u)
	byTransport.Transport = "sse"
	byEndpoint.SSEEndpoint = "/events"
	for _, cfg := range []mcp.ServerConfig{byTransport, byEndpoint} {
		s, err := mcp.Connect(ctx(t), "Sse", cfg, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if s.TransportKind != "sse" {
			t.Fatal(s.TransportKind)
		}
		if out, _ := call(t, s, "add", map[string]any{"a": 1.5, "b": 1}); out != "2.5" {
			t.Fatal(out)
		}
		s.Close()
	}
	// Without either, the same URL is treated as Streamable HTTP (and the SSE server rejects the POST).
	if _, err := mcp.Connect(ctx(t), "Sse", urlCfg(u), t.TempDir()); err == nil {
		t.Fatal("expected failure")
	}
}

func TestConnectionFailuresAreReported(t *testing.T) {
	if _, err := mcp.Connect(ctx(t), "x", mcp.ServerConfig{Command: "/definitely/not/a/server"}, t.TempDir()); err == nil {
		t.Fatal("missing binary should fail")
	}
	// Wrong path on a live Streamable HTTP server → 404 during initialize.
	port, _ := startHTTP(t, "--http")
	_, err := mcp.Connect(ctx(t), "x", urlCfg(fmt.Sprintf("http://127.0.0.1:%d/wrong", port)), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal(err)
	}
	// Nothing listening on the SSE port.
	port, cmd := startHTTP(t, "--sse")
	cmd.Process.Kill()
	cmd.Wait()
	if _, err := mcp.Connect(ctx(t), "x", urlCfg(fmt.Sprintf("http://127.0.0.1:%d/sse", port)), t.TempDir()); err == nil {
		t.Fatal("dead SSE server should fail")
	}
	// Remote plain HTTP is refused before any network I/O.
	_, err = mcp.Connect(ctx(t), "x", urlCfg("http://example.com/mcp"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "localhost") {
		t.Fatal(err)
	}
}

func TestStdioServerExitIsDetected(t *testing.T) {
	s, err := mcp.Connect(ctx(t), "Stdio", stdioCfg(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsAlive() {
		t.Fatal("not alive")
	}
	s.Close() // kills the process
	time.Sleep(200 * time.Millisecond)
	if s.IsAlive() {
		t.Fatal("still alive")
	}
	_, _, err = s.CallTool(ctx(t), "echo", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no longer running") {
		t.Fatal(err)
	}
}

// Config file → manager → agent tool registry → tool call, across all three transports at once.
func TestManagerRegistersToolsFromAllTransports(t *testing.T) {
	hp, _ := startHTTP(t, "--http")
	sp, _ := startHTTP(t, "--sse")
	dir := t.TempDir()
	t.Setenv("AGL_MCP_TEST_PORT", fmt.Sprint(sp))
	cfgJSON, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"Local":  map[string]any{"command": example, "args": []string{"--stdio"}},
		"Web":    map[string]any{"type": "http", "url": fmt.Sprintf("http://127.0.0.1:%d/mcp", hp)},
		"Legacy": map[string]any{"url": "http://127.0.0.1:${AGL_MCP_TEST_PORT}/sse"},
		"Off":    map[string]any{"command": example, "args": []string{"--stdio"}, "disabled": true},
		"Broken": map[string]any{"command": "/no/such/binary"},
	}})
	cfg := filepath.Join(dir, "mcp.json")
	os.WriteFile(cfg, cfgJSON, 0o644)

	mgr := mcp.Start(ctx(t), []string{cfg}, dir)
	var names []string
	for _, s := range mgr.Servers {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"Legacy", "Local", "Web"}) {
		t.Fatal(names)
	}
	if len(mgr.Errors) != 1 || mgr.Errors[0].Name != "Broken" || mgr.ToolCount() != 12 {
		t.Fatalf("%+v %d", mgr.Errors, mgr.ToolCount())
	}

	reg := core.NewToolRegistry()
	mgr.RegisterTools(reg)
	if reg.Len() != 13 { // 12 tools + mcp_read_resource
		t.Fatal(reg.Len())
	}
	tc := core.ToolContext{Cwd: dir}
	for _, server := range []string{"Local", "Web", "Legacy"} {
		echo, _ := reg.Get("mcp_" + server + "_echo")
		if !echo.IsMutating() {
			t.Fatal("echo should be mutating")
		}
		if out, err := echo.Call(ctx(t), tc, json.RawMessage(`{"message":"`+server+`"}`)); err != nil || out != server {
			t.Fatal(out, err)
		}
		if add, _ := reg.Get("mcp_" + server + "_add"); add.IsMutating() {
			t.Fatal("add is read-only")
		}
		fail, _ := reg.Get("mcp_" + server + "_fail")
		_, err := fail.Call(ctx(t), tc, json.RawMessage(`{}`))
		if !core.IsToolError(err, core.ErrFailed) || err.Error() != "this tool always fails" {
			t.Fatal(err)
		}
	}
	read, _ := reg.Get("mcp_read_resource")
	if out, err := read.Call(ctx(t), tc, json.RawMessage(`{"server":"Legacy","uri":"example://greeting"}`)); err != nil || out != "Hello from sse!" {
		t.Fatal(out, err)
	}
	if _, err := read.Call(ctx(t), tc, json.RawMessage(`{"server":"Nope","uri":"x"}`)); err == nil {
		t.Fatal("unknown server should fail")
	}

	status := strings.Join(mgr.StatusLines(), "\n")
	for _, want := range []string{"● Local [stdio] example-stdio 1.0.0 — connected (4 tools, 1 resources)", "[http]", "[sse]", "✗ Broken"} {
		if !strings.Contains(status, want) {
			t.Fatalf("missing %q in\n%s", want, status)
		}
	}
	mgr.Shutdown()
	for _, s := range mgr.Servers {
		if s.IsAlive() {
			t.Fatal(s.Name, "still alive")
		}
	}
}

func TestBadConfigFileIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mcp.json")
	os.WriteFile(p, []byte("{ not json"), 0o644)
	mgr := mcp.Start(ctx(t), []string{p, filepath.Join(dir, "missing.json")}, dir)
	if len(mgr.Servers) != 0 || len(mgr.Errors) != 1 || mgr.Errors[0].Name != "config" {
		t.Fatalf("%+v", mgr)
	}
}

// ---- live servers ----------------------------------------------------------------

// Live round-trip against AgentMCP's HelloWorld stdio server when installed
// (~/bin/mcp-server-hello); skipped otherwise.
func TestLiveHelloWorldStdio(t *testing.T) {
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "mcp-server-hello")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("mcp-server-hello not installed")
	}
	s, err := mcp.Connect(ctx(t), "HelloWorld", mcp.ServerConfig{Command: bin}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := call(t, s, "hello", map[string]any{"name": "Todd"})
	t.Log("hello →", out)
	if isErr || !strings.Contains(out, "Todd") {
		t.Fatal(out)
	}
}

// Live Streamable HTTP round-trip against AgentMCP's DemoHttp server
// (~/bin/mcp-server-demo-http <port>, Python) when installed; skipped otherwise.
func TestLiveDemoHTTP(t *testing.T) {
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, "bin", "mcp-server-demo-http")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("mcp-server-demo-http not installed")
	}
	port := 18000 + os.Getpid()%1000
	cmd := exec.Command(bin, fmt.Sprint(port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var s *mcp.Server
	for range 50 {
		time.Sleep(100 * time.Millisecond)
		if c, err := mcp.Connect(ctx(t), "DemoHttp", urlCfg(fmt.Sprintf("http://localhost:%d/mcp", port)), t.TempDir()); err == nil {
			s = c
			break
		}
	}
	if s == nil {
		t.Fatal("demo-http server did not come up")
	}
	defer s.Close()
	out, isErr := call(t, s, "calculate", map[string]any{"operation": "add", "a": 2, "b": 3})
	if isErr || !strings.Contains(out, "5") {
		t.Fatal(out)
	}
	if out, _ := call(t, s, "reverse_string", map[string]any{"text": "Agent!"}); out != "!tnegA" {
		t.Fatal(out)
	}
}
