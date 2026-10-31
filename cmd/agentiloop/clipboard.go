package main

// /copy: put text on the system clipboard — the platform's copy tool when
// one runs, else an OSC 52 escape so the terminal (also over SSH) copies it.

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode/utf16"
)

// clipboardTools are the copy tools to try in order on goos: program and its arguments.
func clipboardTools(goos string) [][]string {
	switch goos {
	case "darwin":
		return [][]string{{"pbcopy"}}
	case "windows":
		return [][]string{{"clip"}}
	default:
		return [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}}
	}
}

// clipboardInput is what a copy tool reads on stdin: clip.exe wants UTF-16LE
// with a BOM to keep non-ASCII text.
func clipboardInput(program, text string) []byte {
	if program != "clip" {
		return []byte(text)
	}
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(text)) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// osc52 is the OSC 52 "set clipboard" sequence for text.
func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}

// copyText copies text and says how: the tool that took it, or "the terminal (OSC 52)".
func copyText(text string) string {
	for _, tool := range clipboardTools(runtime.GOOS) {
		cmd := exec.Command(tool[0], tool[1:]...)
		cmd.Stdin = strings.NewReader(string(clipboardInput(tool[0], text)))
		if cmd.Run() == nil {
			return tool[0]
		}
	}
	os.Stderr.WriteString(osc52(text))
	return "the terminal (OSC 52)"
}
