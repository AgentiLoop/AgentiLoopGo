package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AgentiLoop/AgentiLoopGo/core"
)

func call(t *testing.T, tool core.Tool, dir, input string) (string, error) {
	t.Helper()
	return tool.Call(context.Background(), core.ToolContext{Cwd: dir}, json.RawMessage(input))
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteThenReadWithLineNumbers(t *testing.T) {
	d := t.TempDir()
	out, err := call(t, WriteFile{}, d, `{"path":"sub/a.txt","content":"one\ntwo\n"}`)
	if err != nil || !strings.HasPrefix(out, "wrote 8 bytes") {
		t.Fatal(err, out)
	}
	if read(t, d, "sub/a.txt") != "one\ntwo\n" {
		t.Fatal("content")
	}
	out, err = call(t, ReadFile{}, d, `{"path":"sub/a.txt"}`)
	if err != nil || out != "    1\u2502one\n    2\u2502two" {
		t.Fatalf("%v %q", err, out)
	}
}

func TestReadMissingFileFails(t *testing.T) {
	_, err := call(t, ReadFile{}, t.TempDir(), `{"path":"nope.txt"}`)
	if !core.IsToolError(err, core.ErrFailed) {
		t.Fatalf("%#v", err)
	}
}

func TestReadRejectsBadInput(t *testing.T) {
	_, err := call(t, ReadFile{}, t.TempDir(), `{"nope":1}`)
	if !core.IsToolError(err, core.ErrInvalidInput) {
		t.Fatalf("%#v", err)
	}
	_, err = call(t, ReadFile{}, t.TempDir(), `{"path":1}`)
	if !core.IsToolError(err, core.ErrInvalidInput) {
		t.Fatalf("%#v", err)
	}
}

func TestAbsolutePathsAreNotJoinedToCwd(t *testing.T) {
	d := t.TempDir()
	abs, _ := json.Marshal(filepath.Join(d, "abs.txt"))
	if _, err := call(t, WriteFile{}, "/definitely/not/here", `{"path":`+string(abs)+`,"content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if read(t, d, "abs.txt") != "x" {
		t.Fatal("content")
	}
}

func TestEditReplacesSingleMatch(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "f.go"), []byte("func a() {}\nfunc b() {}\n"), 0o644)
	out, err := call(t, EditFile{}, d, `{"path":"f.go","old_string":"func b()","new_string":"func c()"}`)
	if err != nil || !strings.HasPrefix(out, "replaced 1 occurrence") {
		t.Fatal(err, out)
	}
	if read(t, d, "f.go") != "func a() {}\nfunc c() {}\n" {
		t.Fatal(read(t, d, "f.go"))
	}
}

func TestEditRefusesAmbiguousMatchUnlessReplaceAll(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "f.txt"), []byte("x x x"), 0o644)
	_, err := call(t, EditFile{}, d, `{"path":"f.txt","old_string":"x","new_string":"y"}`)
	if err == nil || !strings.Contains(err.Error(), "matched 3 times") {
		t.Fatal(err)
	}
	if read(t, d, "f.txt") != "x x x" {
		t.Fatal("file must be untouched on refusal")
	}
	if _, err := call(t, EditFile{}, d, `{"path":"f.txt","old_string":"x","new_string":"y","replace_all":true}`); err != nil {
		t.Fatal(err)
	}
	if read(t, d, "f.txt") != "y y y" {
		t.Fatal(read(t, d, "f.txt"))
	}
}

func TestEditReportsMissingOldString(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "f.txt"), []byte("abc"), 0o644)
	_, err := call(t, EditFile{}, d, `{"path":"f.txt","old_string":"zzz","new_string":"y"}`)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatal(err)
	}
}

func TestListDirSortsAndMarksDirectories(t *testing.T) {
	d := t.TempDir()
	os.Mkdir(filepath.Join(d, "dir"), 0o755)
	os.WriteFile(filepath.Join(d, "b.txt"), nil, 0o644)
	os.WriteFile(filepath.Join(d, "a.txt"), nil, 0o644)
	if out, err := call(t, ListDir{}, d, `{}`); err != nil || out != "a.txt\nb.txt\ndir/" {
		t.Fatalf("%v %q", err, out)
	}
	if out, err := call(t, ListDir{}, d, `{"path":"dir"}`); err != nil || out != "" {
		t.Fatalf("%v %q", err, out)
	}
}

func TestBashRunsInCwdAndCapturesOutput(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "marker"), nil, 0o644)
	cmd := `ls && echo err 1>&2`
	if runtime.GOOS == "windows" {
		cmd = `dir /b && echo err 1>&2`
	}
	out, err := call(t, Bash{}, d, `{"command":`+quote(cmd)+`}`)
	if err != nil || !strings.HasPrefix(out, "marker") || !strings.Contains(out, "err") || strings.Contains(out, "[exit status") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestBashReportsNonzeroExit(t *testing.T) {
	out, err := call(t, Bash{}, t.TempDir(), `{"command":"exit 3"}`)
	if err != nil || !strings.HasSuffix(out, "[exit status: 3]") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestBashTimesOut(t *testing.T) {
	cmd := "sleep 5"
	if runtime.GOOS == "windows" {
		cmd = "ping -n 6 127.0.0.1 >NUL"
	}
	_, err := call(t, Bash{}, t.TempDir(), `{"command":`+quote(cmd)+`,"timeout_secs":1}`)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatal(err)
	}
}

func TestDefaultRegistryHasAllBuiltins(t *testing.T) {
	r := DefaultRegistry()
	if r.Len() != 7 {
		t.Fatal(r.Len())
	}
	for _, name := range []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "bash"} {
		if _, ok := r.Get(name); !ok {
			t.Fatal("missing", name)
		}
	}
	for name, want := range map[string]bool{"read_file": false, "list_dir": false, "glob": false, "grep": false, "write_file": true, "edit_file": true, "bash": true} {
		if tool, _ := r.Get(name); tool.IsMutating() != want {
			t.Fatalf("%s mutating = %v", name, !want)
		}
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func seedTree(t *testing.T, d string) {
	t.Helper()
	files := map[string]string{
		"src/main.rs":             "fn main() {\n    println!(\"Hello\");\n}\n",
		"src/lib/util.rs":         "pub fn hello() {}\n// TODO: later\n",
		"README.md":               "hello world\n",
		"target/debug/junk.rs":    "fn hello() {}\n",
		"node_modules/x/index.js": "hello\n",
		".git/config":             "hello\n",
		"blob.bin":                "hello\x00\x01\x02",
	}
	for rel, body := range files {
		p := filepath.Join(d, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGlobMatching(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{"*.rs", "main.rs", true}, {"*.rs", "a/b/c/main.rs", true}, {"*.rs", "main.go", false},
		{"Cargo.toml", "crates/x/Cargo.toml", true},
		{"src/*.rs", "src/main.rs", true}, {"src/*.rs", "src/a/main.rs", false},
		{"src/**/*.rs", "src/main.rs", true}, {"src/**/*.rs", "src/a/b/main.rs", true}, {"src/**/*.rs", "lib/main.rs", false},
		{"**/test?.go", "x/test1.go", true},
		{"*.{rs,go}", "a/b.go", true}, {"*.{rs,go}", "b.rs", true}, {"*.{rs,go}", "b.py", false},
		{"{src,lib}/**/x.rs", "lib/a/x.rs", true},
	} {
		if got := newGlob(c.pat).matches(c.path); got != c.want {
			t.Errorf("glob %q vs %q = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
	if got := strings.Join(expandBraces("a{b,c{d,e}}f"), ","); got != "abf,acdf,acef" {
		t.Fatal(got)
	}
	if got := expandBraces("no{braces"); len(got) != 1 || got[0] != "no{braces" {
		t.Fatal(got)
	}
}

func TestGlobFindsFilesAndSkipsVendorDirs(t *testing.T) {
	d := t.TempDir()
	seedTree(t, d)
	for in, want := range map[string]string{
		`{"pattern":"*.rs"}`:                  "src/lib/util.rs\nsrc/main.rs",
		`{"pattern":"src/**/*.{rs,md}"}`:      "src/lib/util.rs\nsrc/main.rs",
		`{"pattern":"*.rs","path":"src/lib"}`: "src/lib/util.rs",
		`{"pattern":"*.zzz"}`:                 "no matches",
	} {
		out, err := call(t, GlobFiles{}, d, in)
		if err != nil || out != want {
			t.Fatalf("%s: %v %q", in, err, out)
		}
	}
	_, err := call(t, GlobFiles{}, d, `{"pattern":"*","path":"nope"}`)
	if !core.IsToolError(err, core.ErrFailed) {
		t.Fatalf("%#v", err)
	}
}

func TestGlobCapsResults(t *testing.T) {
	d := t.TempDir()
	for i := 0; i < 510; i++ {
		if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%03d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := call(t, GlobFiles{}, d, `{"pattern":"*.txt"}`)
	if err != nil || len(strings.Split(out, "\n")) != 501 || !strings.Contains(out, "truncated") {
		t.Fatal(err, len(strings.Split(out, "\n")))
	}
}

func TestGrepReportsPathLineTextAndSkipsBinaryAndVendor(t *testing.T) {
	d := t.TempDir()
	seedTree(t, d)
	for in, want := range map[string]string{
		`{"pattern":"hello"}`: "README.md:1:hello world\nsrc/lib/util.rs:1:pub fn hello() {}",
		`{"pattern":"hello","case_insensitive":true,"glob":"*.rs"}`: "src/lib/util.rs:1:pub fn hello() {}\nsrc/main.rs:2:    println!(\"Hello\");",
		`{"pattern":"TODO","path":"src/lib/util.rs"}`:               "src/lib/util.rs:2:// TODO: later",
		`{"pattern":"nothing-like-this"}`:                           "no matches",
	} {
		out, err := call(t, Grep{}, d, in)
		if err != nil || out != want {
			t.Fatalf("%s: %v %q", in, err, out)
		}
	}
}

func TestGrepLimitsAndValidatesInput(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a.txt"), []byte("x\nx\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, Grep{}, d, `{"pattern":"x","max_results":2}`)
	if err != nil || !strings.HasPrefix(out, "a.txt:1:x\na.txt:2:x\n…[truncated at 2") {
		t.Fatal(err, out)
	}
	_, err = call(t, Grep{}, d, `{"pattern":"("}`)
	if !core.IsToolError(err, core.ErrInvalidInput) {
		t.Fatalf("%#v", err)
	}
	_, err = call(t, Grep{}, d, `{"pattern":"x","path":"missing"}`)
	if !core.IsToolError(err, core.ErrFailed) {
		t.Fatalf("%#v", err)
	}
	_, err = call(t, Grep{}, d, `{}`)
	if !core.IsToolError(err, core.ErrInvalidInput) {
		t.Fatalf("%#v", err)
	}
}

func TestGrepTruncatesLongLines(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a.txt"), []byte(strings.Repeat("y", 1000)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, Grep{}, d, `{"pattern":"y"}`)
	if err != nil || out != "a.txt:1:"+strings.Repeat("y", 300)+"…" {
		t.Fatal(err, out)
	}
}
