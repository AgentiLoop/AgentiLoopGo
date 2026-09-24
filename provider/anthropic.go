package provider

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
	"strings"
	"unicode"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicAPIVersion     = "2023-06-01"
	oauthPrefix             = "sk-ant-oat01-"
	oauthBeta               = "oauth-2025-04-20,prompt-caching-2024-07-31"
	// OAuth tokens (from `claude setup-token`) are gated at the API to requests
	// whose first system block is exactly this string.
	claudeCodeIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
)

type Anthropic struct {
	client     *http.Client
	credential string
	baseURL    string
}

// NewAnthropic accepts either a standard API key (sk-ant-api…) or a Claude Code
// OAuth token (sk-ant-oat01-…); the auth scheme is chosen automatically.
func NewAnthropic(credential string) *Anthropic {
	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		base = anthropicDefaultBaseURL
	}
	return &Anthropic{client: &http.Client{}, credential: sanitize(credential), baseURL: base}
}

// AnthropicFromEnv reads ANTHROPIC_API_KEY (API key or OAuth token), falling back to ANTHROPIC_OAUTH_TOKEN.
func AnthropicFromEnv() (*Anthropic, error) {
	key, ok := os.LookupEnv("ANTHROPIC_API_KEY")
	if !ok {
		key, ok = os.LookupEnv("ANTHROPIC_OAUTH_TOKEN")
	}
	if !ok {
		return nil, errors.New("ANTHROPIC_API_KEY is not set (API key or sk-ant-oat01- OAuth token)")
	}
	return NewAnthropic(key), nil
}

func (a *Anthropic) IsOAuth() bool { return strings.HasPrefix(a.credential, oauthPrefix) }

// sanitize strips whitespace/control chars a terminal paste may have wrapped into the token.
func sanitize(raw string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return -1
		}
		return r
	}, raw)
}

func (a *Anthropic) Name() string         { return "anthropic" }
func (a *Anthropic) DefaultModel() string { return "claude-sonnet-5" }

func (a *Anthropic) auth(r *http.Request, beta string) {
	r.Header.Set("anthropic-version", anthropicAPIVersion)
	if a.IsOAuth() {
		r.Header.Set("authorization", "Bearer "+a.credential)
		r.Header.Set("anthropic-beta", beta)
	} else {
		r.Header.Set("x-api-key", a.credential)
	}
}

type wireUsage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

type wireErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (a *Anthropic) sendMessages(ctx context.Context, req core.ProviderRequest, stream bool) (*http.Response, error) {
	type sys struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	system := []sys{}
	if a.IsOAuth() {
		system = append(system, sys{"text", claudeCodeIdentity})
	}
	system = append(system, sys{"text", req.System})
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"system":     system,
		"messages":   nonNilMessages(req.Messages),
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if stream {
		body["stream"] = true
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("content-type", "application/json")
	a.auth(hr, oauthBeta)
	resp, err := a.client.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("request to Anthropic failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		text, _ := io.ReadAll(resp.Body)
		var we struct{ Error wireErrorBody }
		if json.Unmarshal(text, &we) == nil && we.Error.Type != "" {
			return nil, fmt.Errorf("Anthropic %s (%s): %s", statusText(resp), we.Error.Type, we.Error.Message)
		}
		return nil, fmt.Errorf("Anthropic %s: %s", statusText(resp), text)
	}
	return resp, nil
}

// ListModels fetches GET /v1/models, newest first (the API sorts by created_at descending).
func (a *Anthropic) ListModels(ctx context.Context) ([]core.ModelInfo, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, err
	}
	a.auth(hr, "oauth-2025-04-20")
	resp, err := a.client.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("request to /v1/models failed: %w", err)
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("Anthropic %s: %s", statusText(resp), text)
	}
	var list struct {
		Data []core.ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(text, &list); err != nil {
		return nil, fmt.Errorf("decoding /v1/models: %w", err)
	}
	return list.Data, nil
}

func (a *Anthropic) Complete(ctx context.Context, req core.ProviderRequest) (core.ProviderResponse, error) {
	resp, err := a.sendMessages(ctx, req, false)
	if err != nil {
		return core.ProviderResponse{}, err
	}
	defer resp.Body.Close()
	var wire struct {
		Content    []core.ContentBlock `json:"content"`
		StopReason *string             `json:"stop_reason"`
		Usage      wireUsage           `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return core.ProviderResponse{}, fmt.Errorf("decoding Anthropic response: %w", err)
	}
	return core.ProviderResponse{
		Message:      core.Message{Role: core.RoleAssistant, Content: wire.Content},
		StopReason:   parseStopReason(wire.StopReason),
		InputTokens:  wire.Usage.InputTokens,
		OutputTokens: wire.Usage.OutputTokens,
	}, nil
}

// partial is a content block being assembled from stream deltas.
type partial struct {
	kind     string // "text", "tool_use", "skip"
	text     string
	id, name string
	json     strings.Builder
}

func (a *Anthropic) CompleteStream(ctx context.Context, req core.ProviderRequest, onText func(string)) (core.ProviderResponse, error) {
	resp, err := a.sendMessages(ctx, req, true)
	if err != nil {
		return core.ProviderResponse{}, err
	}
	defer resp.Body.Close()

	var blocks []*partial
	var stopReason *string
	var inTok, outTok uint64
	r := bufio.NewReader(resp.Body)
	for {
		line, rerr := r.ReadString('\n')
		// SSE frames are newline-delimited; the JSON payload sits on `data:` lines.
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n \t"), "data:"); ok {
			var ev struct {
				Type    string `json:"type"`
				Message struct {
					Usage wireUsage `json:"usage"`
				} `json:"message"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
				Index int `json:"index"`
				Delta struct {
					Type        string  `json:"type"`
					Text        string  `json:"text"`
					PartialJSON string  `json:"partial_json"`
					StopReason  *string `json:"stop_reason"`
				} `json:"delta"`
				Usage wireUsage     `json:"usage"`
				Error wireErrorBody `json:"error"`
			}
			if err := json.Unmarshal([]byte(strings.TrimLeft(data, " ")), &ev); err != nil {
				return core.ProviderResponse{}, fmt.Errorf("decoding stream event: %w", err)
			}
			switch ev.Type {
			case "message_start":
				inTok = ev.Message.Usage.InputTokens
			case "content_block_start":
				p := &partial{kind: "skip"}
				switch ev.ContentBlock.Type {
				case "text":
					p.kind = "text"
				case "tool_use":
					p.kind, p.id, p.name = "tool_use", ev.ContentBlock.ID, ev.ContentBlock.Name
				}
				blocks = append(blocks, p)
			case "content_block_delta":
				if ev.Index < 0 || ev.Index >= len(blocks) {
					break
				}
				b := blocks[ev.Index]
				switch {
				case b.kind == "text" && ev.Delta.Type == "text_delta":
					onText(ev.Delta.Text)
					b.text += ev.Delta.Text
				case b.kind == "tool_use" && ev.Delta.Type == "input_json_delta":
					b.json.WriteString(ev.Delta.PartialJSON)
				}
			case "message_delta":
				stopReason = ev.Delta.StopReason
				outTok = ev.Usage.OutputTokens
			case "error":
				return core.ProviderResponse{}, fmt.Errorf("Anthropic stream error (%s): %s", ev.Error.Type, ev.Error.Message)
			case "content_block_stop", "message_stop", "ping":
			default:
				return core.ProviderResponse{}, fmt.Errorf("decoding stream event: unknown variant `%s`", ev.Type)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return core.ProviderResponse{}, fmt.Errorf("reading Anthropic stream: %w", rerr)
		}
	}

	var content []core.ContentBlock
	for _, b := range blocks {
		switch b.kind {
		case "text":
			content = append(content, core.TextBlock(b.text))
		case "tool_use":
			raw := strings.TrimSpace(b.json.String())
			input := json.RawMessage("{}")
			if raw != "" {
				if !json.Valid([]byte(raw)) {
					return core.ProviderResponse{}, fmt.Errorf("decoding tool input for `%s`: invalid JSON", b.name)
				}
				input = json.RawMessage(raw)
			}
			content = append(content, core.ToolUseBlock(b.id, b.name, input))
		}
	}
	return core.ProviderResponse{
		Message:      core.Message{Role: core.RoleAssistant, Content: content},
		StopReason:   parseStopReason(stopReason),
		InputTokens:  inTok,
		OutputTokens: outTok,
	}, nil
}

func parseStopReason(raw *string) core.StopReason {
	if raw == nil {
		return core.StopOther
	}
	switch s := core.StopReason(*raw); s {
	case core.StopEndTurn, core.StopToolUse, core.StopMaxTokens, core.StopStopSequence:
		return s
	}
	return core.StopOther
}

// statusText renders like reqwest's StatusCode Display: "401 Unauthorized".
func statusText(resp *http.Response) string {
	return fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
}

func nonNilMessages(m []core.Message) []core.Message {
	if m == nil {
		return []core.Message{}
	}
	return m
}
