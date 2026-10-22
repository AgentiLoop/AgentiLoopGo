package core

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// @path mentions: a word starting with "@" in a prompt that names an existing file or folder
// (relative to the project folder, or absolute) attaches it to the prompt the model receives —
// the file's text with line numbers, the folder's entries, or an image (png/jpeg/gif/webp) as an
// image block. The typed text is kept as is; the attachments follow it. Words that name nothing
// (@alice, an e-mail address) are left alone.

const (
	// MentionFileLimit: files longer than this are cut (bytes).
	MentionFileLimit = 64 * 1024
	// MentionDirLimit: folders list at most this many entries.
	MentionDirLimit = 200
	// MentionImageLimit: images larger than this (bytes) are not attached (the Anthropic API limit).
	MentionImageLimit = 5 * 1024 * 1024
)

// Attachment is one attached path, for the UI.
type Attachment struct {
	// Mention as typed, without the "@".
	Mention string
	Path    string
	// Lines of a file, entries of a folder, bytes of an image.
	Lines int
	IsDir bool
	// MediaType and Data are set for an image: it goes to the model as an image block, not as text.
	MediaType string
	Data      string
}

// ImageMediaType is the media type of an image file, by extension ("" for anything else).
func ImageMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

const mentionLead = "([{\"'<"
const mentionTrail = ",.;:)]}\"'?!>"

// MentionCandidates are the candidate mentions in text: "@" at the start or after whitespace,
// then non-whitespace, with surrounding punctuation trimmed. Duplicates are kept once, in order.
func MentionCandidates(text string) []string {
	var out []string
	for _, word := range strings.Fields(text) {
		word = strings.TrimLeft(word, mentionLead)
		rest, ok := strings.CutPrefix(word, "@")
		if !ok {
			continue
		}
		rest = strings.TrimRight(rest, mentionTrail)
		if rest == "" || strings.HasPrefix(rest, "@") {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o == rest
		}
		if !dup {
			out = append(out, rest)
		}
	}
	return out
}

func resolveMention(cwd, mention string) string {
	if filepath.IsAbs(mention) {
		return mention
	}
	if rest, ok := strings.CutPrefix(mention, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return filepath.Join(cwd, mention)
}

// ExpandMentions returns the prompt with every mentioned file or folder appended, plus what was attached.
func ExpandMentions(text, cwd string) (string, []Attachment) {
	var sb strings.Builder
	sb.WriteString(text)
	var attached []Attachment
	for _, mention := range MentionCandidates(text) {
		path := resolveMention(cwd, mention)
		meta, err := os.Stat(path)
		if err != nil {
			continue
		}
		if meta.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				continue
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			sort.Strings(names)
			total := len(names)
			if total > MentionDirLimit {
				names = names[:MentionDirLimit]
			}
			body := strings.Join(names, "\n")
			if total > MentionDirLimit {
				body += fmt.Sprintf("\n… %d more entries", total-MentionDirLimit)
			}
			fmt.Fprintf(&sb, "\n\n<folder path=\"%s\">\n%s\n</folder>", mention, body)
			attached = append(attached, Attachment{Mention: mention, Path: path, Lines: total, IsDir: true})
		} else if mt := ImageMediaType(path); mt != "" && meta.Mode().IsRegular() {
			if meta.Size() > MentionImageLimit {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			attached = append(attached, Attachment{Mention: mention, Path: path, Lines: len(data), MediaType: mt, Data: base64.StdEncoding.EncodeToString(data)})
		} else if meta.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			cut := len(data) > MentionFileLimit
			if cut {
				data = data[:MentionFileLimit]
			}
			content := strings.ToValidUTF8(string(data), "\uFFFD")
			var body strings.Builder
			lines := 0
			for i, l := range strings.SplitAfter(content, "\n") {
				if l == "" {
					continue
				}
				fmt.Fprintf(&body, "%4d\t%s", i+1, strings.TrimRight(l, "\r\n"))
				body.WriteByte('\n')
				lines = i + 1
			}
			if cut {
				fmt.Fprintf(&body, "… cut at %d bytes (%d bytes in all)\n", MentionFileLimit, meta.Size())
			}
			fmt.Fprintf(&sb, "\n\n<file path=\"%s\">\n%s</file>", mention, body.String())
			attached = append(attached, Attachment{Mention: mention, Path: path, Lines: lines})
		}
	}
	return sb.String(), attached
}

// MentionContent is the user turn for the model: the expanded text, then one image block per attached image.
func MentionContent(text string, attached []Attachment) []ContentBlock {
	blocks := []ContentBlock{TextBlock(text)}
	for _, a := range attached {
		if a.MediaType != "" {
			blocks = append(blocks, ImageBlock(a.MediaType, a.Data))
		}
	}
	return blocks
}

// DescribeAttachment is one line for the UI: "📎 src/main.rs (120 lines)".
func DescribeAttachment(a Attachment) string {
	if a.IsDir {
		return fmt.Sprintf("📎 %s/ (%d entries)", strings.TrimRight(a.Mention, "/"), a.Lines)
	}
	if a.MediaType != "" {
		return fmt.Sprintf("📎 %s (%s, %d KB)", a.Mention, a.MediaType, (a.Lines+1023)/1024)
	}
	return fmt.Sprintf("📎 %s (%d lines)", a.Mention, a.Lines)
}
