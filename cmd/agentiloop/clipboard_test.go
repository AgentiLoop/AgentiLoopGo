package main

import (
	"bytes"
	"reflect"
	"testing"
)

func TestEachOSHasCopyToolsInPreferenceOrder(t *testing.T) {
	if got := clipboardTools("darwin")[0][0]; got != "pbcopy" {
		t.Fatalf("darwin: %s", got)
	}
	if got := clipboardTools("windows")[0][0]; got != "clip" {
		t.Fatalf("windows: %s", got)
	}
	var linux []string
	for _, tool := range clipboardTools("linux") {
		linux = append(linux, tool[0])
	}
	if want := []string{"wl-copy", "xclip", "xsel"}; !reflect.DeepEqual(linux, want) {
		t.Fatalf("linux: %v, want %v", linux, want)
	}
	if n := len(clipboardTools("freebsd")); n != 3 {
		t.Fatalf("freebsd: %d tools", n)
	}
}

func TestClipGetsUTF16WithABOMAndOthersUTF8(t *testing.T) {
	if got, want := clipboardInput("clip", "aé"), []byte{0xFF, 0xFE, 'a', 0, 0xE9, 0}; !bytes.Equal(got, want) {
		t.Fatalf("clip: %v, want %v", got, want)
	}
	if got := clipboardInput("pbcopy", "aé"); string(got) != "aé" {
		t.Fatalf("pbcopy: %q", got)
	}
}

func TestOSC52WrapsBase64OfTheText(t *testing.T) {
	if got := osc52("ab"); got != "\x1b]52;c;YWI=\x07" {
		t.Fatalf("osc52 = %q", got)
	}
}

func TestCopiedLineNamesTheReplyAndHowItWasCopied(t *testing.T) {
	if got, want := copiedLine(1, 42, "pbcopy"), "copied the last reply (42 characters) to the clipboard via pbcopy"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := copiedLine(3, 7, "the terminal (OSC 52)"), "copied reply #3 from the end (7 characters) to the clipboard via the terminal (OSC 52)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
