package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPatchAddUpdateDeleteAndMove(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "a.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644)
	os.WriteFile(filepath.Join(d, "gone.txt"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(d, "old.txt"), []byte("keep\nfix me\n"), 0o644)
	patch := `*** Begin Patch
*** Add File: sub/new.txt
+hello
+world
*** Update File: a.txt
@@
 one
-two
+TWO
 three
@@
-four
+FOUR
*** Delete File: gone.txt
*** Update File: old.txt
*** Move to: moved.txt
@@
 keep
-fix me
+fixed
*** End Patch`
	out, err := Apply(patch, d)
	if err != nil {
		t.Fatal(err)
	}
	if out != "A sub/new.txt\nM a.txt\nD gone.txt\nR old.txt -> moved.txt" {
		t.Fatal(out)
	}
	if readT(t, filepath.Join(d, "sub/new.txt")) != "hello\nworld\n" || readT(t, filepath.Join(d, "a.txt")) != "one\nTWO\nthree\nFOUR\n" {
		t.Fatal("content")
	}
	for _, gone := range []string{"gone.txt", "old.txt"} {
		if _, err := os.Stat(filepath.Join(d, gone)); err == nil {
			t.Fatal(gone, "still exists")
		}
	}
	if readT(t, filepath.Join(d, "moved.txt")) != "keep\nfixed\n" {
		t.Fatal("moved")
	}
}

func TestPatchAnchorPicksTheRightOccurrence(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "f.rs")
	os.WriteFile(f, []byte("fn a() {\n    x();\n}\nfn b() {\n    x();\n}\n"), 0o644)
	if _, err := Apply("*** Begin Patch\n*** Update File: f.rs\n@@ fn b() {\n-    x();\n+    y();\n*** End Patch", d); err != nil {
		t.Fatal(err)
	}
	if got := readT(t, f); got != "fn a() {\n    x();\n}\nfn b() {\n    y();\n}\n" {
		t.Fatal(got)
	}
}

func TestPatchMismatchWritesNothing(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "a.txt"), []byte("one\n"), 0o644)
	_, err := Apply("*** Begin Patch\n*** Add File: b.txt\n+b\n*** Update File: a.txt\n@@\n-nope\n+x\n*** End Patch", d)
	if err == nil || !strings.Contains(err.Error(), "a.txt: hunk does not match") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d, "b.txt")); err == nil {
		t.Fatal("earlier Add File must not be written")
	}
	if readT(t, filepath.Join(d, "a.txt")) != "one\n" {
		t.Fatal("a.txt changed")
	}
}

func TestPatchRejectsMalformedPatches(t *testing.T) {
	d := t.TempDir()
	for patch, want := range map[string]string{
		"no header":                      "Begin Patch",
		"*** Begin Patch\n*** End Patch": "no file changes",
		"*** Begin Patch\n*** Delete File: nope\n*** End Patch": "no such file",
		"*** Begin Patch\n*** Update File: nope\n*** End Patch": "cannot read nope",
	} {
		if _, err := Apply(patch, d); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(patch, err)
		}
	}
}

func TestPatchTrailingWhitespaceIsToleratedWhenMatching(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "a.txt")
	os.WriteFile(f, []byte("a  \nb\n"), 0o644)
	if _, err := Apply("*** Begin Patch\n*** Update File: a.txt\n@@\n a\n-b\n+c\n*** End Patch", d); err != nil {
		t.Fatal(err)
	}
	if got := readT(t, f); got != "a  \nc\n" {
		t.Fatalf("%q", got)
	}
}
