package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

// skipDirs are never descended into: VCS data and dependency/build output that would drown real hits.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true, "target": true,
	"__pycache__": true, ".venv": true, "venv": true,
}

const (
	maxGlobResults     = 500
	defaultGrepResults = 200
	maxGrepResults     = 2000
	maxLineChars       = 300
	maxFileBytes       = 2 * 1024 * 1024
)

// display is `path` as the model should quote it back: relative to the working directory when inside it.
func display(tc core.ToolContext, path string) string {
	if rel, err := filepath.Rel(tc.Cwd, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		path = rel
	}
	return filepath.ToSlash(path)
}

// ------------------------------------------------------------------- globbing

// expandBraces turns `a.{rs,go}` into `a.rs`, `a.go` (nested braces supported).
func expandBraces(p string) []string {
	r := []rune(p)
	open := -1
	for i, c := range r {
		if c == '{' {
			open = i
			break
		}
	}
	if open < 0 {
		return []string{p}
	}
	depth, closeIdx := 0, -1
	for i := open; i < len(r) && closeIdx < 0; i++ {
		switch r[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		}
	}
	if closeIdx < 0 {
		return []string{p}
	}
	prefix, suffix := string(r[:open]), string(r[closeIdx+1:])
	var alts []string
	var cur []rune
	depth = 0
	for _, c := range r[open+1 : closeIdx] {
		if c == ',' && depth == 0 {
			alts = append(alts, string(cur))
			cur = nil
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
		}
		cur = append(cur, c)
	}
	alts = append(alts, string(cur))
	var out []string
	for _, a := range alts {
		out = append(out, expandBraces(prefix+a+suffix)...)
	}
	return out
}

// segmentMatch matches `*` and `?` within one path segment.
func segmentMatch(pat, s []rune) bool {
	p, i, star, mark := 0, 0, -1, 0
	for i < len(s) {
		switch {
		case p < len(pat) && (pat[p] == '?' || (pat[p] != '*' && pat[p] == s[i])):
			p++
			i++
		case p < len(pat) && pat[p] == '*':
			star, mark = p, i
			p++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}

func segmentsMatch(pat, path [][]rune) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if string(pat[0]) == "**" {
		for skip := 0; skip <= len(path); skip++ {
			if segmentsMatch(pat[1:], path[skip:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	return segmentMatch(pat[0], path[0]) && segmentsMatch(pat[1:], path[1:])
}

// glob is a compiled pattern: `*`/`?` within a segment, `**` across directories, `{a,b}` alternatives.
// A pattern without `/` matches the file name at any depth.
type glob [][][]rune

func newGlob(pattern string) glob { return buildGlob(pattern, true) }

// buildGlob compiles pattern; bareAnyDepth makes a pattern without `/` match at any depth (otherwise it is anchored).
func buildGlob(pattern string, bareAnyDepth bool) glob {
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	pattern = strings.TrimPrefix(pattern, "./")
	var g glob
	for _, p := range expandBraces(pattern) {
		if bareAnyDepth && !strings.Contains(p, "/") {
			p = "**/" + p
		}
		var segs [][]rune
		for _, s := range strings.Split(p, "/") {
			if s != "" {
				segs = append(segs, []rune(s))
			}
		}
		g = append(g, segs)
	}
	return g
}

func (g glob) matches(rel string) bool {
	var path [][]rune
	for _, s := range strings.Split(rel, "/") {
		path = append(path, []rune(s))
	}
	for _, p := range g {
		if segmentsMatch(p, path) {
			return true
		}
	}
	return false
}

// ignoreRule is one .gitignore line. Supports comments, `!` negation, a trailing `/` (directories
// only), anchoring (a leading or inner `/`), `*`, `?`, `**`; not character classes.
type ignoreRule struct {
	base    string // directory holding the .gitignore, relative to the walk root ("" for the root itself)
	glob    glob
	negate  bool
	dirOnly bool
}

func parseIgnoreRule(base, line string) (ignoreRule, bool) {
	line = strings.TrimRight(line, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	r := ignoreRule{base: base}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if strings.HasPrefix(line, "\\#") || strings.HasPrefix(line, "\\!") {
		line = line[1:]
	}
	r.dirOnly = strings.HasSuffix(line, "/")
	line = strings.TrimRight(line, "/")
	if line == "" {
		return ignoreRule{}, false
	}
	r.glob = buildGlob(strings.TrimLeft(line, "/"), !strings.Contains(line, "/"))
	return r, true
}

func (r ignoreRule) matches(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	below := rel
	if r.base != "" {
		if !strings.HasPrefix(rel, r.base+"/") {
			return false
		}
		below = rel[len(r.base)+1:]
	}
	return r.glob.matches(below)
}

// isIgnored: the last matching rule wins, as in git.
func isIgnored(rules []ignoreRule, rel string, isDir bool) bool {
	ignored := false
	for _, r := range rules {
		if r.matches(rel, isDir) {
			ignored = !r.negate
		}
	}
	return ignored
}

// walk visits files under root depth-first in name order; visit returns false to stop.
// Honors .gitignore files found at or below root. Symlinked directories are not followed.
func walk(root string, visit func(abs, rel string) bool) {
	var rules []ignoreRule
	var step func(dir, rel string) bool
	step = func(dir, rel string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		mark := len(rules)
		defer func() { rules = rules[:mark] }()
		if data, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil {
			for _, l := range strings.Split(string(data), "\n") {
				if r, ok := parseIgnoreRule(rel, l); ok {
					rules = append(rules, r)
				}
			}
		}
		for _, e := range entries { // ReadDir returns entries sorted by filename
			name := e.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			switch {
			case e.IsDir():
				if !skipDirs[name] && !isIgnored(rules, childRel, true) && !step(filepath.Join(dir, name), childRel) {
					return false
				}
			case e.Type().IsRegular():
				if !isIgnored(rules, childRel, false) && !visit(filepath.Join(dir, name), childRel) {
					return false
				}
			}
		}
		return true
	}
	step(root, "")
}

// ----------------------------------------------------------------------- glob

type GlobFiles struct{}

func (GlobFiles) Name() string { return "glob" }
func (GlobFiles) Description() string {
	return "Find files by name pattern, searching recursively and skipping .git, node_modules, target, anything listed in .gitignore and similar. " +
		"`*` and `?` match within one path segment, `**` matches any number of directories, `{a,b}` offers alternatives. " +
		"A pattern without `/` (like `*.rs`) matches file names at any depth; one with `/` (like `src/**/*.go`) " +
		"matches the path relative to `path`. Returns paths sorted by name."
}
func (GlobFiles) InputSchema() any {
	return schema(map[string]any{
		"pattern": map[string]any{"type": "string", "description": "Glob, e.g. `**/*.{ts,tsx}` or `*.md`"},
		"path":    map[string]any{"type": "string", "description": "Directory to search (default: cwd)", "default": "."},
	}, "pattern")
}
func (GlobFiles) IsMutating() bool { return false }
func (GlobFiles) Call(_ context.Context, tc core.ToolContext, input json.RawMessage) (string, error) {
	a := struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}{Path: "."}
	if err := parse(input, &a, "pattern"); err != nil {
		return "", err
	}
	root := resolve(tc, a.Path)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return "", core.Failed("%s is not a directory", root)
	}
	g := newGlob(a.Pattern)
	var found []string
	more := false
	walk(root, func(abs, rel string) bool {
		if !g.matches(rel) {
			return true
		}
		if len(found) == maxGlobResults {
			more = true
			return false
		}
		found = append(found, display(tc, abs))
		return true
	})
	if len(found) == 0 {
		return "no matches", nil
	}
	out := strings.Join(found, "\n")
	if more {
		out += fmt.Sprintf("\n…[truncated: more than %d matches; narrow the pattern]", maxGlobResults)
	}
	return out, nil
}

// ----------------------------------------------------------------------- grep

type Grep struct{}

func (Grep) Name() string { return "grep" }
func (Grep) Description() string {
	return "Search file contents with a regular expression (RE2-style syntax). Searches recursively, skipping .git, " +
		"node_modules, target, anything listed in .gitignore, binary and very large files. Returns `path:line:text` for each matching line. " +
		"Use `glob` to restrict which files are searched (same syntax as the glob tool)."
}
func (Grep) InputSchema() any {
	return schema(map[string]any{
		"pattern":          map[string]any{"type": "string", "description": "Regular expression to look for"},
		"path":             map[string]any{"type": "string", "description": "File or directory to search (default: cwd)", "default": "."},
		"glob":             map[string]any{"type": "string", "description": "Only search files matching this glob, e.g. `*.rs`"},
		"case_insensitive": map[string]any{"type": "boolean", "default": false},
		"max_results":      map[string]any{"type": "integer", "description": "Maximum matching lines to return", "default": 200},
	}, "pattern")
}
func (Grep) IsMutating() bool { return false }

func truncateLine(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if utf8.RuneCountInString(line) > maxLineChars {
		return string([]rune(line)[:maxLineChars]) + "…"
	}
	return line
}

func (Grep) Call(_ context.Context, tc core.ToolContext, input json.RawMessage) (string, error) {
	a := struct {
		Pattern         string  `json:"pattern"`
		Path            string  `json:"path"`
		Glob            *string `json:"glob"`
		CaseInsensitive bool    `json:"case_insensitive"`
		MaxResults      *int    `json:"max_results"`
	}{Path: "."}
	if err := parse(input, &a, "pattern"); err != nil {
		return "", err
	}
	expr := a.Pattern
	if a.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", core.InvalidInput("bad regex: %v", err)
	}
	root := resolve(tc, a.Path)
	fi, err := os.Stat(root)
	if err != nil {
		return "", core.Failed("%s does not exist", root)
	}
	limit := defaultGrepResults
	if a.MaxResults != nil {
		limit = *a.MaxResults
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxGrepResults {
		limit = maxGrepResults
	}
	var filter glob
	if a.Glob != nil {
		filter = newGlob(*a.Glob)
	}
	var hits []string
	more := false
	search := func(abs, rel string) bool {
		if filter != nil && !filter.matches(rel) {
			return true
		}
		if info, err := os.Stat(abs); err != nil || info.Size() > maxFileBytes {
			return true
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return true
		}
		head := data
		if len(head) > 8000 {
			head = head[:8000]
		}
		if strings.IndexByte(string(head), 0) >= 0 {
			return true
		}
		shown := display(tc, abs)
		text := strings.ToValidUTF8(string(data), "\uFFFD")
		for i, line := range splitLines(text) {
			if re.MatchString(line) {
				if len(hits) == limit {
					more = true
					return false
				}
				hits = append(hits, fmt.Sprintf("%s:%d:%s", shown, i+1, truncateLine(line)))
			}
		}
		return true
	}
	if fi.Mode().IsRegular() {
		search(root, filepath.Base(root))
	} else {
		walk(root, search)
	}
	if len(hits) == 0 {
		return "no matches", nil
	}
	out := strings.Join(hits, "\n")
	if more {
		out += fmt.Sprintf("\n…[truncated at %d matches; narrow the pattern or path, or raise max_results]", limit)
	}
	return out, nil
}
