package core

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMentionCandidatesNeedALeadingAtAndDropPunctuation(t *testing.T) {
	got := MentionCandidates("look at @src/main.rs, then @README.md. mail bob@x.com (@docs) @@ @")
	if want := []string{"src/main.rs", "README.md", "docs"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := MentionCandidates("@a @a @b"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
	if got := MentionCandidates("nothing here"); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestExpandMentionsAttachesFilesAndFoldersAndSkipsUnknown(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "src", "main.rs"), []byte("fn main() {}\nfn b() {}\n"), 0o644)
	os.WriteFile(filepath.Join(d, "src", "lib.rs"), nil, 0o644)
	out, att := ExpandMentions("explain @src/main.rs and list @src; ignore @nobody", d)
	if !strings.HasPrefix(out, "explain @src/main.rs and list @src; ignore @nobody\n\n") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "<file path=\"src/main.rs\">\n   1\tfn main() {}\n   2\tfn b() {}\n</file>") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "<folder path=\"src\">\nlib.rs\nmain.rs\n</folder>") || strings.Contains(out, "nobody\">") {
		t.Fatal(out)
	}
	if len(att) != 2 || att[0].Lines != 2 || att[0].IsDir || att[1].Lines != 2 || !att[1].IsDir {
		t.Fatalf("%+v", att)
	}
	if DescribeAttachment(att[0]) != "📎 src/main.rs (2 lines)" || DescribeAttachment(att[1]) != "📎 src/ (2 entries)" {
		t.Fatal(DescribeAttachment(att[0]), DescribeAttachment(att[1]))
	}
	if same, none := ExpandMentions("no mentions", d); same != "no mentions" || len(none) != 0 {
		t.Fatal(same, none)
	}
}

func TestExpandMentionsCutsBigFiles(t *testing.T) {
	d := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 10_000; i++ {
		sb.WriteString("line ")
		sb.WriteString(strings.Repeat("x", i%7))
		sb.WriteString("\n")
	}
	os.WriteFile(filepath.Join(d, "big.txt"), []byte(sb.String()), 0o644)
	out, att := ExpandMentions("@big.txt", d)
	if !strings.Contains(out, "… cut at 65536 bytes") || att[0].Lines >= 10_000 {
		t.Fatal(out[len(out)-200:], att)
	}
}
