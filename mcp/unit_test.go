package mcp

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSSEParsesEventsAcrossChunks(t *testing.T) {
	var p sseParser
	if evs := p.push([]byte("event: endpoint\ndata: /messages?s")); len(evs) != 0 {
		t.Fatal(evs)
	}
	evs := p.push([]byte("=1\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":3}\n\n"))
	if len(evs) != 2 || evs[0] != (sseEvent{"endpoint", "/messages?s=1"}) {
		t.Fatalf("%+v", evs)
	}
	if _, ok := evs[0].messageJSON(); ok {
		t.Fatal("endpoint event is not a message")
	}
	m, _ := evs[1].messageJSON()
	if id, ok := responseID(m); !ok || id != 3 {
		t.Fatal(m)
	}
}

func TestSSEFlushesTrailingEventAndStringIDs(t *testing.T) {
	var p sseParser
	p.push([]byte(`data: {"id":"7"}`))
	ev, ok := p.flush()
	m, _ := ev.messageJSON()
	if id, idOK := responseID(m); !ok || !idOK || id != 7 {
		t.Fatal(ev)
	}
}

func TestConfigParsesAgentAndClaudeShapes(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/mcp.json"
	os.WriteFile(p, []byte(`{"mcpServers":{
		"Hello":{"transport":"stdio","command":"/bin/hello","args":["-v"],"env":{}},
		"Demo":{"type":"http","url":"http://localhost:8085/mcp","headers":{}},
		"Off":{"command":"x","disabled":true},
		"Off2":{"command":"x","enabled":false}
	}}`), 0o644)
	s, errs := LoadConfig([]string{p, dir + "/missing.json"})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	h, d := s["Hello"], s["Demo"]
	if h.IsHTTP() || !reflect.DeepEqual(h.Args, []string{"-v"}) || !d.IsHTTP() || d.Transport != "http" {
		t.Fatalf("%+v %+v", h, d)
	}
	off, off2 := s["Off"], s["Off2"]
	if off.ShouldStart() || off2.ShouldStart() || !h.ShouldStart() {
		t.Fatal("ShouldStart")
	}
}

func TestExpandsEnvVars(t *testing.T) {
	t.Setenv("AGENTILOOP_MCP_TEST", "tok")
	os.Unsetenv("AGENTILOOP_MCP_NOPE")
	for in, want := range map[string]string{
		"Bearer ${AGENTILOOP_MCP_TEST}": "Bearer tok",
		"${AGENTILOOP_MCP_NOPE:-x}/y":   "x/y",
		"plain ${oops":                  "plain ${oops",
	} {
		if got := ExpandEnv(in); got != want {
			t.Fatalf("%q → %q", in, got)
		}
	}
}

func TestToolSchemaIsNormalizedAndNamesValidated(t *testing.T) {
	ti, ok := parseTool(map[string]any{"name": "echo", "inputSchema": map[string]any{"properties": nil}})
	if !ok || !reflect.DeepEqual(ti.InputSchema, map[string]any{"type": "object", "properties": map[string]any{}}) {
		t.Fatalf("%+v", ti)
	}
	if _, ok := parseTool(map[string]any{"name": "bad name"}); ok {
		t.Fatal("bad name accepted")
	}
	ro, _ := parseTool(map[string]any{"name": "get", "annotations": map[string]any{"readOnlyHint": true}})
	if !ro.ReadOnly {
		t.Fatal("readOnlyHint")
	}
}

func TestContentBlocksFlattenToText(t *testing.T) {
	r := map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "hi"},
		map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"},
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///x"}},
	}}
	if got := FormatContent(r); got != "hi\n[image: image/png, 4 bytes base64]\n[resource: file:///x]" {
		t.Fatal(got)
	}
	if got := FormatContent(map[string]any{"content": []any{}, "structuredContent": map[string]any{"a": 1}}); got != `{"a":1}` {
		t.Fatal(got)
	}
}

func TestRemotePlainHTTPIsRefused(t *testing.T) {
	cfg := func(u string) ServerConfig { return ServerConfig{URL: u} }
	if _, err := httpURL(cfg("http://example.com/mcp")); err == nil {
		t.Fatal("remote http allowed")
	}
	if _, err := httpURL(cfg("http://localhost:8085/mcp")); err != nil {
		t.Fatal(err)
	}
	c := cfg("https://x.dev/api/")
	c.HTTPEndpoint = "/mcp"
	if u, err := httpURL(c); err != nil || u.String() != "https://x.dev/api/mcp" {
		t.Fatal(u, err)
	}
}

func TestToolNamesAreAPISafe(t *testing.T) {
	if got := ToolName("Hello World", "say.hi"); got != "mcp_Hello_World_say_hi" {
		t.Fatal(got)
	}
	if got := ToolName(strings.Repeat("x", 80), "t"); len(got) != 64 {
		t.Fatal(len(got))
	}
}

func TestServerEnvBlocksInjectionAndWidensPath(t *testing.T) {
	env := serverEnv(map[string]string{"DYLD_INSERT_LIBRARIES": "x", "LD_PRELOAD": "y", "KEEP": "z"})
	if _, ok := env["DYLD_INSERT_LIBRARIES"]; ok {
		t.Fatal("DYLD_ passed through")
	}
	if _, ok := env["LD_PRELOAD"]; ok {
		t.Fatal("LD_PRELOAD passed through")
	}
	if env["KEEP"] != "z" || env["PATH"] == "" {
		t.Fatalf("%v", env)
	}
}
