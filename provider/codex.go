package provider

// Codex: your ChatGPT subscription via the OAuth tokens `codex login` writes to
// ~/.codex/auth.json (or $CODEX_HOME/auth.json), against
// chatgpt.com/backend-api/codex — the Responses API the official Codex CLI
// uses. Streaming only; tokens refresh automatically and are written back so
// the Codex CLI stays signed in too.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

const (
	codexBaseURL  = "https://chatgpt.com/backend-api/codex"
	codexTokenURL = "https://auth.openai.com/oauth/token"
	// codexClientID is OpenAI's public (PKCE) client id for the Codex CLI.
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// codexClientVersion: the backend wants a Codex CLI version on every call (/models 400s without it).
	codexClientVersion = "0.154.0"
	// codexIdentity is the line the Codex CLI opens its instructions with.
	codexIdentity = "You are Codex, based on GPT-5. You are running as a coding agent in the Codex CLI on a user's computer."
)

type Codex struct {
	client   *http.Client
	authPath string
	baseURL  string
	tokenURL string
	effort   string
}

type codexTokens struct {
	access, refresh, account string
}

func NewCodex(authPath, baseURL, tokenURL string) *Codex {
	return &Codex{
		client:   &http.Client{},
		authPath: authPath,
		baseURL:  strings.TrimRight(baseURL, "/"),
		tokenURL: tokenURL,
		effort:   "medium",
	}
}

// CodexFromEnv uses $CODEX_HOME/auth.json (default ~/.codex/auth.json) and
// reasoning effort from CODEX_REASONING_EFFORT (low | medium | high | xhigh, default medium).
func CodexFromEnv() (*Codex, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("no home directory")
		}
		home = filepath.Join(h, ".codex")
	}
	c := NewCodex(filepath.Join(home, "auth.json"), codexBaseURL, codexTokenURL)
	if e := strings.TrimSpace(os.Getenv("CODEX_REASONING_EFFORT")); e != "" {
		c.effort = strings.ToLower(e)
	}
	if _, err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Codex) Name() string { return "codex" }

// DefaultModel is empty: the model set depends on the ChatGPT plan, so ask /models.
func (c *Codex) DefaultModel() string { return "" }

func (c *Codex) load() (codexTokens, error) {
	missing := fmt.Errorf("%s not found or not signed in — run `codex login` first", c.authPath)
	data, err := os.ReadFile(c.authPath)
	if err != nil {
		return codexTokens{}, missing
	}
	var root struct {
		Tokens struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &root) != nil || root.Tokens.Access == "" || root.Tokens.Refresh == "" || root.Tokens.Account == "" {
		return codexTokens{}, missing
	}
	return codexTokens{root.Tokens.Access, root.Tokens.Refresh, root.Tokens.Account}, nil
}

// tokens returns the current tokens, refreshed first when the access token expires within 5 minutes.
func (c *Codex) tokens(ctx context.Context) (codexTokens, error) {
	t, err := c.load()
	if err != nil {
		return t, err
	}
	if exp, ok := jwtExp(t.access); ok && exp < time.Now().Unix()+300 {
		return c.refresh(ctx, t)
	}
	return t, nil
}

// refresh trades the refresh token for new tokens and writes them back to
// auth.json, keeping every other key the Codex CLI stores there.
func (c *Codex) refresh(ctx context.Context, t codexTokens) (codexTokens, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": t.refresh,
		"client_id":     codexClientID,
		"scope":         "openid profile email offline_access",
	})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return t, err
	}
	hr.Header.Set("content-type", "application/json")
	resp, err := c.client.Do(hr)
	if err != nil {
		return t, fmt.Errorf("refreshing Codex token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return t, fmt.Errorf("codex token refresh failed (%s) — run `codex login` again", resp.Status)
	}
	var v struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return t, fmt.Errorf("decoding Codex token refresh: %w", err)
	}
	if v.Access == "" {
		return t, errors.New("token refresh returned no access_token")
	}
	out := codexTokens{access: v.Access, refresh: t.refresh, account: t.account}
	if v.Refresh != "" {
		out.refresh = v.Refresh
	}
	if a, ok := jwtAccount(v.Access); ok {
		out.account = a
	}

	root := map[string]any{}
	if data, err := os.ReadFile(c.authPath); err == nil {
		_ = json.Unmarshal(data, &root)
	}
	tokens, _ := root["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"], tokens["refresh_token"], tokens["account_id"] = out.access, out.refresh, out.account
	if v.ID != "" {
		tokens["id_token"] = v.ID
	}
	root["tokens"] = tokens
	data, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(c.authPath, data, 0o600); err != nil {
		return out, fmt.Errorf("writing %s: %w", c.authPath, err)
	}
	return out, nil
}

// send issues the request with the current token; on 401 it refreshes once and retries.
func (c *Codex) send(ctx context.Context, path string, body []byte) (*http.Response, error) {
	url := c.baseURL + path + "?client_version=" + codexClientVersion
	t, err := c.tokens(ctx)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		method := http.MethodGet
		var rd io.Reader
		if body != nil {
			method, rd = http.MethodPost, bytes.NewReader(body)
		}
		hr, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, err
		}
		hr.Header.Set("authorization", "Bearer "+t.access)
		hr.Header.Set("chatgpt-account-id", t.account)
		hr.Header.Set("openai-beta", "responses=v1")
		hr.Header.Set("user-agent", "codex_cli_rs/"+codexClientVersion)
		if body != nil {
			hr.Header.Set("content-type", "application/json")
			hr.Header.Set("accept", "text/event-stream")
		}
		resp, err := c.client.Do(hr)
		if err != nil {
			return nil, fmt.Errorf("request to Codex failed: %w", err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return resp, nil
		}
		text, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 401 && attempt == 0 {
			if t, err = c.refresh(ctx, t); err != nil {
				return nil, err
			}
			continue
		}
		return nil, codexHTTPError(resp.StatusCode, text)
	}
}

func (c *Codex) requestBody(req core.ProviderRequest) map[string]any {
	tools := make([]any, 0, len(req.Tools)+1)
	for _, t := range req.Tools {
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.InputSchema})
	}
	// Hosted search: runs on OpenAI's side, billed to the subscription.
	tools = append(tools, map[string]any{"type": "web_search"})
	return map[string]any{
		"model":        req.Model,
		"instructions": codexIdentity + "\n\n" + req.System,
		"input":        codexInput(req.Messages),
		"tools":        tools,
		"store":        false,
		"stream":       true,
		"reasoning":    map[string]any{"effort": c.effort},
	}
}

func jwtClaims(jwt string) (map[string]any, bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, false
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	return claims, json.Unmarshal(data, &claims) == nil
}

// jwtExp is the `exp` claim of a JWT (seconds since epoch).
func jwtExp(jwt string) (int64, bool) {
	c, ok := jwtClaims(jwt)
	exp, isNum := c["exp"].(float64)
	return int64(exp), ok && isNum
}

func jwtAccount(jwt string) (string, bool) {
	c, _ := jwtClaims(jwt)
	auth, _ := c["https://api.openai.com/auth"].(map[string]any)
	a, ok := auth["chatgpt_account_id"].(string)
	return a, ok && a != ""
}

func codexHTTPError(status int, text []byte) error {
	var v struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Resets  uint64 `json:"resets_in_seconds"`
		} `json:"error"`
		Type    string `json:"type"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Resets  uint64 `json:"resets_in_seconds"`
	}
	_ = json.Unmarshal(text, &v)
	typ, msg, resets := v.Error.Type, v.Error.Message, v.Error.Resets
	if typ == "" && msg == "" {
		typ, msg, resets = v.Type, v.Message, v.Resets
	}
	if typ == "usage_limit_reached" {
		return fmt.Errorf("codex %d: ChatGPT plan usage limit reached; resets in ~%.1fh", status, float64(resets)/3600)
	}
	if msg == "" {
		msg = v.Detail
	}
	if msg == "" {
		msg = string(text)
	}
	hint := ""
	if status == 401 {
		hint = " — run `codex login` again"
	}
	return fmt.Errorf("codex %d: %s%s", status, msg, hint)
}

// codexInput turns the conversation into Responses input items. Tool calls and
// results become function_call / function_call_output items (no server-side state: store:false).
func codexInput(history []core.Message) []any {
	out := []any{}
	for _, m := range history {
		role, kind := "user", "input_text"
		if m.Role == core.RoleAssistant {
			role, kind = "assistant", "output_text"
		}
		for _, b := range m.Content {
			switch b.Type {
			case core.BlockText:
				out = append(out, map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": b.Text}}})
			case core.BlockToolUse:
				out = append(out, map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": compactJSON(b.Input)})
			case core.BlockToolResult:
				out = append(out, map[string]any{"type": "function_call_output", "call_id": b.ToolUseID, "output": b.Content})
			}
		}
	}
	return out
}

// ListModels reads /models: only visibility "list" entries are user-selectable;
// the server's priority puts the newest first.
func (c *Codex) ListModels(ctx context.Context) ([]core.ModelInfo, error) {
	resp, err := c.send(ctx, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v struct {
		Models *[]struct {
			Slug          string  `json:"slug"`
			DisplayName   string  `json:"display_name"`
			Visibility    string  `json:"visibility"`
			Priority      *int64  `json:"priority"`
			ContextWindow *uint64 `json:"context_window"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("decoding Codex model list: %w", err)
	}
	if v.Models == nil {
		return nil, errors.New("Codex /models reply has no `models` array")
	}
	listed := (*v.Models)[:0:0]
	for _, m := range *v.Models {
		if m.Visibility == "list" && m.Slug != "" {
			listed = append(listed, m)
		}
	}
	prio := func(p *int64) int64 {
		if p == nil {
			return 1<<63 - 1
		}
		return *p
	}
	sort.SliceStable(listed, func(i, j int) bool { return prio(listed[i].Priority) < prio(listed[j].Priority) })
	out := make([]core.ModelInfo, 0, len(listed))
	for _, m := range listed {
		name := m.DisplayName
		if name == "" {
			name = m.Slug
		}
		out = append(out, core.ModelInfo{ID: m.Slug, DisplayName: name, MaxInputTokens: m.ContextWindow})
	}
	return out, nil
}

func (c *Codex) Complete(ctx context.Context, req core.ProviderRequest) (core.ProviderResponse, error) {
	return c.CompleteStream(ctx, req, func(string) {})
}

func (c *Codex) CompleteStream(ctx context.Context, req core.ProviderRequest, onText func(string)) (core.ProviderResponse, error) {
	body, err := json.Marshal(c.requestBody(req))
	if err != nil {
		return core.ProviderResponse{}, err
	}
	resp, err := c.send(ctx, "/responses", body)
	if err != nil {
		return core.ProviderResponse{}, err
	}
	defer resp.Body.Close()
	return readResponsesStream(resp, onText, "codex")
}

// readResponsesStream parses a Responses API SSE stream (Codex backend or
// api.openai.com /v1/responses) into one assistant turn. who prefixes stream errors.
func readResponsesStream(resp *http.Response, onText func(string), who string) (core.ProviderResponse, error) {
	out := core.ProviderResponse{Message: core.Message{Role: core.RoleAssistant}, StopReason: core.StopEndTurn}
	r := bufio.NewReader(resp.Body)
	for {
		line, rerr := r.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n \t"), "data:"); ok {
			var ev struct {
				Type    string `json:"type"`
				Delta   string `json:"delta"`
				Message string `json:"message"`
				Error   struct {
					Message string `json:"message"`
				} `json:"error"`
				Item struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
					Content   []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"item"`
				Response struct {
					Usage struct {
						Input  uint64 `json:"input_tokens"`
						Output uint64 `json:"output_tokens"`
					} `json:"usage"`
					Incomplete struct {
						Reason string `json:"reason"`
					} `json:"incomplete_details"`
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(strings.TrimLeft(data, " ")), &ev) == nil {
				switch ev.Type {
				case "response.output_text.delta":
					if ev.Delta != "" {
						onText(ev.Delta)
					}
				// Completed items carry the full text / call arguments.
				case "response.output_item.done":
					switch ev.Item.Type {
					case "message":
						var text strings.Builder
						for _, c := range ev.Item.Content {
							text.WriteString(c.Text)
						}
						if text.Len() > 0 {
							out.Message.Content = append(out.Message.Content, core.TextBlock(text.String()))
						}
					case "function_call":
						input, err := parseToolArgs(ev.Item.Name, ev.Item.Arguments)
						if err != nil {
							return core.ProviderResponse{}, err
						}
						id := ev.Item.CallID
						if id == "" {
							id = ev.Item.ID
						}
						out.Message.Content = append(out.Message.Content, core.ToolUseBlock(id, ev.Item.Name, input))
					}
				case "response.completed", "response.incomplete":
					out.InputTokens, out.OutputTokens = ev.Response.Usage.Input, ev.Response.Usage.Output
					if ev.Type == "response.incomplete" {
						out.StopReason = core.StopOther
						if ev.Response.Incomplete.Reason == "max_output_tokens" {
							out.StopReason = core.StopMaxTokens
						}
					}
				case "response.failed":
					msg := ev.Response.Error.Message
					if msg == "" {
						msg = "response.failed"
					}
					return core.ProviderResponse{}, fmt.Errorf("%s: %s", who, msg)
				case "error":
					msg := ev.Message
					if msg == "" {
						msg = ev.Error.Message
					}
					if msg == "" {
						msg = "stream error"
					}
					return core.ProviderResponse{}, fmt.Errorf("%s: %s", who, msg)
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return core.ProviderResponse{}, fmt.Errorf("reading %s stream: %w", who, rerr)
		}
	}
	for _, b := range out.Message.Content {
		if b.Type == core.BlockToolUse {
			out.StopReason = core.StopToolUse
		}
	}
	return out, nil
}
