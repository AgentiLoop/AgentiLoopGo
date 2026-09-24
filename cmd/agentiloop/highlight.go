package main

// Syntax highlighting (chroma) → styled spans. Used for fenced code blocks in
// markdown and for read_file previews in the TUI.

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/gdamore/tcell/v2"
)

// Xcode Dark palette (same hex values as AgentColorSyntax's CodeBlockTheme).
// Keywords are bold like the Agent app.
const (
	xcPlain     = 0xDFDFE0
	xcComment   = 0x6C9C5A
	xcKeyword   = 0xFF7AB2
	xcString    = 0xFC6A5D
	xcNumber    = 0xD9C97C
	xcType      = 0xD0A8FF
	xcFunction  = 0x67B7A4
	xcBuiltin   = 0xB281EB
	xcPreproc   = 0xFFA14F
	xcAttribute = 0xFD8F3F
	xcProperty  = 0x4EB0CC
)

// importWords are keywords that pull in other code; Xcode colors them like the preprocessor.
var importWords = map[string]bool{"import": true, "@import": true, "#import": true, "#include": true, "using": true}

// selfWords are the language's "this object" keywords, colored like keywords.
var selfWords = map[string]bool{"self": true, "Self": true, "this": true, "super": true}

// tokenStyle maps a chroma token (type + text) onto the palette. Chroma's token
// classes are coarser than TextMate scopes, so a few words are special-cased to
// match Xcode: imports are orange, self/this are keywords, capitalized builtins are types.
func tokenStyle(t chroma.TokenType, value string) tcell.Style {
	c := func(h int32, bold bool) tcell.Style { return fg(rgb(h)).Bold(bold) }
	v := strings.TrimSpace(value)
	switch {
	case importWords[v] && (t.InCategory(chroma.Keyword) || t.InCategory(chroma.Comment)):
		return c(xcPreproc, true)
	case selfWords[v] && (t.InCategory(chroma.Keyword) || t.InCategory(chroma.Name)):
		return c(xcKeyword, true)
	case t == chroma.NameBuiltin && v != "" && unicode.IsUpper([]rune(v)[0]):
		return c(xcType, false)
	case t == chroma.CommentPreproc || t == chroma.CommentPreprocFile || t == chroma.KeywordNamespace:
		return c(xcPreproc, true)
	case t.InCategory(chroma.Comment):
		return c(xcComment, false)
	case t == chroma.KeywordType:
		return c(xcType, false)
	case t == chroma.KeywordConstant:
		return c(xcNumber, false)
	case t.InCategory(chroma.Keyword):
		return c(xcKeyword, true)
	case t.InCategory(chroma.LiteralString):
		return c(xcString, false)
	case t.InCategory(chroma.LiteralNumber) || t == chroma.NameConstant:
		return c(xcNumber, false)
	case t == chroma.NameBuiltinPseudo:
		return c(xcKeyword, true)
	case t == chroma.NameClass || t == chroma.NameNamespace || t == chroma.NameException:
		return c(xcType, false)
	case t == chroma.NameFunction || t == chroma.NameFunctionMagic:
		return c(xcFunction, false)
	case t == chroma.NameBuiltin:
		return c(xcBuiltin, false)
	case t == chroma.NameDecorator || t == chroma.NameAttribute:
		return c(xcAttribute, false)
	case t == chroma.NameTag || t == chroma.NameProperty:
		return c(xcProperty, false)
	}
	return c(xcPlain, false)
}

// lexerFor resolves a fence language ("rust", "py", "bash") or a file extension.
func lexerFor(hint string) chroma.Lexer {
	h := strings.TrimSpace(hint)
	if h == "" {
		return nil
	}
	if l := lexers.Get(h); l != nil {
		return l
	}
	return lexers.Match("x." + strings.TrimPrefix(h, "."))
}

// highlight returns one span list per line, no line terminators. nil when the
// language is unknown.
func highlight(code, hint string) []Line {
	lexer := lexerFor(hint)
	if lexer == nil {
		return nil
	}
	lexer = chroma.Coalesce(lexer)
	code = strings.ReplaceAll(code, "\t", "    ")
	it, err := lexer.Tokenise(nil, code)
	if err != nil {
		return nil
	}
	out := []Line{nil}
	for _, tok := range it.Tokens() {
		st := tokenStyle(tok.Type, tok.Value)
		parts := strings.Split(tok.Value, "\n")
		for i, p := range parts {
			if i > 0 {
				out = append(out, nil)
			}
			if p = strings.TrimSuffix(p, "\r"); p != "" {
				out[len(out)-1] = append(out[len(out)-1], styled(p, st))
			}
		}
	}
	// A trailing newline doesn't start another line.
	if strings.HasSuffix(code, "\n") && len(out) > 1 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	return out
}

// highlightNumbered highlights read_file-style lines ("    1│code") by path's
// extension, keeping the gutter. nil if the extension is unknown or a line has no gutter.
func highlightNumbered(lines []string, path string) []Line {
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		return nil
	}
	gutters := make([]string, len(lines))
	bodies := make([]string, len(lines))
	for i, l := range lines {
		g, b, ok := strings.Cut(l, "│")
		if !ok {
			return nil
		}
		gutters[i], bodies[i] = g+"│", b
	}
	code := highlight(strings.Join(bodies, "\n"), ext)
	if code == nil {
		return nil
	}
	out := make([]Line, len(gutters))
	for i, g := range gutters {
		line := Line{styled(g, fg(tcell.ColorGray))}
		if i < len(code) {
			line = append(line, code[i]...)
		}
		out[i] = line
	}
	return out
}
