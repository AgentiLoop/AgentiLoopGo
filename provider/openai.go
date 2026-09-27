package provider

// OpenAI-compatible Chat Completions backend. Works with OpenAI itself and
// anything that speaks the same wire format (Ollama /v1, LM Studio, Groq,
// OpenRouter, Together, DeepSeek, vLLM, …) — point OPENAI_BASE_URL at it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

const openAIDefaultBaseURL = "https://api.openai.com/v1"

// openAIModelDocsURL: OpenAI's per-model doc pages (<id>.md), the only place
// OpenAI publishes context windows and output caps — /models doesn't carry them.
const openAIModelDocsURL = "https://developers.openai.com/api/docs/models"

type OpenAI struct {
	client       *http.Client
	apiKey       string
	baseURL      string
	modelDocsURL string
	name         string
	defaultModel string
}

func NewOpenAI(apiKey, baseURL string) *OpenAI {
	return &OpenAI{
		client:       &http.Client{},
		apiKey:       strings.TrimSpace(apiKey),
		baseURL:      strings.TrimRight(baseURL, "/"),
		modelDocsURL: openAIModelDocsURL,
		name:         "openai",
		defaultModel: "gpt-4o-mini",
	}
}

// WithIdentity rebrands this backend for a server that speaks the OpenAI wire
// format under its own name (e.g. omlx), so settings.json and the status line
// key on that name. An empty defaultModel means "ask /models".
func (o *OpenAI) WithIdentity(name, defaultModel string) *OpenAI {
	o.name, o.defaultModel = name, defaultModel
	return o
}

// OpenAIFromEnv reads OPENAI_API_KEY and OPENAI_BASE_URL (default https://api.openai.com/v1).
// Local servers such as Ollama ignore the key, so it may be omitted when a
// custom base URL is set.
func OpenAIFromEnv() (*OpenAI, error) {
	base, ok := os.LookupEnv("OPENAI_BASE_URL")
	if !ok {
		base = openAIDefaultBaseURL
	}
	key, ok := os.LookupEnv("OPENAI_API_KEY")
	if !ok {
		if base == openAIDefaultBaseURL {
			return nil, errors.New("OPENAI_API_KEY is not set (or set OPENAI_BASE_URL for a local server)")
		}
		key = "none"
	}
	return NewOpenAI(key, base), nil
}

func (o *OpenAI) Name() string         { return o.name }
func (o *OpenAI) DefaultModel() string { return o.defaultModel }

// httpError is the error for a non-2xx reply, named after this backend. 401/403
// say outright that the API key is missing or wrong and where to set it.
func (o *OpenAI) httpError(resp *http.Response, text []byte) error {
	var we struct {
		Error *struct {
			Type    *string `json:"type"`
			Message *string `json:"message"`
		} `json:"error"`
	}
	detail := ": " + string(text)
	if json.Unmarshal(text, &we) == nil && we.Error != nil && we.Error.Message != nil {
		kind := ""
		if we.Error.Type != nil {
			kind = *we.Error.Type
		}
		detail = fmt.Sprintf("(%s): %s", kind, *we.Error.Message)
	}
	msg := fmt.Sprintf("%s %s %s", o.name, statusText(resp), detail)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		hint := "set OPENAI_API_KEY"
		if o.name == "omlx" {
			hint = "set OMLX_API_KEY or auth.api_key in ~/.omlx/settings.json"
		}
		msg += " — API key missing or invalid; " + hint
	}
	return errors.New(msg)
}

// ---- wire types -------------------------------------------------------------

type wireToolCallFn struct {
	Name string `json:"name"`
	// Arguments is JSON-encoded (a string on the wire, not an object).
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function wireToolCallFn `json:"function"`
}

// wireMessage: `content` is always present (null for tool-call-only assistant turns).
type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// toWireMessages flattens our Anthropic-shaped history into OpenAI's
// role-per-message form: tool results become individual `tool` messages,
// assistant tool uses become `tool_calls` with stringified arguments.
func toWireMessages(system string, history []core.Message) []wireMessage {
	str := func(s string) *string { return &s }
	out := make([]wireMessage, 0, len(history)+1)
	if system != "" {
		out = append(out, wireMessage{Role: "system", Content: str(system)})
	}
	for _, m := range history {
		if m.Role == core.RoleUser {
			for _, b := range m.Content {
				switch b.Type {
				case core.BlockText:
					out = append(out, wireMessage{Role: "user", Content: str(b.Text)})
				case core.BlockToolResult:
					out = append(out, wireMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: str(b.Content)})
				}
			}
			continue
		}
		wm := wireMessage{Role: "assistant"}
		if t := m.Text(); t != "" {
			wm.Content = str(t)
		}
		for _, u := range m.ToolUses() {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{u.ID, "function", wireToolCallFn{u.Name, compactJSON(u.Input)}})
		}
		out = append(out, wm)
	}
	return out
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if len(raw) == 0 || json.Compact(&buf, raw) != nil {
		return "{}"
	}
	return buf.String()
}

type wireUsageOAI struct {
	PromptTokens     uint64 `json:"prompt_tokens"`
	CompletionTokens uint64 `json:"completion_tokens"`
}

func parseFinish(raw *string, hasToolCalls bool) core.StopReason {
	r := ""
	if raw != nil {
		r = *raw
	}
	switch {
	case r == "tool_calls" || r == "function_call":
		return core.StopToolUse
	case r == "stop" && hasToolCalls: // some servers report "stop" alongside tool calls
		return core.StopToolUse
	case r == "stop":
		return core.StopEndTurn
	case r == "length":
		return core.StopMaxTokens
	case hasToolCalls:
		return core.StopToolUse
	}
	return core.StopOther
}

func parseToolArgs(name, raw string) (json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("decoding tool arguments for `%s`: %s", name, raw)
	}
	return json.RawMessage(raw), nil
}

func buildContent(text string, calls []wireToolCall) ([]core.ContentBlock, error) {
	var content []core.ContentBlock
	if text != "" {
		content = append(content, core.TextBlock(text))
	}
	for _, c := range calls {
		input, err := parseToolArgs(c.Function.Name, c.Function.Arguments)
		if err != nil {
			return nil, err
		}
		content = append(content, core.ToolUseBlock(c.ID, c.Function.Name, input))
	}
	return content, nil
}

func (o *OpenAI) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("authorization", "Bearer "+o.apiKey)
	if body != nil {
		hr.Header.Set("content-type", "application/json")
	}
	return hr, nil
}

// wireRequest builds the chat/completions body. api.openai.com wants
// max_completion_tokens (its newer models reject max_tokens); every other
// compatible server gets max_tokens.
func (o *OpenAI) wireRequest(req core.ProviderRequest, stream bool) map[string]any {
	type fn struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	}
	type tool struct {
		Type     string `json:"type"`
		Function fn     `json:"function"`
	}
	capField := "max_tokens"
	if strings.HasPrefix(o.baseURL, "https://api.openai.com") {
		capField = "max_completion_tokens"
	}
	body := map[string]any{
		"model":    req.Model,
		capField:   req.MaxTokens,
		"messages": toWireMessages(req.System, req.Messages),
	}
	if len(req.Tools) > 0 {
		var tools []tool
		for _, t := range req.Tools {
			tools = append(tools, tool{"function", fn{t.Name, t.Description, t.InputSchema}})
		}
		body["tools"] = tools
	}
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	return body
}

func (o *OpenAI) sendChat(ctx context.Context, req core.ProviderRequest, stream bool) (*http.Response, error) {
	data, err := json.Marshal(o.wireRequest(req, stream))
	if err != nil {
		return nil, err
	}
	hr, err := o.newRequest(ctx, http.MethodPost, "/chat/completions", data)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", o.baseURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		text, _ := io.ReadAll(resp.Body)
		return nil, o.httpError(resp, text)
	}
	return resp, nil
}

// ListModels calls GET /models; sorted newest first by `created` when present.
func (o *OpenAI) ListModels(ctx context.Context) ([]core.ModelInfo, error) {
	hr, err := o.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("request to /models failed: %w", err)
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, o.httpError(resp, text)
	}
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			Created uint64 `json:"created"`
			// OpenRouter: context window.
			ContextLength *uint64 `json:"context_length"`
			// vLLM and oMLX: context window.
			MaxModelLen *uint64 `json:"max_model_len"`
			// OpenRouter: output cap lives under top_provider.
			TopProvider *struct {
				MaxCompletionTokens *int `json:"max_completion_tokens"`
			} `json:"top_provider"`
		} `json:"data"`
	}
	if err := json.Unmarshal(text, &list); err != nil {
		return nil, fmt.Errorf("decoding /models: %w", err)
	}
	sort.SliceStable(list.Data, func(i, j int) bool {
		a, b := list.Data[i], list.Data[j]
		if a.Created != b.Created {
			return a.Created > b.Created
		}
		return a.ID < b.ID
	})
	out := make([]core.ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		info := core.ModelInfo{ID: m.ID, DisplayName: m.ID, MaxInputTokens: m.ContextLength}
		if info.MaxInputTokens == nil {
			info.MaxInputTokens = m.MaxModelLen
		}
		if m.TopProvider != nil {
			info.MaxTokens = m.TopProvider.MaxCompletionTokens
		}
		out = append(out, info)
	}
	return out, nil
}

// ModelInfo scans /models (OpenRouter, vLLM and oMLX report limits there),
// then asks Ollama's /api/show for the context window when the catalog didn't
// carry one, and finally fills in OpenAI's published limits.
func (o *OpenAI) ModelInfo(ctx context.Context, id string) (*core.ModelInfo, error) {
	models, err := o.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	var info *core.ModelInfo
	for i := range models {
		if models[i].ID == id {
			info = &models[i]
			break
		}
	}
	if info == nil {
		return nil, nil
	}
	if info.MaxInputTokens == nil {
		info.MaxInputTokens = o.ollamaContextLength(ctx, id)
	}
	if info.MaxInputTokens == nil || info.MaxTokens == nil {
		if input, output, ok := o.openAIPublishedLimits(ctx, id); ok {
			if info.MaxInputTokens == nil {
				info.MaxInputTokens = &input
			}
			if info.MaxTokens == nil {
				info.MaxTokens = &output
			}
		}
	}
	return info, nil
}

// ollamaContextLength: Ollama (local or ollama.com) mounts its native API
// beside /v1: POST /api/show reports the model's context window under
// model_info["<family>.context_length"], capped by a num_ctx Modelfile
// parameter when set. Any other server 404s → nil.
func (o *OpenAI) ollamaContextLength(ctx context.Context, id string) *uint64 {
	root, ok := strings.CutSuffix(o.baseURL, "/v1")
	if !ok {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"model": id})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/api/show", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	hr.Header.Set("authorization", "Bearer "+o.apiKey)
	hr.Header.Set("content-type", "application/json")
	resp, err := o.client.Do(hr)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil
	}
	var show struct {
		ModelInfo  map[string]json.RawMessage `json:"model_info"`
		Parameters string                     `json:"parameters"`
	}
	if json.NewDecoder(resp.Body).Decode(&show) != nil {
		return nil
	}
	var modelMax, numCtx *uint64
	for k, v := range show.ModelInfo {
		var n uint64
		if strings.HasSuffix(k, ".context_length") && json.Unmarshal(v, &n) == nil {
			modelMax = &n
			break
		}
	}
	for _, line := range strings.Split(show.Parameters, "\n") {
		words := strings.Fields(line)
		if len(words) >= 2 && words[0] == "num_ctx" {
			if n, err := strconv.ParseUint(words[1], 10, 64); err == nil {
				numCtx = &n
			}
			break
		}
	}
	if modelMax != nil && numCtx != nil {
		n := min(*modelMax, *numCtx)
		return &n
	}
	if modelMax != nil {
		return modelMax
	}
	return numCtx
}

// openAIPublishedLimits fetches OpenAI's published (context window, max
// output) for one of its own models from the model's doc page. Dated
// snapshots (o1-mini-2024-09-12) have no page of their own, so the date
// suffix is dropped and the family page is used instead.
func (o *OpenAI) openAIPublishedLimits(ctx context.Context, id string) (input uint64, output int, ok bool) {
	if !looksLikeOpenAIModel(id) {
		return 0, 0, false
	}
	pages := []string{id}
	if n := len(id) - 11; n > 0 && id[n] == '-' && isSnapshotDate(id[n+1:]) {
		pages = append(pages, id[:n])
	}
	for _, page := range pages {
		hr, err := http.NewRequestWithContext(ctx, http.MethodGet, o.modelDocsURL+"/"+page+".md", nil)
		if err != nil {
			return 0, 0, false
		}
		resp, err := o.client.Do(hr)
		if err != nil {
			return 0, 0, false
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			continue
		}
		if err != nil {
			return 0, 0, false
		}
		return parseModelDoc(string(body))
	}
	return 0, 0, false
}

// parseModelDoc pulls (context window, max output) out of an OpenAI model doc
// page (developers.openai.com/api/docs/models/<id>.md). The "Model details"
// list carries lines like "- 400,000 context window",
// "- Maximum input tokens: 272,000" and "- 128,000 max output tokens"; the
// explicit input cap wins over the context window when both appear.
func parseModelDoc(md string) (input uint64, output int, ok bool) {
	number := func(label string) (uint64, bool) {
		for _, line := range strings.Split(md, "\n") {
			item, isItem := strings.CutPrefix(strings.TrimLeft(line, " \t"), "- ")
			if !isItem || !strings.Contains(item, label) {
				continue
			}
			for _, w := range strings.FieldsFunc(item, func(c rune) bool { return (c < '0' || c > '9') && c != ',' }) {
				if strings.ContainsAny(w, "0123456789") {
					n, err := strconv.ParseUint(strings.ReplaceAll(w, ",", ""), 10, 64)
					return n, err == nil
				}
			}
			return 0, false
		}
		return 0, false
	}
	input, ok = number("Maximum input tokens")
	if !ok {
		input, ok = number("context window")
	}
	if !ok {
		return 0, 0, false
	}
	out, ok := number("max output tokens")
	if !ok || out > uint64(^uint32(0)) {
		return 0, 0, false
	}
	return input, int(out), true
}

// looksLikeOpenAIModel: gpt-*, chatgpt-*, o1…o4.
func looksLikeOpenAIModel(id string) bool {
	return strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "chatgpt-") ||
		(len(id) > 1 && id[0] == 'o' && id[1] >= '0' && id[1] <= '9')
}

// isSnapshotDate: YYYY-MM-DD as used in OpenAI snapshot ids.
func isSnapshotDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			if c != '-' {
				return false
			}
		} else if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (o *OpenAI) Complete(ctx context.Context, req core.ProviderRequest) (core.ProviderResponse, error) {
	resp, err := o.sendChat(ctx, req, false)
	if err != nil {
		return core.ProviderResponse{}, err
	}
	defer resp.Body.Close()
	var wire struct {
		Choices []struct {
			Message struct {
				Content   *string        `json:"content"`
				ToolCalls []wireToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *wireUsageOAI `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return core.ProviderResponse{}, fmt.Errorf("decoding OpenAI response: %w", err)
	}
	if len(wire.Choices) == 0 {
		return core.ProviderResponse{}, errors.New("OpenAI response had no choices")
	}
	ch := wire.Choices[0]
	text := ""
	if ch.Message.Content != nil {
		text = *ch.Message.Content
	}
	content, err := buildContent(text, ch.Message.ToolCalls)
	if err != nil {
		return core.ProviderResponse{}, err
	}
	var usage wireUsageOAI
	if wire.Usage != nil {
		usage = *wire.Usage
	}
	return core.ProviderResponse{
		Message:      core.Message{Role: core.RoleAssistant, Content: content},
		StopReason:   parseFinish(ch.FinishReason, len(ch.Message.ToolCalls) > 0),
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}, nil
}

func (o *OpenAI) CompleteStream(ctx context.Context, req core.ProviderRequest, onText func(string)) (core.ProviderResponse, error) {
	resp, err := o.sendChat(ctx, req, true)
	if err != nil {
		return core.ProviderResponse{}, err
	}
	defer resp.Body.Close()

	var text strings.Builder
	// Tool calls arrive as deltas keyed by `index`; arguments are concatenated.
	var calls []wireToolCall
	var finish *string
	var usage wireUsageOAI
	r := bufio.NewReader(resp.Body)
	for {
		line, rerr := r.ReadString('\n')
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n \t"), "data:")
		if ok {
			data = strings.TrimLeft(data, " ")
			if data == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content   *string `json:"content"`
						ToolCalls []struct {
							Index    int     `json:"index"`
							ID       *string `json:"id"`
							Function *struct {
								Name      *string `json:"name"`
								Arguments *string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *wireUsageOAI `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return core.ProviderResponse{}, fmt.Errorf("decoding stream chunk: %s: %w", data, err)
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			for _, ch := range chunk.Choices {
				if c := ch.Delta.Content; c != nil && *c != "" {
					onText(*c)
					text.WriteString(*c)
				}
				for _, tc := range ch.Delta.ToolCalls {
					if tc.Index < 0 {
						continue
					}
					for len(calls) <= tc.Index {
						calls = append(calls, wireToolCall{Type: "function"})
					}
					slot := &calls[tc.Index]
					if tc.ID != nil && *tc.ID != "" {
						slot.ID = *tc.ID
					}
					if f := tc.Function; f != nil {
						if f.Name != nil && *f.Name != "" {
							slot.Function.Name = *f.Name
						}
						if f.Arguments != nil {
							slot.Function.Arguments += *f.Arguments
						}
					}
				}
				if ch.FinishReason != nil {
					finish = ch.FinishReason
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return core.ProviderResponse{}, fmt.Errorf("reading OpenAI stream: %w", rerr)
		}
	}
	// Servers that omit ids would break tool_result pairing; synthesize one.
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d", i)
		}
	}
	content, err := buildContent(text.String(), calls)
	if err != nil {
		return core.ProviderResponse{}, err
	}
	return core.ProviderResponse{
		Message:      core.Message{Role: core.RoleAssistant, Content: content},
		StopReason:   parseFinish(finish, len(calls) > 0),
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}, nil
}
