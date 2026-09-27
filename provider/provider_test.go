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

// Trimmed from developers.openai.com/api/docs/models/{gpt-4o-mini,gpt-5}.md.
const doc4oMini = "# GPT-4o mini\n\n## Model details\n\n- Default snapshot: `gpt-4o-mini-2024-07-18`\n- 128,000 context window\n- 16,384 max output tokens\n- Oct 01, 2023 knowledge cutoff\n"
const docGPT5 = "# GPT-5\n\n- 400,000 context window\n- Maximum input tokens: 272,000\n- 128,000 max output tokens\n"

func TestOpenAIModelInfoPullsPublishedLimitsFromModelDocs(t *testing.T) {
	// No /v1 suffix, so no Ollama probe; /models carries no limits.
	base := serveRoutes(t, map[string]string{
		"/models":              `{"data":[{"id":"gpt-4o-mini"},{"id":"gpt-4o-mini-2024-07-18"},{"id":"gpt-5"},{"id":"llama3"}]}`,
		"/docs/gpt-4o-mini.md": doc4oMini,
		"/docs/gpt-5.md":       docGPT5,
	})
	p := NewOpenAI("k", base)
	p.modelDocsURL = base + "/docs"
	m, err := p.ModelInfo(context.Background(), "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	if in, out := limits(m); in != 128_000 || out != 16_384 {
		t.Fatal(in, out)
	}
	// Dated snapshot has no page of its own → family page.
	m, err = p.ModelInfo(context.Background(), "gpt-4o-mini-2024-07-18")
	if err != nil {
		t.Fatal(err)
	}
	if in, out := limits(m); in != 128_000 || out != 16_384 {
		t.Fatal(in, out)
	}
	// Explicit input cap beats the context window.
	m, err = p.ModelInfo(context.Background(), "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if in, out := limits(m); in != 272_000 || out != 128_000 {
		t.Fatal(in, out)
	}
	m, err = p.ModelInfo(context.Background(), "llama3")
	if err != nil || m.MaxInputTokens != nil || m.MaxTokens != nil {
		t.Fatal(m, err)
	}
}

func TestModelDocParsingIgnoresProseAndNeedsBothLimits(t *testing.T) {
	// gpt-4.1-nano's blurb mentions "1M token context window" in prose; only the details list counts.
	md := "GPT-4.1 nano: 1M token context window, and low latency.\n\n- 1,047,576 context window\n- 32,768 max output tokens\n"
	if in, out, ok := parseModelDoc(md); !ok || in != 1_047_576 || out != 32_768 {
		t.Fatal(in, out, ok)
	}
	if _, _, ok := parseModelDoc("- 128,000 context window\n"); ok {
		t.Fatal("output missing should fail")
	}
	if _, _, ok := parseModelDoc("## Model details\n"); ok {
		t.Fatal("empty should fail")
	}
}

func TestOnlyOpenAILookingIDsAreLookedUp(t *testing.T) {
	for _, id := range []string{"gpt-4o", "gpt-5.2", "chatgpt-4o-latest", "o1-mini", "o3", "o4-mini-2025-04-16"} {
		if !looksLikeOpenAIModel(id) {
			t.Fatal(id)
		}
	}
	for _, id := range []string{"llama3", "qwen3:4b", "olmo-2", "claude-3", "o"} {
		if looksLikeOpenAIModel(id) {
			t.Fatal(id)
		}
	}
	if !isSnapshotDate("2024-09-12") || isSnapshotDate("preview") {
		t.Fatal("snapshot date")
	}
}

func TestOpenAIListModelsMissingCreatedSortsLast(t *testing.T) {
	p := NewOpenAI("k", serveRoutes(t, map[string]string{"/models": `{"data":[{"id":"b","created":1},{"id":"a","created":5},{"id":"c","created":5},{"id":"z"}]}`}))
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	// Missing created counts as 0 → oldest.
	if !reflect.DeepEqual(ids, []string{"a", "c", "b", "z"}) {
		t.Fatal(ids)
	}
	if models[0].DisplayName != "a" || models[0].CreatedAt != "" {
		t.Fatal(models[0])
	}
}

func TestOpenAIListModels401NamesProviderAndHint(t *testing.T) {
	p := NewOpenAI("k", serveOnce(t, 401, `{"error":{"message":"bad key","type":"invalid_request_error"}}`, 1000, nil))
	_, err := p.ListModels(context.Background())
	want := "openai 401 Unauthorized (invalid_request_error): bad key — API key missing or invalid; set OPENAI_API_KEY"
	if err == nil || err.Error() != want {
		t.Fatal(err)
	}

	p = NewOpenAI("k", serveOnce(t, 403, "nope", 1000, nil)).WithIdentity("omlx", "")
	_, err = p.ListModels(context.Background())
	want = "omlx 403 Forbidden : nope — API key missing or invalid; set OMLX_API_KEY or auth.api_key in ~/.omlx/settings.json"
	if err == nil || err.Error() != want {
		t.Fatal(err)
	}

	p = NewOpenAI("k", serveOnce(t, 500, `{"error":{"message":"boom"}}`, 1000, nil))
	_, err = p.ListModels(context.Background())
	if want = "openai 500 Internal Server Error (): boom"; err == nil || err.Error() != want {
		t.Fatal(err)
	}
}

func TestOpenAIListModelsRejectsMalformedCatalog(t *testing.T) {
	p := NewOpenAI("k", serveRoutes(t, map[string]string{"/models": `{"data":"nope"}`}))
	_, err := p.ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decoding /models") {
		t.Fatal(err)
	}
}

func TestOpenAIModelInfoPrefersCatalogLimitsOverPublishedDocs(t *testing.T) {
	base := serveRoutes(t, map[string]string{
		"/models":              `{"data":[{"id":"gpt-5","context_length":1000,"top_provider":{"max_completion_tokens":10}},{"id":"gpt-4o-mini","max_model_len":100}]}`,
		"/docs/gpt-5.md":       docGPT5,
		"/docs/gpt-4o-mini.md": doc4oMini,
	})
	p := NewOpenAI("k", base)
	p.modelDocsURL = base + "/docs"
	// Both limits from the catalog → docs never consulted.
	m, err := p.ModelInfo(context.Background(), "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if in, out := limits(m); in != 1000 || out != 10 {
		t.Fatal(in, out)
	}
	// Catalog input only → docs fill just the output cap.
	m, err = p.ModelInfo(context.Background(), "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	if in, out := limits(m); in != 100 || out != 16_384 {
		t.Fatal(in, out)
	}
}

func TestOpenAIModelInfoWithoutDocPageLeavesLimitsEmpty(t *testing.T) {
	base := serveRoutes(t, map[string]string{
		"/models":     `{"data":[{"id":"gpt-9"},{"id":"o9-2030-01-01"},{"id":"o3"}]}`,
		"/docs/o3.md": "- 200,000 context window\n- 100,000 max output tokens\n",
	})
	p := NewOpenAI("k", base)
	p.modelDocsURL = base + "/docs"
	// Unknown OpenAI-looking id: page 404s. Snapshot whose family page also 404s.
	for _, id := range []string{"gpt-9", "o9-2030-01-01"} {
		m, err := p.ModelInfo(context.Background(), id)
		if err != nil || m.MaxInputTokens != nil || m.MaxTokens != nil {
			t.Fatal(id, m, err)
		}
	}
	// Ids shorter than a date suffix still resolve.
	m, err := p.ModelInfo(context.Background(), "o3")
	if err != nil {
		t.Fatal(err)
	}
	if in, out := limits(m); in != 200_000 || out != 100_000 {
		t.Fatal(in, out)
	}
}

func TestOpenAIModelInfoDocsUnreachableIsNotAnError(t *testing.T) {
	p := NewOpenAI("k", serveRoutes(t, map[string]string{"/models": `{"data":[{"id":"gpt-5"}]}`}))
	p.modelDocsURL = "http://127.0.0.1:1/docs"
	m, err := p.ModelInfo(context.Background(), "gpt-5")
	if err != nil || m == nil || m.MaxInputTokens != nil || m.MaxTokens != nil {
		t.Fatal(m, err)
	}
}

func TestOpenAIOllamaContextWindowVariants(t *testing.T) {
	probe := func(show string) *uint64 {
		base := serveRoutes(t, map[string]string{"/api/show": show})
		return NewOpenAI("k", base+"/v1").ollamaContextLength(context.Background(), "m")
	}
	cases := []struct {
		show string
		want uint64 // 0 = nil
	}{
		// Model max only.
		{`{"model_info":{"llama.context_length":131072}}`, 131072},
		// num_ctx only.
		{`{"parameters":"num_ctx 4096"}`, 4096},
		// num_ctx above the model max doesn't raise it.
		{`{"model_info":{"llama.context_length":8192},"parameters":"num_ctx 32768"}`, 8192},
		// Neither → unknown.
		{`{"model_info":{"general.architecture":"llama"},"parameters":"stop \"x\""}`, 0},
		{`{"model_info":{"llama.context_length":"big"}}`, 0},
	}
	for _, c := range cases {
		got := probe(c.show)
		if (got == nil) != (c.want == 0) || (got != nil && *got != c.want) {
			t.Fatal(c.show, got)
		}
	}
	// Not an Ollama-style base URL → no probe at all (nothing is listening on :1).
	if NewOpenAI("k", "http://127.0.0.1:1").ollamaContextLength(context.Background(), "m") != nil {
		t.Fatal("probed a non-/v1 base")
	}
	// /api/show 404 (plain OpenAI-compatible server mounted under /v1).
	if NewOpenAI("k", serveRoutes(t, nil)+"/v1").ollamaContextLength(context.Background(), "m") != nil {
		t.Fatal("404 should be unknown")
	}
}

func TestModelDocParsingEdgeCases(t *testing.T) {
	// Indented list items and thousands separators.
	if in, out, ok := parseModelDoc("  - 1,047,576 context window\n  - 32,768 max output tokens\n"); !ok || in != 1_047_576 || out != 32_768 {
		t.Fatal(in, out, ok)
	}
	// Input cap without a context window line still counts.
	if in, out, ok := parseModelDoc("- Maximum input tokens: 272,000\n- 128,000 max output tokens\n"); !ok || in != 272_000 || out != 128_000 {
		t.Fatal(in, out, ok)
	}
	for _, md := range []string{
		// Only the list items count: a matching label in prose is ignored.
		"128,000 context window\n16,384 max output tokens\n",
		// A label with no number on its line yields nothing.
		"- context window\n- 16,384 max output tokens\n",
		// Output cap that doesn't fit a uint32 is rejected rather than truncated.
		"- 128,000 context window\n- 5,000,000,000 max output tokens\n",
		// Context window only, no output cap.
		"- Maximum input tokens: 272,000\n",
	} {
		if _, _, ok := parseModelDoc(md); ok {
			t.Fatal(md)
		}
	}
}

func TestSnapshotDateShape(t *testing.T) {
	for _, s := range []string{"2024-09-12", "2025-04-16", "0000-00-00"} {
		if !isSnapshotDate(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"2024-9-12", "2024/09/12", "2024-09-12x", "24-09-12", "abcd-ef-gh", ""} {
		if isSnapshotDate(s) {
			t.Fatal(s)
		}
	}
}

func TestOpenAIIdentityAndDefaultModel(t *testing.T) {
	p := NewOpenAI(" k ", "http://x/v1/")
	if p.Name() != "openai" || p.DefaultModel() != "gpt-4o-mini" || p.apiKey != "k" || p.baseURL != "http://x/v1" {
		t.Fatal(p.Name(), p.DefaultModel(), p.apiKey, p.baseURL)
	}
	p = p.WithIdentity("omlx", "")
	if p.Name() != "omlx" || p.DefaultModel() != "" {
		t.Fatal(p.Name(), p.DefaultModel())
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
	// No auth.json from `codex login` either.
	t.Setenv("CODEX_HOME", t.TempDir())
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

// api.openai.com 400s on max_tokens for gpt-5 / o-series ("Use
// 'max_completion_tokens' instead"); other servers keep max_tokens.
func TestOutputCapFieldDependsOnHost(t *testing.T) {
	body := func(base string) map[string]any {
		data, err := json.Marshal(NewOpenAI("k", base).wireRequest(req(), false))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	official := body(openAIDefaultBaseURL)
	if _, has := official["max_tokens"]; has || official["max_completion_tokens"] != float64(16) {
		t.Fatalf("official: %v", official)
	}
	local := body("http://localhost:11434/v1")
	if _, has := local["max_completion_tokens"]; has || local["max_tokens"] != float64(16) {
		t.Fatalf("local: %v", local)
	}
}

// gpt-6-* 400 on chat/completions tools with reasoning on; only real
// OpenAI is moved to /v1/responses.
func TestOnlyOfficialOpenAIUsesResponses(t *testing.T) {
	if !NewOpenAI("k", "https://api.openai.com/v1").usesResponses() {
		t.Fatal("api.openai.com should use /responses")
	}
	if NewOpenAI("k", "http://localhost:11434/v1").usesResponses() {
		t.Fatal("local server should keep chat/completions")
	}
	if NewOpenAI("k", "https://api.openai.com/v1").WithIdentity("omlx", "").usesResponses() {
		t.Fatal("rebranded backend should keep chat/completions")
	}
}
