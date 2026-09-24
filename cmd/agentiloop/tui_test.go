package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"github.com/gdamore/tcell/v2"
)

func key(k tcell.Key) *tcell.EventKey { return tcell.NewEventKey(k, 0, tcell.ModNone) }
func char(r rune) *tcell.EventKey     { return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone) }

func typeStr(a *App, s string) {
	for _, r := range s {
		a.HandleKey(char(r))
	}
}

// screen renders the app on a simulated w×h terminal and returns its rows.
func screen(t *testing.T, a *App, w, h int) string {
	t.Helper()
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(w, h)
	a.Draw(s)
	s.Show()
	cells, cw, ch := s.(tcell.SimulationScreen).GetContents()
	var sb strings.Builder
	for y := range ch {
		for x := 0; x < cw; x++ {
			c := cells[y*cw+x]
			if len(c.Runes) == 0 {
				continue // trailing half of a wide rune
			}
			sb.WriteString(string(c.Runes))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestEnterSubmitsAndRecordsUserLine(t *testing.T) {
	a := NewApp("s")
	typeStr(a, "hello there")
	act, ok := a.HandleKey(key(tcell.KeyEnter)).(actSubmit)
	if !ok || act.Line != "hello there" || !a.busy || a.input != "" {
		t.Fatalf("%+v", act)
	}
	s := screen(t, a, 40, 8)
	if !strings.Contains(s, "> hello there") || !strings.Contains(s, "Thinking") || !strings.Contains(s, "0s") {
		t.Fatal(s)
	}
}

func TestReplayedSessionShowsAndClearWipes(t *testing.T) {
	a := NewApp("s")
	a.Apply(uiUser{"earlier question"})
	a.Apply(uiEvent{core.EvText{Text: "earlier answer"}})
	if s := screen(t, a, 40, 8); !strings.Contains(s, "> earlier question") || !strings.Contains(s, "earlier answer") {
		t.Fatal(s)
	}
	a.Apply(uiClear{})
	if strings.Contains(screen(t, a, 40, 8), "earlier") {
		t.Fatal("not cleared")
	}
}

func TestEnterIgnoredWhileBusyAndAfterIdle(t *testing.T) {
	a := NewApp("s")
	typeStr(a, "a")
	a.HandleKey(key(tcell.KeyEnter))
	typeStr(a, "b")
	if a.HandleKey(key(tcell.KeyEnter)) != nil {
		t.Fatal("submitted while busy")
	}
	a.Apply(uiIdle{})
	if act, ok := a.HandleKey(key(tcell.KeyEnter)).(actSubmit); !ok || act.Line != "b" {
		t.Fatal(act)
	}
}

func TestSlashExitQuitsAndCtrlCQuits(t *testing.T) {
	a := NewApp("s")
	typeStr(a, "/exit")
	if _, ok := a.HandleKey(key(tcell.KeyEnter)).(actQuit); !ok {
		t.Fatal("/exit")
	}
	if _, ok := NewApp("s").HandleKey(key(tcell.KeyCtrlC)).(actQuit); !ok {
		t.Fatal("ctrl-c")
	}
}

func TestStreamingDeltasAccumulateIntoOneEntry(t *testing.T) {
	a := NewApp("s")
	a.Apply(uiEvent{core.EvTextDelta{Text: "Hel"}})
	a.Apply(uiEvent{core.EvTextDelta{Text: "lo"}})
	a.Apply(uiEvent{core.EvText{Text: "Hello"}})
	if len(a.entries) != 1 || a.entries[0].text != "Hello" {
		t.Fatalf("%+v", a.entries)
	}
	a.Apply(uiEvent{core.EvTextDelta{Text: "again"}})
	if len(a.entries) != 2 {
		t.Fatal(len(a.entries))
	}
}

func TestToolEventsRenderWithMarks(t *testing.T) {
	a := NewApp("s")
	a.Apply(uiEvent{core.EvToolCall{ID: "1", Name: "bash", Input: json.RawMessage(`{"cmd":"ls"}`)}})
	a.Apply(uiEvent{core.EvToolResult{ID: "1", Name: "bash", Output: "boom", IsError: true}})
	s := screen(t, a, 50, 8)
	if !strings.Contains(s, "\U0001f527") || !strings.Contains(s, `bash {"cmd":"ls"}`) || !strings.Contains(s, "✖ boom") {
		t.Fatal(s)
	}
}

func TestReadFilePreviewIsHighlightedAndAligned(t *testing.T) {
	a := NewApp("s")
	a.Apply(uiEvent{core.EvToolCall{ID: "7", Name: "read_file", Input: json.RawMessage(`{"path":"src/a.rs"}`)}})
	a.Apply(uiEvent{core.EvToolResult{ID: "7", Name: "read_file", Output: "    1│fn main() {}\n    2│let x = 1;"}})
	code := a.entries[len(a.entries)-1].code
	if code == nil {
		t.Fatal("expected highlighted entry")
	}
	if fgOf(find(t, code[0], "fn")) != rgb(xcKeyword) || len(a.pendingPaths) != 0 {
		t.Fatal("highlight")
	}
	s := screen(t, a, 40, 8)
	if !strings.Contains(s, "✓     1│fn main() {}") || !strings.Contains(s, "      2│let x = 1;") {
		t.Fatal(s)
	}
}

func TestClickingALinkOpensItAndWheelScrolls(t *testing.T) {
	a := NewApp("s")
	a.Apply(uiEvent{core.EvText{Text: "see [docs](https://a.io/x) now"}})
	if s := screen(t, a, 40, 8); !strings.Contains(s, "see docs now") {
		t.Fatal(s)
	}
	click := func(x, y int) Action { return a.HandleMouse(tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone)) }
	// "docs" occupies columns 4..8 on the first transcript row.
	if act, ok := click(5, 0).(actOpenURL); !ok || act.URL != "https://a.io/x" {
		t.Fatal(act)
	}
	if click(1, 0) != nil || click(5, 1) != nil {
		t.Fatal("miss should not open")
	}
	if a.HandleMouse(tcell.NewEventMouse(0, 0, tcell.WheelUp, tcell.ModNone)) != nil || a.scroll != 3 {
		t.Fatal(a.scroll)
	}
}

func TestLongLinesWrapAndBottomFollows(t *testing.T) {
	a := NewApp("s")
	for i := range 30 {
		a.Apply(uiLine{"line " + strconv.Itoa(i) + " " + strings.Repeat("x", 60)})
	}
	s := screen(t, a, 30, 10)
	if !strings.Contains(s, "line 29") || strings.Contains(s, "line 0 ") {
		t.Fatal(s)
	}
	a.HandleKey(key(tcell.KeyPgUp))
	if s2 := screen(t, a, 30, 10); strings.Contains(s2, "line 29") {
		t.Fatal(s2)
	}
}

func TestBlankLineInTextRendersAsOneBlankRow(t *testing.T) {
	a := NewApp("s")
	a.Apply(uiEvent{core.EvText{Text: "para one\n\npara two"}})
	rowsOut := strings.Split(screen(t, a, 20, 8), "\n")
	one, two := -1, -1
	for i, r := range rowsOut {
		switch strings.TrimRight(r, " ") {
		case "para one":
			one = i
		case "para two":
			two = i
		}
	}
	if one < 0 || two-one != 2 {
		t.Fatalf("expected exactly one blank row between paragraphs:\n%s", strings.Join(rowsOut, "\n"))
	}
}

func TestEscInPermissionModalCancelsJustThatCall(t *testing.T) {
	a := NewApp("s")
	req := &PermissionRequest{Tool: "bash", Input: "{}", Reply: make(chan Answer, 1)}
	a.Apply(uiPermission{req})
	if !strings.Contains(screen(t, a, 70, 12), "[esc] skip") {
		t.Fatal("modal")
	}
	if a.HandleKey(key(tcell.KeyEscape)) != nil || a.modal != nil || a.Quit() {
		t.Fatal("esc")
	}
	if <-req.Reply != AnswerCancel {
		t.Fatal("answer")
	}
}

func TestPermissionModalAnswersAndCloses(t *testing.T) {
	a := NewApp("s")
	req := &PermissionRequest{Tool: "bash", Input: `{"cmd": "rm x"}`, Reply: make(chan Answer, 1)}
	a.Apply(uiPermission{req})
	s := screen(t, a, 60, 12)
	if !strings.Contains(s, "bash wants to run") || !strings.Contains(s, "[y]es") {
		t.Fatal(s)
	}
	// Unrelated keys keep the modal up and don't reach the input.
	a.HandleKey(char('z'))
	if a.modal == nil || a.input != "" {
		t.Fatal("z leaked")
	}
	a.HandleKey(char('a'))
	if a.modal != nil || <-req.Reply != AnswerAlways {
		t.Fatal("always")
	}
}

func TestChannelPolicyRoundTripAndAlways(t *testing.T) {
	q := newUIQueue()
	p := NewChannelPolicy(q)
	ctx := context.Background()
	nextReq := func() *PermissionRequest {
		for range 200 {
			msgs, _ := q.Drain()
			for _, m := range msgs {
				if r, ok := m.(uiPermission); ok {
					return r.Req
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("expected permission request")
		return nil
	}
	res := make(chan core.Permission, 1)
	go func() { res <- p.Check(ctx, "bash", true, json.RawMessage(`{}`)) }()
	req := nextReq()
	if req.Tool != "bash" {
		t.Fatal(req.Tool)
	}
	req.Reply <- AnswerAlways
	if <-res != core.Allow {
		t.Fatal("allow")
	}
	// Remembered: no second prompt.
	if p.Check(ctx, "bash", true, nil) != core.Allow {
		t.Fatal("always")
	}
	if msgs, _ := q.Drain(); len(msgs) != 0 {
		t.Fatal("prompted again")
	}
	// Non-mutating never asks.
	if p.Check(ctx, "read_file", false, nil) != core.Allow {
		t.Fatal("read-only")
	}
	// Dropped reply → deny.
	go func() { res <- p.Check(ctx, "write_file", true, nil) }()
	close(nextReq().Reply)
	if <-res != core.Deny {
		t.Fatal("deny")
	}
	// Closed UI → deny without blocking.
	q.Close()
	if p.Check(ctx, "edit_file", true, nil) != core.Deny {
		t.Fatal("closed queue")
	}
}

func TestHistoryPersistsAcrossLaunches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "history.txt")
	a := NewApp("s").WithHistoryFile(path)
	typeStr(a, `first \ prompt`)
	a.HandleKey(key(tcell.KeyEnter))
	a.Apply(uiIdle{})
	typeStr(a, "second")
	a.HandleKey(key(tcell.KeyEnter))
	if b, _ := os.ReadFile(path); string(b) != "#V2\nfirst \\\\ prompt\nsecond\n" {
		t.Fatalf("%q", b)
	}
	// A fresh launch recalls both, newest first.
	a2 := NewApp("s").WithHistoryFile(path)
	a2.HandleKey(key(tcell.KeyUp))
	if a2.input != "second" {
		t.Fatal(a2.input)
	}
	a2.HandleKey(key(tcell.KeyUp))
	if a2.input != `first \ prompt` {
		t.Fatal(a2.input)
	}
}

func TestStatusUpdateReplacesModelInBar(t *testing.T) {
	a := NewApp(" anthropic  claude-fable-5-1 ")
	a.Apply(uiStatus{" anthropic  claude-opus-5-5 "})
	if s := screen(t, a, 120, 8); !strings.Contains(s, "claude-opus-5-5") || strings.Contains(s, "claude-fable-5-1") {
		t.Fatal(s)
	}
}

func TestStatusBarShowsTokensPerSecond(t *testing.T) {
	a := NewApp("S")
	ft := uint64(500)
	a.Apply(uiEvent{core.EvTurnComplete{InputTokens: 10, OutputTokens: 100, ElapsedMs: 2500, FirstTokenMs: &ft}})
	if s := screen(t, a, 120, 8); !strings.Contains(s, "50.0 tok/s · ttft 0.50s · 100 tok in 2.5s") {
		t.Fatal(s)
	}
}

func TestHistoryRecallUpDown(t *testing.T) {
	a := NewApp("s")
	for _, l := range []string{"one", "two"} {
		typeStr(a, l)
		a.HandleKey(key(tcell.KeyEnter))
		a.Apply(uiIdle{})
	}
	for _, step := range []struct {
		k    tcell.Key
		want string
	}{{tcell.KeyUp, "two"}, {tcell.KeyUp, "one"}, {tcell.KeyDown, "two"}, {tcell.KeyDown, ""}} {
		a.HandleKey(key(step.k))
		if a.input != step.want {
			t.Fatalf("%v → %q", step.k, a.input)
		}
	}
}

func TestBusyTitleAnimatesWithElapsedTime(t *testing.T) {
	a := NewApp("s")
	start := time.Now()
	a.now = func() time.Time { return start }
	typeStr(a, "go")
	a.HandleKey(key(tcell.KeyEnter))
	a.now = func() time.Time { return start.Add(75 * time.Second) }
	title := a.busyTitle().Text()
	if !strings.Contains(title, "Thinking") || !strings.Contains(title, "1m 15s") {
		t.Fatal(title)
	}
	a.Apply(uiEvent{core.EvToolCall{ID: "1", Name: "bash", Input: json.RawMessage(`{}`)}})
	if !strings.Contains(a.busyTitle().Text(), "Running bash") {
		t.Fatal(a.busyTitle().Text())
	}
}

func TestInputEditingKeys(t *testing.T) {
	a := NewApp("s")
	typeStr(a, "héllo")
	a.HandleKey(key(tcell.KeyLeft))
	a.HandleKey(key(tcell.KeyBackspace2))
	if a.input != "hélo" {
		t.Fatal(a.input)
	}
	a.HandleKey(key(tcell.KeyHome))
	a.HandleKey(key(tcell.KeyDelete))
	if a.input != "élo" {
		t.Fatal(a.input)
	}
	a.HandleKey(key(tcell.KeyCtrlU))
	if a.input != "" || a.cursor != 0 {
		t.Fatal(a.input)
	}
}
