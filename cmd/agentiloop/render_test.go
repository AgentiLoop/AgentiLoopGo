package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func rows(md string, w int) []string {
	lines, _ := renderMarkdown(md, w)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text()
	}
	return out
}

func eqRows(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestHeadingsListsAndCode(t *testing.T) {
	md := "# Title\n\nSome *em* and **strong** text.\n\n- one\n- two\n  - nested\n\n1. first\n2. second\n\n```rust\nfn x() {}\n```\n"
	eqRows(t, rows(md, 40), []string{
		" Title ", "", "Some em and strong text.", "", "• one", "• two", "  • nested", "",
		"1. first", "2. second", "", "▎rust", "▎ fn x() {}",
	})
	lines, _ := renderMarkdown(md, 40)
	has := func(l Line, f func(Span) bool) bool {
		for _, s := range l {
			if f(s) {
				return true
			}
		}
		return false
	}
	if !has(lines[0], func(s Span) bool {
		_, bg, _ := s.Style.Decompose()
		return strings.Contains(s.Text, "Title") && bg == tcell.ColorPurple
	}) {
		t.Fatal("H1 banner")
	}
	if !has(lines[2], func(s Span) bool { _, _, a := s.Style.Decompose(); return s.Text == "em" && a&tcell.AttrItalic != 0 }) {
		t.Fatal("italic")
	}
	if !has(lines[2], func(s Span) bool { _, _, a := s.Style.Decompose(); return s.Text == "strong" && a&tcell.AttrBold != 0 }) {
		t.Fatal("bold")
	}
}

func TestHeadingLevelsDropHashes(t *testing.T) {
	eqRows(t, rows("## Two\n\n### Three\n\n#### Four", 40), []string{"Two", strings.Repeat("─", 40), "", "Three", "", "Four"})
}

func TestTablesAreBoxedAndAligned(t *testing.T) {
	md := "| Feature | Groovy? |\n|---|---|\n| Dynamic | ✅ |\n| **Scripting** | yes |"
	eqRows(t, rows(md, 60), []string{
		"┌───────────┬─────────┐",
		"│ Feature   │ Groovy? │",
		"├───────────┼─────────┤",
		"│ Dynamic   │ ✅      │",
		"│ Scripting │ yes     │",
		"└───────────┴─────────┘",
	})
}

func TestWrapsAtWidthWithContinuationIndent(t *testing.T) {
	eqRows(t, rows("- alpha beta gamma delta epsilon", 14), []string{"• alpha beta", "  gamma delta", "  epsilon"})
	words := strings.TrimSpace(strings.Repeat("word ", 10))
	r := rows(words, 12)
	for _, l := range r {
		if width(l) > 12 {
			t.Fatal(r)
		}
	}
	if strings.Join(r, " ") != words {
		t.Fatal(r)
	}
}

func TestBlockquoteAndInlineCode(t *testing.T) {
	eqRows(t, rows("> quoted `code` here", 40), []string{"│ quoted `code` here"})
}

func TestParagraphsSeparatedByOneBlankRow(t *testing.T) {
	eqRows(t, rows("para one\n\npara two", 20), []string{"para one", "", "para two"})
}

func TestLinkPositionsAreReported(t *testing.T) {
	md := "see [the docs](https://a.io/x) and [b](https://b.io)"
	lines, links := renderMarkdown(md, 80)
	eqRows(t, rows(md, 80), []string{"see the docs and b"})
	want := []LinkPos{{0, 4, 12, "https://a.io/x"}, {0, 17, 18, "https://b.io"}}
	if !reflect.DeepEqual(links, want) {
		t.Fatalf("%+v", links)
	}
	for _, s := range lines[0] {
		if s.Link != 0 {
			t.Fatal("link tag must be stripped")
		}
	}
	// A link that wraps reports one range per line, inside a list indent.
	_, links = renderMarkdown("- [alpha beta gamma](https://w.io)", 12)
	var ranges [][3]int
	for _, l := range links {
		ranges = append(ranges, [3]int{l.Line, l.Start, l.End})
		if l.URL != "https://w.io" {
			t.Fatal(l)
		}
	}
	if !reflect.DeepEqual(ranges, [][3]int{{0, 2, 7}, {1, 2, 12}}) {
		t.Fatal(ranges)
	}
}

func TestMarkdownFencesRenderAsMarkdown(t *testing.T) {
	eqRows(t, rows("```markdown\n# Hi\n\n- a\n```\n", 20), []string{" Hi ", "", "• a"})
	// An embedded ```markdown block renders too; other languages stay code.
	eqRows(t, rows("Intro\n\n```markdown\n### Hi\n```\n\n```rust\n# x\n```", 20), []string{"Intro", "", "Hi", "", "▎rust", "▎ # x"})
}

// ---- highlight -------------------------------------------------------------------

func find(t *testing.T, l Line, text string) tcell.Style {
	t.Helper()
	for _, s := range l {
		if s.Text == text {
			return s.Style
		}
	}
	t.Fatalf("no span %q in %+v", text, l)
	return tcell.StyleDefault
}

func fgOf(st tcell.Style) tcell.Color { f, _, _ := st.Decompose(); return f }

func TestRustKeywordsGetAColorAndTextIsPreserved(t *testing.T) {
	hl := highlight("fn main() {}\n", "rust")
	if len(hl) != 1 || hl[0].Text() != "fn main() {}" {
		t.Fatalf("%+v", hl)
	}
	kw, name := find(t, hl[0], "fn"), find(t, hl[0], "main")
	if !fgOf(kw).IsRGB() || fgOf(kw) == fgOf(name) {
		t.Fatal("keyword / name colors")
	}
}

func TestSwiftAndTOMLGrammarsLoad(t *testing.T) {
	if hl := highlight("let x: Int = 1\n", "swift"); hl == nil {
		t.Fatal("swift")
	} else {
		find(t, hl[0], "let")
	}
	if highlight("[package]\nname = \"a\"\n", "toml") == nil {
		t.Fatal("toml")
	}
	if highlightNumbered([]string{"    1│import SwiftUI"}, "App/AgentApp.swift") == nil {
		t.Fatal("numbered swift")
	}
}

func TestXcodeDarkPaletteIsApplied(t *testing.T) {
	hl := highlight("pub fn f() { let s = \"hi\"; } // note\n", "rust")
	kw := find(t, hl[0], "fn")
	if _, _, a := kw.Decompose(); fgOf(kw) != rgb(xcKeyword) || a&tcell.AttrBold == 0 {
		t.Fatal("keyword")
	}
	if fgOf(find(t, hl[0], "f")) != rgb(xcFunction) {
		t.Fatal("function")
	}
	var str, comment tcell.Color
	for _, s := range hl[0] {
		if strings.Contains(s.Text, "hi") {
			str = fgOf(s.Style)
		}
		if strings.Contains(s.Text, "note") {
			comment = fgOf(s.Style)
		}
	}
	if str != rgb(xcString) || comment != rgb(xcComment) {
		t.Fatal("string / comment")
	}
	sw := highlight("import SwiftUI\nstruct A { var x: Int = 1 }\n", "swift")
	if fgOf(find(t, sw[0], "import")) != rgb(xcPreproc) {
		t.Fatal("import should be Xcode orange")
	}
	if fgOf(find(t, sw[1], "struct")) != rgb(xcKeyword) {
		t.Fatal("struct")
	}
	if fgOf(find(t, sw[1], "Int")) != rgb(xcType) {
		t.Fatal("Int should be a type")
	}
}

func TestUnknownLanguageIsNil(t *testing.T) {
	if highlight("x", "nope-not-a-lang") != nil || highlight("x", "") != nil {
		t.Fatal("expected nil")
	}
}

func TestNumberedLinesKeepGutter(t *testing.T) {
	lines := []string{"    1│fn x() {}", "    2│"}
	hl := highlightNumbered(lines, "src/a.rs")
	if hl == nil || hl[0].Text() != "    1│fn x() {}" || hl[1].Text() != "    2│" {
		t.Fatalf("%+v", hl)
	}
	if highlightNumbered([]string{"no gutter"}, "a.rs") != nil || highlightNumbered(lines, "README") != nil {
		t.Fatal("expected nil")
	}
}

func TestHardWrapSplitsAtWidth(t *testing.T) {
	var got []string
	for _, p := range hardWrap([]Span{raw("abcdefgh")}, 3) {
		got = append(got, p.Text())
	}
	eqRows(t, got, []string{"abc", "def", "gh"})
	if len(hardWrap(nil, 3)) != 1 {
		t.Fatal("empty input yields one piece")
	}
}

func TestWrapText(t *testing.T) {
	eqRows(t, wrapText("", 5), []string{""})
	eqRows(t, wrapText("aaa bbb ccc", 7), []string{"aaa bbb", "ccc"})
	eqRows(t, wrapText("abcdefghij", 4), []string{"abcd", "efgh", "ij"})
}
