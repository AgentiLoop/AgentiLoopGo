package main

// Markdown → styled, word-wrapped lines for the TUI transcript.
//
// Supports headings, paragraphs, emphasis/strong/strikethrough, inline code,
// fenced code blocks, bullet and numbered lists (nested), block quotes,
// tables, links and horizontal rules. Anything else falls through as plain text.

import (
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// linkBlue is macOS system blue (dark-mode variant). ANSI blue is far too
// dark on dark themes; truecolor works in every mainstream terminal.
var linkBlue = tcell.NewRGBColor(10, 132, 255)

// LinkPos is a link's cell range on one rendered line, for click handling.
type LinkPos struct {
	Line, Start, End int
	URL              string
}

var mdParser = goldmark.New(goldmark.WithExtensions(extension.Strikethrough, extension.Table)).Parser()

// renderMarkdown renders text as markdown into lines no wider than w cells,
// also returning where each link landed (for click handling).
func renderMarkdown(src string, w int) ([]Line, []LinkPos) {
	r := &mdRenderer{width: max(w, 1)}
	source := []byte(unfenceMarkdown(src))
	doc := mdParser.Parse(text.NewReader(source))
	r.src = source
	ast.Walk(doc, r.walk)
	r.flushPara()
	// Drop the trailing blank separator so entries don't double-space.
	for len(r.out) > 0 && len(r.out[len(r.out)-1]) == 0 {
		r.out = r.out[:len(r.out)-1]
	}
	return r.out, r.links
}

// unfenceMarkdown: models often wrap a whole reply in a ```markdown fence when
// asked for markdown. Rendering that as a code block defeats the purpose, so if
// the entire text is one such fence, return its body instead.
func unfenceMarkdown(s string) string {
	t := strings.TrimSpace(s)
	for _, p := range []string{"```markdown\n", "```md\n"} {
		if body, ok := strings.CutPrefix(t, p); ok {
			if inner, ok := strings.CutSuffix(body, "```"); ok {
				return inner
			}
		}
	}
	return s
}

// mod is one inline style layer; layers are folded to the effective style.
type mod struct {
	fg, bg                                tcell.Color
	bold, italic, underline, strike, link bool
	linkID                                int
}

type listKind struct {
	numbered bool
	next     int
}

type mdRenderer struct {
	src    []byte
	width  int
	out    []Line
	para   []Span // inline spans of the block being built
	styles []mod
	lists  []*listKind
	quote  int
	// marker is the prefix for the first wrapped line of the current block (e.g. "1. ").
	marker  *string
	heading int
	// table rows of cells; row 0 is the header.
	table   [][][]Span
	needGap bool
	urls    []string
	links   []LinkPos
}

func (r *mdRenderer) style() (tcell.Style, int) {
	st := tcell.StyleDefault
	link := 0
	for _, m := range r.styles {
		if m.fg != tcell.ColorDefault {
			st = st.Foreground(m.fg)
		}
		if m.bg != tcell.ColorDefault {
			st = st.Background(m.bg)
		}
		if m.bold {
			st = st.Bold(true)
		}
		if m.italic {
			st = st.Italic(true)
		}
		if m.underline {
			st = st.Underline(true)
		}
		if m.strike {
			st = st.StrikeThrough(true)
		}
		if m.link {
			link = m.linkID + 1
		}
	}
	return st, link
}

func (r *mdRenderer) text(s string) {
	if s == "" {
		return
	}
	st, link := r.style()
	r.para = append(r.para, Span{s, st, link})
}

// indent for continuation lines: two cells per open list plus quote bars.
func (r *mdRenderer) indent() string {
	return strings.Repeat("│ ", r.quote) + strings.Repeat("  ", len(r.lists))
}

func (r *mdRenderer) gap() {
	if r.needGap && len(r.out) > 0 {
		r.out = append(r.out, nil)
	}
	r.needGap = false
}

func trimLast2(s string) string {
	if len(s) < 2 {
		return ""
	}
	return s[:len(s)-2]
}

func (r *mdRenderer) flushPara() {
	if len(r.para) == 0 {
		return
	}
	spans := r.para
	r.para = nil
	cont := r.indent()
	first := cont
	if r.marker != nil {
		// The marker replaces the innermost list indent on the first line.
		first = trimLast2(cont) + *r.marker
		r.marker = nil
	}
	lines := wrapSpans(spans, r.width, first, cont)
	for i, line := range lines {
		col := 0
		for j := range line {
			s := &line[j]
			w := width(s.Text)
			if s.Link > 0 {
				url := r.urls[s.Link-1]
				row := len(r.out) + i
				// Same link continuing in the next span (e.g. `code` inside it).
				if n := len(r.links); n > 0 && r.links[n-1].Line == row && r.links[n-1].End == col && r.links[n-1].URL == url {
					r.links[n-1].End = col + w
				} else {
					r.links = append(r.links, LinkPos{row, col, col + w, url})
				}
				s.Link = 0
			}
			col += w
		}
	}
	r.out = append(r.out, lines...)
}

func (r *mdRenderer) segText(n ast.Node) string {
	var sb strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			sb.Write(t.Segment.Value(r.src))
		case *ast.String:
			sb.Write(t.Value)
		default:
			sb.WriteString(r.segText(c))
		}
	}
	return sb.String()
}

func linesText(src []byte, lines *text.Segments) string {
	var sb strings.Builder
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		sb.Write(seg.Value(src))
	}
	return sb.String()
}

func (r *mdRenderer) walk(n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch n := n.(type) {
	case *ast.Document, *ast.TextBlock:
	case *ast.Text:
		if entering {
			r.text(string(n.Segment.Value(r.src)))
			if n.HardLineBreak() {
				r.flushPara()
				r.marker = nil
			} else if n.SoftLineBreak() {
				r.text(" ")
			}
		}
	case *ast.String:
		if entering {
			r.text(string(n.Value))
		}
	case *ast.CodeSpan:
		if entering {
			st, link := r.style()
			r.para = append(r.para, Span{"`" + r.segText(n) + "`", st.Foreground(tcell.ColorOlive), link})
		}
		return ast.WalkSkipChildren, nil
	case *ast.ThematicBreak:
		if entering {
			r.flushPara()
			r.gap()
			r.out = append(r.out, Line{styled(strings.Repeat("─", min(r.width, 40)), fg(tcell.ColorGray))})
			r.needGap = true
		}
	case *ast.HTMLBlock:
		if entering {
			r.text(linesText(r.src, n.Lines()))
		}
	case *ast.RawHTML:
		if entering {
			r.text(linesText(r.src, n.Segments))
		}
	case *ast.Paragraph:
		if entering {
			r.flushPara()
			// Inside a list item the first paragraph sits on the marker line.
			if r.marker == nil {
				r.gap()
			}
		} else {
			r.flushPara()
			r.needGap = true
		}
	case *ast.Heading:
		r.headingNode(n.Level, entering)
	case *ast.Blockquote:
		if entering {
			r.flushPara()
			r.gap()
			r.quote++
			r.styles = append(r.styles, mod{italic: true})
		} else {
			r.flushPara()
			r.quote--
			r.popStyle()
			r.needGap = true
		}
	case *ast.FencedCodeBlock:
		if entering {
			lang := ""
			if n.Info != nil {
				lang = strings.TrimSpace(string(n.Language(r.src)))
			}
			r.codeBlock(linesText(r.src, n.Lines()), lang)
		}
		return ast.WalkSkipChildren, nil
	case *ast.CodeBlock:
		if entering {
			r.codeBlock(linesText(r.src, n.Lines()), "")
		}
		return ast.WalkSkipChildren, nil
	case *ast.List:
		if entering {
			r.flushPara()
			if len(r.lists) == 0 {
				r.gap()
			}
			r.lists = append(r.lists, &listKind{numbered: n.IsOrdered(), next: n.Start})
		} else {
			r.flushPara()
			r.lists = r.lists[:len(r.lists)-1]
			if len(r.lists) == 0 {
				r.needGap = true
			}
		}
	case *ast.ListItem:
		if entering {
			r.flushPara()
			m := "• "
			if l := r.lists[len(r.lists)-1]; l.numbered {
				m = strconv.Itoa(l.next) + ". "
				l.next++
			}
			r.marker = &m
		} else {
			r.flushPara()
			// An empty item still shows its marker.
			if r.marker != nil {
				r.out = append(r.out, Line{raw(trimLast2(r.indent()) + *r.marker)})
				r.marker = nil
			}
			r.needGap = false
		}
	case *ast.Emphasis:
		if entering {
			if n.Level >= 2 {
				r.styles = append(r.styles, mod{bold: true})
			} else {
				r.styles = append(r.styles, mod{italic: true})
			}
		} else {
			r.popStyle()
		}
	case *east.Strikethrough:
		if entering {
			r.styles = append(r.styles, mod{strike: true})
		} else {
			r.popStyle()
		}
	case *ast.Link:
		if entering {
			r.pushLink(string(n.Destination))
		} else {
			r.popStyle()
		}
	case *ast.AutoLink:
		if entering {
			r.pushLink(string(n.URL(r.src)))
			r.text(string(n.Label(r.src)))
			r.popStyle()
		}
	case *ast.Image:
		if entering {
			r.text("[image: ")
			r.text(string(n.Destination))
			r.text("]")
		}
	case *east.Table:
		if entering {
			r.flushPara()
			r.gap()
			r.table = nil
		} else {
			rows := r.table
			r.table = nil
			r.out = append(r.out, drawTable(rows, r.indent())...)
			r.needGap = true
		}
	case *east.TableHeader, *east.TableRow:
		if entering {
			r.table = append(r.table, nil)
		}
	case *east.TableCell:
		if entering {
			r.para = nil
		} else {
			cell := r.para
			r.para = nil
			if len(r.table) > 0 {
				r.table[len(r.table)-1] = append(r.table[len(r.table)-1], cell)
			}
		}
	}
	return ast.WalkContinue, nil
}

func (r *mdRenderer) popStyle() {
	if len(r.styles) > 0 {
		r.styles = r.styles[:len(r.styles)-1]
	}
}

func (r *mdRenderer) pushLink(url string) {
	id := len(r.urls)
	r.urls = append(r.urls, url)
	r.styles = append(r.styles, mod{fg: linkBlue, underline: true, link: true, linkID: id})
}

func (r *mdRenderer) headingNode(level int, entering bool) {
	if entering {
		r.flushPara()
		r.gap()
		// No font sizes in a terminal: H1 is a filled banner, H2 is bold with a
		// rule under it, H3 bold yellow, H4+ plain bold.
		m := mod{fg: tcell.ColorWhite, bold: true}
		switch level {
		case 1:
			m.bg = tcell.ColorPurple
		case 2:
			m.fg = tcell.ColorTeal
		case 3:
			m.fg = tcell.ColorOlive
		}
		r.styles = append(r.styles, m)
		r.heading = level
		return
	}
	level, r.heading = r.heading, 0
	st, _ := r.style()
	if level == 1 {
		// Banner: pad the text so the background reads as a block.
		r.para = append([]Span{styled(" ", st)}, r.para...)
	}
	r.flushPara()
	if level == 1 && len(r.out) > 0 {
		// Right pad goes on after wrapping, which trims trailing spaces.
		r.out[len(r.out)-1] = append(r.out[len(r.out)-1], styled(" ", st))
	}
	r.popStyle()
	if level == 2 {
		ind := r.indent()
		n := max(min(r.width-len(ind), 60), 1)
		r.out = append(r.out, Line{styled(ind+strings.Repeat("─", n), fg(tcell.ColorTeal))})
	}
	r.needGap = true
}

func (r *mdRenderer) codeBlock(code, lang string) {
	r.flushPara()
	r.gap()
	ind := r.indent()
	// A ```markdown fence is the model showing Markdown, not code: render its
	// body instead of boxing it.
	if lang == "markdown" || lang == "md" {
		inner, links := renderMarkdown(code, r.width-len(ind))
		base, off := len(r.out), width(ind)
		for _, l := range links {
			r.links = append(r.links, LinkPos{base + l.Line, l.Start + off, l.End + off, l.URL})
		}
		for _, l := range inner {
			r.out = append(r.out, append(Line{raw(ind)}, l...))
		}
		r.needGap = true
		return
	}
	gray := fg(tcell.ColorGray)
	if lang != "" {
		r.out = append(r.out, Line{styled(ind+"▎"+lang, gray)})
	}
	// Syntax-highlight when the fence language is known; plain green otherwise.
	// Code is hard-wrapped, never word-wrapped.
	lines := highlight(code, lang)
	if lines == nil {
		for _, l := range strings.Split(strings.TrimSuffix(code, "\n"), "\n") {
			lines = append(lines, Line{styled(l, fg(tcell.ColorGreen))})
		}
	}
	avail := max(r.width-(len(ind)+2), 1)
	for _, line := range lines {
		for _, piece := range hardWrap(line, avail) {
			r.out = append(r.out, append(Line{styled(ind+"▎ ", gray)}, piece...))
		}
	}
	r.needGap = true
}

// wrapSpans word-wraps styled spans to w, prefixing the first line with
// `first` and the rest with `cont`.
func wrapSpans(spans []Span, w int, first, cont string) []Line {
	var lines []Line
	cur := Line{raw(first)}
	col := width(first)
	fresh := true
	for _, sp := range spans {
		for _, word := range splitWords(sp.Text) {
			ww := width(word)
			if !fresh && col+ww > w {
				trimEnd(cur)
				lines = append(lines, cur)
				cur = Line{raw(cont)}
				col = width(cont)
				fresh = true
				if strings.TrimSpace(word) == "" {
					continue
				}
			}
			if last := &cur[len(cur)-1]; !fresh && last.Style == sp.Style && last.Link == sp.Link {
				last.Text += word
			} else {
				cur = append(cur, Span{word, sp.Style, sp.Link})
			}
			col += ww
			fresh = false
		}
	}
	trimEnd(cur)
	return append(lines, cur)
}

func trimEnd(l Line) {
	if len(l) > 0 {
		l[len(l)-1].Text = strings.TrimRight(l[len(l)-1].Text, " \t\n")
	}
}

// drawTable box-draws a table with columns sized to their widest cell. The
// first row is the header (bold) and is separated from the body by a rule.
func drawTable(rows [][][]Span, ind string) []Line {
	ncols := 0
	for _, r := range rows {
		ncols = max(ncols, len(r))
	}
	if ncols == 0 {
		return nil
	}
	// Cell text trimmed at its outer edges (markdown leaves padding around cell content).
	cells := make([][][]Span, len(rows))
	for ri, r := range rows {
		for _, c := range r {
			var out []Span
			for i, s := range c {
				t := s.Text
				if i == 0 {
					t = strings.TrimLeft(t, " \t")
				}
				if i == len(c)-1 {
					t = strings.TrimRight(t, " \t")
				}
				// Table links are styled but not clickable.
				out = append(out, Span{Text: t, Style: s.Style})
			}
			cells[ri] = append(cells[ri], out)
		}
	}
	cellW := func(c []Span) int {
		n := 0
		for _, s := range c {
			n += width(s.Text)
		}
		return n
	}
	widths := make([]int, ncols)
	for _, r := range cells {
		for i, c := range r {
			widths[i] = max(widths[i], cellW(c))
		}
	}
	border := fg(tcell.ColorGray)
	rule := func(l, m, rt string) Line {
		body := make([]string, len(widths))
		for i, w := range widths {
			body[i] = strings.Repeat("─", w+2)
		}
		return Line{styled(ind+l+strings.Join(body, m)+rt, border)}
	}
	out := []Line{rule("┌", "┬", "┐")}
	for ri, r := range cells {
		line := Line{styled(ind+"│", border)}
		for i, w := range widths {
			used := 0
			line = append(line, raw(" "))
			if i < len(r) {
				used = cellW(r[i])
				for _, s := range r[i] {
					st := s.Style
					if ri == 0 {
						st = st.Bold(true)
					}
					line = append(line, styled(s.Text, st))
				}
			}
			line = append(line, raw(strings.Repeat(" ", w-used+1)), styled("│", border))
		}
		out = append(out, line)
		if ri == 0 && len(cells) > 1 {
			out = append(out, rule("├", "┼", "┤"))
		}
	}
	return append(out, rule("└", "┴", "┘"))
}
