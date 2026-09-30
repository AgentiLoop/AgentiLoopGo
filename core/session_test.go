package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	. "github.com/AgentiLoop/AgentiLoopGo/core"
)

func sampleHistory() []Message {
	return []Message{
		UserText("fix the failing test\nplease"),
		{Role: RoleAssistant, Content: []ContentBlock{ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"go test"}`))}},
		ToolResults([]ContentBlock{ToolResultBlock("t1", "ok", false)}),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("done")}},
	}
}

func TestSessionSaveThenLoadRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s := NewSession("/proj", "openai", "qwen3:4b")
	s.History = sampleHistory()
	path, err := s.Save(dir)
	if err != nil || !strings.HasSuffix(path, s.ID+".json") {
		t.Fatal(err, path)
	}
	if _, err := os.Stat(filepath.Join(dir, "."+s.ID+".json.tmp")); err == nil {
		t.Fatal("temp file must be renamed away")
	}
	back, err := LoadSession(dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != s.ID || back.Cwd != "/proj" || back.Provider != "openai" || back.Model != "qwen3:4b" || len(back.History) != 4 {
		t.Fatalf("%+v", back)
	}
	if len(back.History[1].ToolUses()) != 1 || back.History[3].Text() != "done" {
		t.Fatal("history mismatch")
	}
}

func TestSessionJSONMatchesRustFormat(t *testing.T) {
	data, _ := json.Marshal(sampleHistory()[1:3])
	want := `[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"go test"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]`
	if string(data) != want {
		t.Fatalf("\n got %s\nwant %s", data, want)
	}
}

func TestSessionTitleIsFirstUserLineTruncated(t *testing.T) {
	s := NewSession("/p", "a", "m")
	if s.Title() != "" {
		t.Fatal(s.Title())
	}
	s.History = sampleHistory()
	if s.Title() != "fix the failing test" {
		t.Fatal(s.Title())
	}
	s.History = []Message{UserText(strings.Repeat("x", 100))}
	title := s.Title()
	if utf8.RuneCountInString(title) != 61 || !strings.HasSuffix(title, "…") {
		t.Fatal(title)
	}
}

func TestListSessionsNewestFirstSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	older := NewSession("/a", "p", "m")
	older.History = []Message{UserText("older")}
	older.Save(dir)
	newer := NewSession("/b", "p", "m")
	newer.ID = "newer"
	newer.History = []Message{UserText("newer")}
	newer.Save(dir)
	newer.Updated = older.Updated + 10
	data, _ := json.Marshal(newer)
	os.WriteFile(SessionPath(dir, "newer"), data, 0o644)
	os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{not json"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)
	os.WriteFile(filepath.Join(dir, ".hidden.json.tmp"), []byte("{}"), 0o644)

	all, err := ListSessions(dir)
	if err != nil || len(all) != 2 || all[0].ID != "newer" || all[1].ID != older.ID {
		t.Fatalf("%v %+v", err, all)
	}
}

func TestListSessionsOnMissingDirIsEmpty(t *testing.T) {
	all, err := ListSessions(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(all) != 0 {
		t.Fatal(err, all)
	}
}

func TestLatestSessionForMatchesCwd(t *testing.T) {
	dir := t.TempDir()
	a := NewSession("/a", "p", "m")
	a.Save(dir)
	b := NewSession("/b", "p", "m")
	b.ID = "b"
	b.Save(dir)
	if s, _ := LatestSessionFor(dir, "/a"); s == nil || s.ID != a.ID {
		t.Fatal(s)
	}
	if s, _ := LatestSessionFor(dir, "/b"); s == nil || s.ID != "b" {
		t.Fatal(s)
	}
	if s, _ := LatestSessionFor(dir, "/zzz"); s != nil {
		t.Fatal(s)
	}
}

func TestLoadMissingSessionErrorsWithID(t *testing.T) {
	_, err := LoadSession(t.TempDir(), "nope")
	if err == nil || !strings.Contains(err.Error(), "no session nope") {
		t.Fatal(err)
	}
}

func TestMarkdownExportHasSectionsToolsAndSafeFences(t *testing.T) {
	s := NewSession("/work/app", "anthropic", "m1")
	s.ID = "S1"
	s.History = []Message{
		UserText("Fix the bug\nplease"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("Looking."), ToolUseBlock("t1", "read_file", json.RawMessage(`{"path": "a.rs"}`))}},
		ToolResults([]ContentBlock{ToolResultBlock("t1", "code with ``` inside", false)}),
		{Role: RoleAssistant, Content: []ContentBlock{ToolUseBlock("t2", "bash", json.RawMessage(`{"command":"x"}`))}},
		ToolResults([]ContentBlock{ToolResultBlock("t2", strings.Repeat("y", 2500), true)}),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("Done.")}},
	}
	md := s.ToMarkdown()
	head := "# Fix the bug\n\n- Session: S1\n- Provider: anthropic · Model: m1\n- Directory: /work/app\n\n## You\n\nFix the bug\nplease\n\n## AgentiLoop\n\nLooking.\n\n**Tool: `read_file`**\n\n```json\n{\"path\":\"a.rs\"}\n```\n\n**Result**\n\n````\ncode with ``` inside\n````\n"
	if !strings.HasPrefix(md, head) {
		t.Fatal(md)
	}
	if strings.Count(md, "## AgentiLoop") != 1 {
		t.Fatal("section header repeated")
	}
	if !strings.Contains(md, "**Error**") || !strings.Contains(md, strings.Repeat("y", 2000)+"\n…[truncated]") || !strings.HasSuffix(md, "\nDone.\n") {
		t.Fatal(md)
	}
	if !strings.HasPrefix(NewSession("/x", "p", "m").ToMarkdown(), "# AgentiLoop session\n") {
		t.Fatal("empty title")
	}
}
