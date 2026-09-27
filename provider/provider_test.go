package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

// serveOnce serves body in small chunks (splitting SSE lines mid-way to exercise
// the buffering) and records the request body.
func serveOnce(t *testing.T, status int, body string, chunk int, got *[]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			*got, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(status)
		for i := 0; i < len(body); i += chunk {
			w.Write([]byte(body[i:min(i+chunk, len(body))]))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func req() core.ProviderRequest {
	return core.ProviderRequest{Model: "m", System: "s", Messages: []core.Message{core.UserText("hi")}, MaxTokens: 16}
}

func jsonEq(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var a, b any
	json.Unmarshal(got, &a)
	json.Unmarshal([]byte(want), &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("got %s want %s", got, want)
	}
}

// ---- anthropic ----------------------------------------------------------------

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m","usage":{"input_tokens":25,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\": "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.rs\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicStreamsTextDeltasAndAssemblesToolUse(t *testing.T) {
	var body []byte
	p := &Anthropic{client: &http.Client{}, credential: "sk-ant-api-test", baseURL: serveOnce(t, 200, anthropicSSE, 37, &body)}
	var deltas []string
	resp, err := p.CompleteStream(context.Background(), req(), func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deltas, []string{"Hel", "lo"}) || resp.Message.Text() != "Hello" ||
		resp.StopReason != core.StopToolUse || resp.InputTokens != 25 || resp.OutputTokens != 15 {
		t.Fatalf("%v %+v", deltas, resp)
	}
	calls := resp.Message.ToolUses()
	if len(calls) != 1 || calls[0].ID != "toolu_1" || calls[0].Name != "read_file" {
		t.Fatalf("%+v", calls)
	}
	jsonEq(t, calls[0].Input, `{"path":"a.rs"}`)
	// API-key auth: a single system block and "stream": true.
	var sent map[string]any
	json.Unmarshal(body, &sent)
	if sent["stream"] != true || len(sent["system"].([]any)) != 1 {
		t.Fatalf("request %s", body)
	}
}

func TestAnthropicOAuthTokenAddsIdentityAndBearer(t *testing.T) {
	var gotAuth, gotBeta string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("authorization"), r.Header.Get("anthropic-beta")
		body, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	defer srv.Close()
	p := &Anthropic{client: &http.Client{}, credential: sanitize(" sk-ant-oat01-abc\n"), baseURL: srv.URL}
	resp, err := p.Complete(context.Background(), req())
	if err != nil || resp.Message.Text() != "ok" || resp.StopReason != core.StopEndTurn {
		t.Fatal(err, resp)
	}
	if gotAuth != "Bearer sk-ant-oat01-abc" || !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Fatal(gotAuth, gotBeta)
	}
	var sent struct{ System []struct{ Text string } }
	json.Unmarshal(body, &sent)
	if len(sent.System) != 2 || sent.System[0].Text != claudeCodeIdentity || sent.System[1].Text != "s" {
		t.Fatalf("%s", body)
	}
}

func TestAnthropicErrorBodyIsReported(t *testing.T) {
	base := serveOnce(t, 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, 1000, nil)
	p := &Anthropic{client: &http.Client{}, credential: "bad", baseURL: base}
	_, err := p.Complete(context.Background(), req())
	if err == nil || err.Error() != "Anthropic 401 Unauthorized (authentication_error): invalid x-api-key" {
		t.Fatal(err)
	}
}

func TestAnthropicModelInfoCarriesLimitsAndUnknownIsNil(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/models/nope" {
			w.WriteHeader(404)
			w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"model: nope"}}`))
			return
		}
		w.Write([]byte(`{"id":"claude-opus-5","display_name":"Claude Opus 5","created_at":"2026-01-01T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000}`))
	}))
	defer srv.Close()
	p := &Anthropic{client: &http.Client{}, credential: "k", baseURL: srv.URL}

	m, err := core.LookupModelInfo(context.Background(), p, "claude-opus-5")
	if err != nil || m == nil || m.MaxInputTokens == nil || *m.MaxInputTokens != 1_000_000 || m.MaxTokens == nil || *m.MaxTokens != 128_000 {
		t.Fatalf("%v %+v", err, m)
	}
	m, err = p.ModelInfo(context.Background(), "nope")
	if err != nil || m != nil {
		t.Fatalf("%v %+v", err, m)
	}
	if !reflect.DeepEqual(paths, []string{"/v1/models/claude-opus-5", "/v1/models/nope"}) {
		t.Fatal(paths)
	}
}

// ---- openai -------------------------------------------------------------------

func TestHistoryFlattensToOpenAIRoles(t *testing.T) {
	history := []core.Message{
		core.UserText("hi"),
		{Role: core.RoleAssistant, Content: []core.ContentBlock{
			core.TextBlock("checking"),
			core.ToolUseBlock("call_1", "list_dir", json.RawMessage(`{"path": "."}`)),
		}},
		core.ToolResults([]core.ContentBlock{core.ToolResultBlock("call_1", "a.rs", false)}),
		{Role: core.RoleAssistant, Content: []core.ContentBlock{core.ToolUseBlock("call_2", "bash", nil)}},
	}
	data, _ := json.Marshal(toWireMessages("sys", history))
	jsonEq(t, data, `[
		{"role":"system","content":"sys"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"a.rs"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_2","type":"function","function":{"name":"bash","arguments":"{}"}}]}
	]`)
}

// Shape captured from Ollama's /v1 endpoint: tool_call id+name in one chunk,
// arguments fragmented across later chunks, usage in a trailing choices-less chunk.
const sseTool = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me "},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"look."},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_abc","index":0,"type":"function","function":{"name":"list_dir","arguments":""}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"/tmp\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":139,"completion_tokens":21,"total_tokens":160}}

data: [DONE]

`

func TestOpenAIStreamAssemblesTextAndFragmentedToolCall(t *testing.T) {
	var body []byte
	p := NewOpenAI("k", serveOnce(t, 200, sseTool, 41, &body))
	var deltas []string
	resp, err := p.CompleteStream(context.Background(), req(), func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deltas, []string{"Let me ", "look."}) || resp.Message.Text() != "Let me look." ||
		resp.StopReason != core.StopToolUse || resp.InputTokens != 139 || resp.OutputTokens != 21 {
		t.Fatalf("%v %+v", deltas, resp)
	}
	calls := resp.Message.ToolUses()
	if len(calls) != 1 || calls[0].ID != "call_abc" || calls[0].Name != "list_dir" {
		t.Fatalf("%+v", calls)
	}
	jsonEq(t, calls[0].Input, `{"path":"/tmp"}`)
	var sent map[string]any
	json.Unmarshal(body, &sent)
	if sent["stream"] != true || sent["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("%s", body)
	}
}

const sseText = `data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

func TestOpenAIStreamPlainTextEndsTurnWithoutUsage(t *testing.T) {
	p := NewOpenAI("k", serveOnce(t, 200, sseText, 41, nil))
	var deltas []string
	resp, err := p.CompleteStream(context.Background(), req(), func(s string) { deltas = append(deltas, s) })
	if err != nil || !reflect.DeepEqual(deltas, []string{"hi"}) || resp.StopReason != core.StopEndTurn ||
		resp.InputTokens != 0 || resp.OutputTokens != 0 || len(resp.Message.ToolUses()) != 0 {
		t.Fatal(err, deltas, resp)
	}
}

const nonStream = `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`

func TestOpenAINonStreamingToolCallWithStopIsToolUse(t *testing.T) {
	p := NewOpenAI("k", serveOnce(t, 200, nonStream, 41, nil))
	resp, err := p.Complete(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	// Some servers say "stop" even when tool_calls are present; we must still loop.
	calls := resp.Message.ToolUses()
	if resp.StopReason != core.StopToolUse || calls[0].Name != "bash" || resp.InputTokens != 7 || resp.OutputTokens != 3 {
		t.Fatalf("%+v", resp)
	}
	jsonEq(t, calls[0].Input, `{"command":"ls"}`)
}

func TestOpenAI401NamesProviderAndHint(t *testing.T) {
	p := NewOpenAI("k", serveOnce(t, 401, `{"error":{"message":"bad key","type":"invalid_request_error"}}`, 1000, nil)).WithIdentity("omlx", "")
	_, err := p.Complete(context.Background(), req())
	want := "omlx 401 Unauthorized (invalid_request_error): bad key — API key missing or invalid; set OMLX_API_KEY or auth.api_key in ~/.omlx/settings.json"
	if err == nil || err.Error() != want {
		t.Fatal(err)
	}
}

func TestOpenAIListModelsNewestFirst(t *testing.T) {
	p := NewOpenAI("k", serveOnce(t, 200, `{"data":[{"id":"b","created":1},{"id":"a","created":5},{"id":"c","created":5}]}`, 1000, nil))
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if !reflect.DeepEqual(ids, []string{"a", "c", "b"}) {
		t.Fatal(ids)
	}
}

// serveRoutes serves canned JSON bodies keyed by request path (404 otherwise).
func serveRoutes(t *testing.T, routes map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			body = "{}"
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func limits(m *core.ModelInfo) (in uint64, out int) {
	if m.MaxInputTokens != nil {
		in = *m.MaxInputTokens
	}
	if m.MaxTokens != nil {
		out = *m.MaxTokens
	}
	return in, out
}

func TestOpenAIListModelsReadsLimitsReportedByCatalog(t *testing.T) {
	// OpenRouter shape (context_length + top_provider) and vLLM/oMLX shape (max_model_len).
	base := serveRoutes(t, map[string]string{
		"/models": `{"data":[{"id":"a","context_length":131072,"top_provider":{"max_completion_tokens":8192}},{"id":"b","max_model_len":32768}]}`,
	})
	models, err := NewOpenAI("k", base).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if in, out := limits(&models[0]); in != 131072 || out != 8192 {
		t.Fatal(in, out)
	}
	if in, out := limits(&models[1]); in != 32768 || models[1].MaxTokens != nil {
		t.Fatal(in, out)
	}
}

func TestOpenAIModelInfoAsksOllamaForContextWindowCappedByNumCtx(t *testing.T) {
	base := serveRoutes(t, map[string]string{
		"/v1/models": `{"data":[{"id":"qwen3:4b"}]}`,
		"/api/show":  `{"model_info":{"qwen3.context_length":262144},"parameters":"stop \"x\"\nnum_ctx 8192"}`,
	})
	p := NewOpenAI("k", base+"/v1")
	m, err := p.ModelInfo(context.Background(), "qwen3:4b")
	if err != nil || m == nil {
		t.Fatal(m, err)
	}
	if in, _ := limits(m); in != 8192 || m.MaxTokens != nil {
		t.Fatal(in, m.MaxTokens)
	}
	if m, err := p.ModelInfo(context.Background(), "missing"); err != nil || m != nil {
		t.Fatal(m, err)
	}
}

func TestOpenAIModelInfoFillsInPublishedLimits(t *testing.T) {
	// No /v1 suffix, so no Ollama probe; /models carries no limits.
	base := serveRoutes(t, map[string]string{"/models": `{"data":[{"id":"gpt-4o-mini"},{"id":"llama3"}]}`})
	p := NewOpenAI("k", base)
	m, err := p.ModelInfo(context.Background(), "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	if in, out := limits(m); in != 128_000 || out != 16_384 {
		t.Fatal(in, out)
	}
	m, err = p.ModelInfo(context.Background(), "llama3")
	if err != nil || m.MaxInputTokens != nil || m.MaxTokens != nil {
		t.Fatal(m, err)
	}
}

func TestOpenAILimitsPreferLongestPrefix(t *testing.T) {
	cases := []struct {
		id  string
		in  uint64
		out int
		ok  bool
	}{
		{"o1-mini-2024-09-12", 128_000, 65_536, true},
		{"o1-preview", 200_000, 100_000, true},
		{"gpt-4.1-nano", 1_047_576, 32_768, true},
		{"gpt-oss:120b", 0, 0, false},
	}
	for _, c := range cases {
		if in, out, ok := openAILimits(c.id); in != c.in || out != c.out || ok != c.ok {
			t.Fatal(c.id, in, out, ok)
		}
	}
}

func TestFinishReasonMapping(t *testing.T) {
	s := func(v string) *string { return &v }
	cases := []struct {
		raw   *string
		calls bool
		want  core.StopReason
	}{
		{s("stop"), false, core.StopEndTurn}, {s("stop"), true, core.StopToolUse}, {s("tool_calls"), true, core.StopToolUse},
		{s("length"), false, core.StopMaxTokens}, {nil, false, core.StopOther}, {nil, true, core.StopToolUse},
	}
	for _, c := range cases {
		if got := parseFinish(c.raw, c.calls); got != c.want {
			t.Fatalf("%v %v → %v", c.raw, c.calls, got)
		}
	}
}

func TestEmptyToolArgumentsBecomeEmptyObject(t *testing.T) {
	for _, raw := range []string{"", "  "} {
		if v, err := parseToolArgs("x", raw); err != nil || string(v) != "{}" {
			t.Fatal(v, err)
		}
	}
	if _, err := parseToolArgs("x", "{not json"); err == nil {
		t.Fatal("expected error")
	}
}

// ---- omlx ---------------------------------------------------------------------

func TestOMLXIdentityHasNoFixedDefaultModel(t *testing.T) {
	p, _ := OMLXFromEnv()
	if p.Name() != "omlx" || p.DefaultModel() != "" {
		t.Fatal(p.Name(), p.DefaultModel())
	}
}

func TestOMLXSettingsFileSuppliesPortAndKey(t *testing.T) {
	t.Setenv("OMLX_PORT", "")
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"server":{"port":7777},"auth":{"api_key":"sk-omlx-abc","skip_api_key_verification":false}}`), 0o644)
	s := readOMLXSettings(p)
	if s.port != 7777 || s.apiKey != "sk-omlx-abc" || omlxDefaultBaseURL(s) != "http://localhost:7777/v1" {
		t.Fatalf("%+v", s)
	}
	// Verification off → no key needed.
	os.WriteFile(p, []byte(`{"server":{"port":7777},"auth":{"api_key":"sk","skip_api_key_verification":true}}`), 0o644)
	if readOMLXSettings(p).apiKey != "" {
		t.Fatal("key should be ignored")
	}
	// Missing/garbage file → defaults.
	none := readOMLXSettings(filepath.Join(dir, "nope.json"))
	if none.port != 0 || none.apiKey != "" || omlxDefaultBaseURL(none) != "http://localhost:8000/v1" {
		t.Fatalf("%+v", none)
	}
}

// ---- selection ----------------------------------------------------------------

func TestFromEnvPicksProvider(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OMLX_BASE_URL", "OMLX_PORT", "OMLX_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if _, err := FromEnv(""); err == nil || !strings.Contains(err.Error(), "no provider credentials") {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	if p, err := FromEnv(""); err != nil || p.Name() != "openai" {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-x")
	if p, err := FromEnv(""); err != nil || p.Name() != "anthropic" {
		t.Fatal(err)
	}
	if p, err := FromEnv("OMLX"); err != nil || p.Name() != "omlx" {
		t.Fatal(err)
	}
	if _, err := FromEnv("nope"); err == nil || !strings.Contains(err.Error(), "unknown provider `nope`") {
		t.Fatal(err)
	}
}
