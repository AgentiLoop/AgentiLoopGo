package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

const (
	defaultMaxChars = 30_000
	maxMaxChars     = 100_000
	// maxBodyBytes is how much of a response is downloaded before giving up on the rest.
	maxBodyBytes = 2 * 1024 * 1024
	fetchTimeout = 30 * time.Second
)

var (
	reScript  = regexp.MustCompile(`(?is)<script\b.*?</script\s*>`)
	reStyle   = regexp.MustCompile(`(?is)<style\b.*?</style\s*>`)
	reComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBlock   = regexp.MustCompile(`(?i)</?(?:p|div|br|li|tr|h[1-6]|ul|ol|table|section|article|header|footer|pre|blockquote)\b[^>]*>`)
	reTag     = regexp.MustCompile(`<[^>]*>`)
	reNumeric = regexp.MustCompile(`&#([xX][0-9a-fA-F]+|[0-9]+);`)
	reSpaces  = regexp.MustCompile(`[ \t\x{a0}]+`)
)

// htmlToText gives readable text from HTML: drops scripts, styles and comments, turns block tags
// into line breaks, strips the remaining tags, decodes common entities and squeezes whitespace.
func htmlToText(html string) string {
	s := reScript.ReplaceAllString(html, "")
	s = reStyle.ReplaceAllString(s, "")
	s = reComment.ReplaceAllString(s, "")
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = reNumeric.ReplaceAllStringFunc(s, func(m string) string {
		n := reNumeric.FindStringSubmatch(m)[1]
		var code uint64
		var err error
		if n[0] == 'x' || n[0] == 'X' {
			code, err = strconv.ParseUint(n[1:], 16, 32)
		} else {
			code, err = strconv.ParseUint(n, 10, 32)
		}
		if err != nil || !utf8.ValidRune(rune(code)) {
			return m
		}
		return string(rune(code))
	})
	s = strings.NewReplacer("&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&apos;", "'", "&amp;", "&").Replace(s)
	var out []string
	for _, line := range splitLines(s) {
		line = strings.TrimSpace(reSpaces.ReplaceAllString(line, " "))
		if line == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

type WebFetch struct{}

func (WebFetch) Name() string { return "web_fetch" }
func (WebFetch) Description() string {
	return "Fetch an http(s) URL and return its text: HTML pages are converted to plain text, text, JSON and XML " +
		"are returned as they are. Follows redirects. Use it to read documentation or an API response. " +
		"Binary content is refused. Long pages are cut at `max_chars`."
}
func (WebFetch) InputSchema() any {
	return schema(map[string]any{
		"url":       map[string]any{"type": "string", "description": "http:// or https:// URL"},
		"max_chars": map[string]any{"type": "integer", "description": "Maximum characters to return", "default": 30000},
	}, "url")
}

// IsMutating: makes a network request on the user's behalf, so it asks first like the other non-local tools.
func (WebFetch) IsMutating() bool { return true }
func (WebFetch) Call(ctx context.Context, _ core.ToolContext, input json.RawMessage) (string, error) {
	var a struct {
		URL      string `json:"url"`
		MaxChars *int   `json:"max_chars"`
	}
	if err := parse(input, &a, "url"); err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil {
		return "", core.InvalidInput("bad url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", core.InvalidInput("only http and https URLs are supported, not %q", u.Scheme)
	}
	limit := defaultMaxChars
	if a.MaxChars != nil {
		limit = *a.MaxChars
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxMaxChars {
		limit = maxMaxChars
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", core.Failed("%v", err)
	}
	req.Header.Set("User-Agent", "agentiloop")
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return "", core.Failed("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", core.Failed("HTTP %s for %s", resp.Status, u)
	}
	ctype := strings.ToLower(resp.Header.Get("Content-Type"))
	textual := ctype == ""
	for _, t := range []string{"text/", "json", "xml", "javascript", "yaml", "x-sh", "markdown"} {
		textual = textual || strings.Contains(ctype, t)
	}
	if !textual {
		return "", core.Failed("unsupported content type %s; web_fetch only reads text", ctype)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return "", core.Failed("download failed: %v", err)
	}
	bodyCut := len(body) > maxBodyBytes
	if bodyCut {
		body = body[:maxBodyBytes]
	}
	raw := strings.ToValidUTF8(string(body), "\uFFFD")
	isHTML := strings.Contains(ctype, "html") ||
		(ctype == "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "<!doctype html"))
	text := raw
	if isHTML {
		text = htmlToText(raw)
	}
	over := utf8.RuneCountInString(text) > limit
	if over {
		text = string([]rune(text)[:limit])
	}
	if over || bodyCut {
		text += fmt.Sprintf("\n…[truncated at %d characters]", limit)
	}
	return fmt.Sprintf("%s (%s)\n\n%s", u, resp.Status, text), nil
}
