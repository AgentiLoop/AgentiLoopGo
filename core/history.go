package core

// /history [text]: search the saved prompts (the ↑ history shared by the REPL and the TUI).

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HistoryShown is the most matches /history prints (the newest ones).
const HistoryShown = 20

// historyPreviewChars is the longest prompt preview, in characters.
const historyPreviewChars = 100

// HistoryQuery: "/history" → ("", true), "/history foo bar" → ("foo bar", true), anything else → false.
func HistoryQuery(line string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "/history")
	if !ok {
		return "", false
	}
	if r, _ := utf8.DecodeRuneInString(rest); rest != "" && !unicode.IsSpace(r) {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// HistoryListing lists the saved prompts containing query (case-insensitive; empty = all),
// oldest first, the last HistoryShown of them. /history lines themselves are left out.
func HistoryListing(entries []string, query string) string {
	needle := strings.ToLower(query)
	var matches []string
	for _, e := range entries {
		if _, isHistory := HistoryQuery(e); !isHistory && strings.Contains(strings.ToLower(e), needle) {
			matches = append(matches, e)
		}
	}
	if len(matches) == 0 {
		if query == "" {
			return "no saved prompts yet"
		}
		return fmt.Sprintf("no saved prompt contains \"%s\"", query)
	}
	shown := matches[max(0, len(matches)-HistoryShown):]
	what := ""
	if query != "" {
		what = fmt.Sprintf(" containing \"%s\"", query)
	}
	count := fmt.Sprint(len(matches))
	if len(shown) < len(matches) {
		count = fmt.Sprintf("last %d of %d", len(shown), len(matches))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s saved prompt(s)%s, oldest first (↑ recalls them):", count, what)
	for _, e := range shown {
		sb.WriteString("\n  ")
		sb.WriteString(historyPreview(e))
	}
	return sb.String()
}

// historyPreview: the first line of prompt, cut to historyPreviewChars; … marks anything left out.
func historyPreview(prompt string) string {
	first, _, _ := strings.Cut(prompt, "\n")
	first = strings.TrimSuffix(first, "\r")
	out := first
	if rs := []rune(first); len(rs) > historyPreviewChars {
		out = string(rs[:historyPreviewChars])
	}
	if len(out) < len(first) || len(strings.TrimRightFunc(prompt, unicode.IsSpace)) > len(first) {
		out += "…"
	}
	return out
}
