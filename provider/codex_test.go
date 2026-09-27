package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

const codexFar = 4_000_000_000

func testJWT(exp int64, account string) string {
	claims, _ := json.Marshal(map[string]any{"exp": exp, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	return "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
}

func codexAuthFile(t *testing.T, access string) string {
	path := filepath.Join(t.TempDir(), "auth.json")
	root, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil,
		"tokens": map[string]any{"access_token": access, "refresh_token": "r0", "account_id": "acct", "id_token": "i0"}})
	if err := os.WriteFile(path, root, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type seenReq struct {
	method, url, body string
	header            http.Header
}

// codexServer serves canned (status, body) replies in order, recording each request.
func codexServer(t *testing.T, replies ...[2]string) (string, *[]seenReq) {
	var mu sync.Mutex
	var seen []seenReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		i := len(seen)
		seen = append(seen, seenReq{r.Method, r.URL.RequestURI(), string(b), r.Header.Clone()})
		mu.Unlock()
		var status int
		fmt.Sscan(replies[i][0], &status)
		w.WriteHeader(status)
		io.WriteString(w, replies[i][1])
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &seen
}

func codexReq() core.ProviderRequest {
	return core.ProviderRequest{Model: "gpt-5.5", System: "sys", Messages: []core.Message{core.UserText("hi")},
		Tools: []core.ToolSpec{{Name: "list_dir", Description: "d", InputSchema: map[string]any{"type": "object"}}}, MaxTokens: 16}
}

// Event shapes captured from chatgpt.com/backend-api/codex/responses.
func codexSSE(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var v struct{ Type string }
		json.Unmarshal([]byte(e), &v)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", v.Type, e)
	}
	return b.String()
}

func TestCodexJWTClaims(t *testing.T) {
	tok := testJWT(1234, "acct_9")
	if exp, ok := jwtExp(tok); !ok || exp != 1234 {
		t.Fatal(exp, ok)
	}
	if a, ok := jwtAccount(tok); !ok || a != "acct_9" {
		t.Fatal(a, ok)
	}
	if _, ok := jwtExp("opaque"); ok {
		t.Fatal("opaque token has no exp")
	}
}

func TestCodexHistoryBecomesResponsesItems(t *testing.T) {
	history := []core.Message{
		core.UserText("list"),
		{Role: core.RoleAssistant, Content: []core.ContentBlock{core.TextBlock("ok"), core.ToolUseBlock("call_1", "list_dir", json.RawMessage(`{"path": "."}`))}},
		core.ToolResults([]core.ContentBlock{core.ToolResultBlock("call_1", "a.rs", false)}),
	}
	got, _ := json.Marshal(codexInput(history))
	want := `[{"content":[{"text":"list","type":"input_text"}],"role":"user","type":"message"},` +
		`{"content":[{"text":"ok","type":"output_text"}],"role":"assistant","type":"message"},` +
		`{"arguments":"{\"path\":\".\"}","call_id":"call_1","name":"list_dir","type":"function_call"},` +
		`{"call_id":"call_1","output":"a.rs","type":"function_call_output"}]`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCodexRequestBodyShape(t *testing.T) {
	b := NewCodex("/nonexistent", codexBaseURL, codexTokenURL).requestBody(codexReq())
	instr := b["instructions"].(string)
	if !strings.HasPrefix(instr, codexIdentity) || !strings.HasSuffix(instr, "sys") {
		t.Fatal(instr)
	}
	if b["stream"] != true || b["store"] != false || b["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatal(b)
	}
	// The backend 400s on max_output_tokens.
	if _, has := b["max_output_tokens"]; has {
		t.Fatal("max_output_tokens must not be sent")
	}
	tools := b["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "list_dir" || !reflect.DeepEqual(tools[1], map[string]any{"type": "web_search"}) {
		t.Fatal(tools)
	}
}

func TestCodexStreamAssemblesTextToolCallAndUsage(t *testing.T) {
	body := codexSSE(
		`{"type":"response.created"}`,
		`{"type":"response.output_text.delta","item_id":"m1","delta":"Let me "}`,
		`{"type":"response.output_text.delta","item_id":"m1","delta":"look."}`,
		`{"type":"response.output_item.done","item":{"type":"message","id":"m1","content":[{"type":"output_text","text":"Let me look."}]}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"{\"pa"}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc1","call_id":"call_9","name":"list_dir","arguments":"{\"path\":\"/tmp\"}"}}`,
		`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws1"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":43,"output_tokens":5}}}`,
	)
	base, seen := codexServer(t, [2]string{"200", body})
	p := NewCodex(codexAuthFile(t, testJWT(codexFar, "acct")), base, "http://unused")
	var deltas []string
	resp, err := p.CompleteStream(context.Background(), codexReq(), func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deltas, []string{"Let me ", "look."}) || resp.Message.Text() != "Let me look." {
		t.Fatal(deltas, resp.Message.Text())
	}
	if resp.StopReason != core.StopToolUse || resp.InputTokens != 43 || resp.OutputTokens != 5 {
		t.Fatal(resp.StopReason, resp.InputTokens, resp.OutputTokens)
	}
	calls := resp.Message.ToolUses()
	if len(calls) != 1 || calls[0].ID != "call_9" || calls[0].Name != "list_dir" || string(calls[0].Input) != `{"path":"/tmp"}` {
		t.Fatal(calls)
	}
	r := (*seen)[0]
	if r.method != "POST" || !strings.HasPrefix(r.url, "/responses?client_version=") ||
		r.header.Get("chatgpt-account-id") != "acct" || r.header.Get("openai-beta") != "responses=v1" {
		t.Fatal(r)
	}
}

func TestCodexFailedEventIsAnError(t *testing.T) {
	base, _ := codexServer(t, [2]string{"200", codexSSE(`{"type":"response.failed","response":{"error":{"message":"You have no credits remaining"}}}`)})
	p := NewCodex(codexAuthFile(t, testJWT(codexFar, "acct")), base, "http://unused")
	if _, err := p.Complete(context.Background(), codexReq()); err == nil || !strings.Contains(err.Error(), "no credits remaining") {
		t.Fatal(err)
	}
}

func TestCodexListModelsFiltersHiddenAndSortsByPriority(t *testing.T) {
	models := `{"models":[
		{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","priority":12,"context_window":272000},
		{"slug":"gpt-reserve","visibility":"hide","priority":3},
		{"slug":"gpt-6-astra","display_name":"GPT-6-Astra","visibility":"list","priority":1,"context_window":272000}]}`
	base, seen := codexServer(t, [2]string{"200", models})
	p := NewCodex(codexAuthFile(t, testJWT(codexFar, "acct")), base, "http://unused")
	m, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m[0].ID != "gpt-6-astra" || m[0].DisplayName != "GPT-6-Astra" || *m[0].MaxInputTokens != 272000 || m[1].ID != "gpt-5.5" {
		t.Fatal(m)
	}
	if r := (*seen)[0]; r.method != "GET" || !strings.HasPrefix(r.url, "/models?client_version=") {
		t.Fatal(r)
	}
}

func TestCodexExpiringTokenIsRefreshedAndWrittenBack(t *testing.T) {
	fresh := testJWT(codexFar, "acct_new")
	tokenURL, seen := codexServer(t, [2]string{"200", fmt.Sprintf(`{"access_token":%q,"refresh_token":"r1","id_token":"i1"}`, fresh)})
	base, apiSeen := codexServer(t, [2]string{"200", `{"models":[]}`})
	path := codexAuthFile(t, testJWT(1, "acct"))
	if _, err := NewCodex(path, base, tokenURL).ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b := (*seen)[0].body; !strings.Contains(b, `"grant_type":"refresh_token"`) || !strings.Contains(b, `"refresh_token":"r0"`) {
		t.Fatal(b)
	}
	if h := (*apiSeen)[0].header.Get("authorization"); h != "Bearer "+fresh {
		t.Fatal(h)
	}
	var saved struct {
		AuthMode string            `json:"auth_mode"`
		Tokens   map[string]string `json:"tokens"`
	}
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &saved)
	if saved.Tokens["access_token"] != fresh || saved.Tokens["refresh_token"] != "r1" || saved.Tokens["account_id"] != "acct_new" || saved.AuthMode != "chatgpt" {
		t.Fatal(string(data))
	}
}

func TestCodexUnauthorizedRefreshesOnceAndRetries(t *testing.T) {
	fresh := testJWT(codexFar, "acct")
	tokenURL, _ := codexServer(t, [2]string{"200", fmt.Sprintf(`{"access_token":%q}`, fresh)})
	base, seen := codexServer(t, [2]string{"401", `{"detail":"revoked"}`}, [2]string{"200", `{"models":[]}`})
	if _, err := NewCodex(codexAuthFile(t, testJWT(codexFar, "acct")), base, tokenURL).ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := (*seen)[1].header.Get("authorization"); h != "Bearer "+fresh {
		t.Fatal(h)
	}
}

func TestCodexErrorsAreReadable(t *testing.T) {
	base, _ := codexServer(t, [2]string{"429", `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`})
	_, err := NewCodex(codexAuthFile(t, testJWT(codexFar, "acct")), base, "http://unused").ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "usage limit reached; resets in ~2.0h") {
		t.Fatal(err)
	}
	_, err = NewCodex("/nonexistent/auth.json", codexBaseURL, codexTokenURL).ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "run `codex login`") {
		t.Fatal(err)
	}
}
