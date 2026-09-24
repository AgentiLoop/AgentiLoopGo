package main

// Styled text primitives shared by the markdown renderer, the highlighter and
// the TUI: a Span is a run of text in one style, a Line is a row of spans.

import (
	"strings"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

type Span struct {
	Text  string
	Style tcell.Style
	// Link is 1 + the index of the link this span belongs to (0 = none). Carried
	// through word-wrapping so link positions can be reported after layout.
	Link int
}

type Line []Span

func raw(s string) Span                    { return Span{Text: s, Style: tcell.StyleDefault} }
func styled(s string, st tcell.Style) Span { return Span{Text: s, Style: st} }

func rgb(h int32) tcell.Color { return tcell.NewHexColor(h) }

func fg(c tcell.Color) tcell.Style { return tcell.StyleDefault.Foreground(c) }

func width(s string) int { return runewidth.StringWidth(s) }

// Text is the concatenated text of the line.
func (l Line) Text() string {
	var sb strings.Builder
	for _, s := range l {
		sb.WriteString(s.Text)
	}
	return sb.String()
}

func (l Line) Width() int { return width(l.Text()) }

// hardWrap splits styled spans at `w` cells (code is never word-wrapped).
// Always yields at least one (possibly empty) piece.
func hardWrap(spans []Span, w int) []Line {
	w = max(w, 1)
	var out []Line
	var cur Line
	col := 0
	for _, sp := range spans {
		var buf strings.Builder
		for _, r := range sp.Text {
			rw := runewidth.RuneWidth(r)
			if col > 0 && col+rw > w {
				if buf.Len() > 0 {
					cur = append(cur, Span{buf.String(), sp.Style, sp.Link})
					buf.Reset()
				}
				out = append(out, cur)
				cur = nil
				col = 0
			}
			buf.WriteRune(r)
			col += rw
		}
		if buf.Len() > 0 {
			cur = append(cur, Span{buf.String(), sp.Style, sp.Link})
		}
	}
	return append(out, cur)
}

// splitWords splits into words, each carrying its trailing whitespace.
func splitWords(s string) []string {
	var words []string
	start, inSpace := 0, false
	for i, r := range s {
		if unicode.IsSpace(r) {
			inSpace = true
		} else if inSpace {
			words = append(words, s[start:i])
			start, inSpace = i, false
		}
	}
	if start < len(s) {
		words = append(words, s[start:])
	}
	return words
}

// wrapText word-wraps one line of plain text to `w` cells (like the textwrap
// crate): trailing spaces are dropped at breaks, words longer than a line are
// split, and an empty line yields one empty piece.
func wrapText(s string, w int) []string {
	w = max(w, 1)
	var out []string
	var cur strings.Builder
	col := 0
	flush := func() {
		out = append(out, strings.TrimRightFunc(cur.String(), unicode.IsSpace))
		cur.Reset()
		col = 0
	}
	for _, word := range splitWords(s) {
		tw := width(strings.TrimRightFunc(word, unicode.IsSpace))
		if col > 0 && col+tw > w {
			flush()
		}
		// Hard-break words that don't fit on a line of their own.
		for col == 0 && tw > w {
			n, cut := 0, 0
			for i, r := range word {
				rw := runewidth.RuneWidth(r)
				if n+rw > w && i > 0 {
					break
				}
				n, cut = n+rw, i+len(string(r))
			}
			cur.WriteString(word[:cut])
			flush()
			word = word[cut:]
			tw = width(strings.TrimRightFunc(word, unicode.IsSpace))
		}
		cur.WriteString(word)
		col += width(word)
	}
	flush()
	return out
}
