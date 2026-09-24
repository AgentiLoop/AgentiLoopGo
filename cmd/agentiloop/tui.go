package main

// Full-screen terminal UI (--tui): a scrolling transcript, an input line, and a
// status bar. The agent loop runs in its own goroutine and talks to the UI over
// a queue; permission prompts appear as a modal.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// ---- messages between the agent goroutine and the UI --------------------------------

// UiMsg is a message from the agent side to the UI.
type UiMsg interface{ isUiMsg() }

type (
	uiEvent struct{ Ev core.Event }
	// uiLine is a plain informational line (slash command output).
	uiLine       struct{ Text string }
	uiError      struct{ Text string }
	uiPermission struct{ Req *PermissionRequest }
	// uiStatus replaces the status-bar text (model or session changed).
	uiStatus struct{ Text string }
	// uiIdle: the agent finished the current prompt / command.
	uiIdle struct{}
	// uiUser is a user prompt from a resumed session being replayed.
	uiUser struct{ Text string }
	// uiClear wipes the transcript (a different session was loaded, or /clear).
	uiClear struct{}
)

func (uiEvent) isUiMsg()      {}
func (uiLine) isUiMsg()       {}
func (uiError) isUiMsg()      {}
func (uiPermission) isUiMsg() {}
func (uiStatus) isUiMsg()     {}
func (uiIdle) isUiMsg()       {}
func (uiUser) isUiMsg()       {}
func (uiClear) isUiMsg()      {}

// uiQueue is an unbounded, closable queue from the agent goroutine to the UI.
type uiQueue struct {
	mu     sync.Mutex
	items  []UiMsg
	closed bool
	wake   chan struct{}
}

func newUIQueue() *uiQueue { return &uiQueue{wake: make(chan struct{}, 1)} }

// Send enqueues msg; false once the queue is closed.
func (q *uiQueue) Send(msg UiMsg) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.items = append(q.items, msg)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// Drain returns queued messages and whether the sender side has closed.
func (q *uiQueue) Drain() ([]UiMsg, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items, q.closed
}

func (q *uiQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

type Answer int

const (
	AnswerAllow Answer = iota
	AnswerAlways
	AnswerDeny
	// AnswerCancel (Esc): skip this call, keep the agent running.
	AnswerCancel
)

type PermissionRequest struct {
	Tool, Input string
	// Reply receives exactly one answer; closing it without an answer means deny.
	Reply chan Answer
}

// ChannelPolicy asks the UI and waits for its answer.
type ChannelPolicy struct {
	q      *uiQueue
	mu     sync.Mutex
	always map[string]bool
}

func NewChannelPolicy(q *uiQueue) *ChannelPolicy {
	return &ChannelPolicy{q: q, always: map[string]bool{}}
}

func (p *ChannelPolicy) Check(ctx context.Context, tool string, mutating bool, input json.RawMessage) core.Permission {
	p.mu.Lock()
	known := p.always[tool]
	p.mu.Unlock()
	if !mutating || known {
		return core.Allow
	}
	req := &PermissionRequest{Tool: tool, Input: prettyJSON(input), Reply: make(chan Answer, 1)}
	if !p.q.Send(uiPermission{req}) {
		return core.Deny
	}
	select {
	case a, ok := <-req.Reply:
		switch {
		case !ok:
			return core.Deny
		case a == AnswerAllow:
			return core.Allow
		case a == AnswerAlways:
			p.mu.Lock()
			p.always[tool] = true
			p.mu.Unlock()
			return core.Allow
		case a == AnswerCancel:
			return core.Cancel
		}
		return core.Deny
	case <-ctx.Done():
		return core.Deny
	}
}

// ---- UI state --------------------------------------------------------------------

type entryKind int

const (
	kindUser entryKind = iota
	kindAssistant
	kindTool
	kindToolError
	kindInfo
	kindError
)

type entry struct {
	kind entryKind
	text string
	// code holds pre-styled lines (syntax-highlighted code); when set, text is ignored.
	code []Line
}

type linkHit struct {
	x0, x1, y int
	url       string
}

// Action is the result of a key press or mouse event that the event loop must act on.
type Action interface{ isAction() }

type (
	actSubmit  struct{ Line string }
	actQuit    struct{}
	actOpenURL struct{ URL string }
)

func (actSubmit) isAction()  {}
func (actQuit) isAction()    {}
func (actOpenURL) isAction() {}

// App is the UI state; backend-agnostic (any tcell.Screen) so tests can drive it.
type App struct {
	entries []entry
	input   string
	// cursor is the byte offset within input.
	cursor int
	// scroll is lines scrolled up from the bottom (0 = follow output).
	scroll int
	busy   bool
	// busySince is when the current prompt started, for the spinner and elapsed time.
	busySince time.Time
	// activity is what the agent is doing right now, shown next to the spinner.
	activity string
	quit     bool
	modal    *PermissionRequest
	status   string
	// speed of the most recent model call, shown in the status bar.
	speed       string
	history     []string
	histIdx     int // -1 = not browsing
	historyFile string
	// streaming: the last entry is assistant text still being streamed.
	streaming bool
	// pendingPaths: path argument of in-flight read_file calls, by tool-call id.
	pendingPaths map[string]string
	// linkHits: screen cells occupied by links in the last frame, for click handling.
	linkHits []linkHit
	now      func() time.Time
}

func NewApp(status string) *App {
	return &App{status: status, histIdx: -1, pendingPaths: map[string]string{}, busySince: time.Now(), now: time.Now}
}

// WithHistoryFile loads prompt history from path and keeps appending to it, so ↑
// recalls prompts from earlier launches (TUI and REPL share the file).
func (a *App) WithHistoryFile(path string) *App {
	if path != "" {
		a.history = loadHistory(path)
	}
	a.historyFile = path
	return a
}

func (a *App) Quit() bool { return a.quit }

func (a *App) push(kind entryKind, text string) {
	a.streaming = false
	a.entries = append(a.entries, entry{kind: kind, text: text})
}

func (a *App) Apply(msg UiMsg) {
	switch m := msg.(type) {
	case uiEvent:
		a.applyEvent(m.Ev)
	case uiLine:
		a.push(kindInfo, m.Text)
	case uiError:
		a.push(kindError, m.Text)
	case uiPermission:
		a.activity = "Waiting for approval"
		a.modal = m.Req
	case uiStatus:
		a.status = m.Text
	case uiIdle:
		a.busy = false
	case uiUser:
		a.push(kindUser, m.Text)
	case uiClear:
		a.entries = nil
		a.streaming = false
		a.pendingPaths = map[string]string{}
		a.scroll = 0
	}
}

func (a *App) applyEvent(ev core.Event) {
	switch e := ev.(type) {
	case core.EvTextDelta:
		a.activity = "Writing"
	case core.EvToolCall:
		a.activity = "Running " + e.Name
	case core.EvCompacted:
		a.activity = "Compacting"
	default:
		a.activity = "Thinking"
	}
	switch e := ev.(type) {
	case core.EvTextDelta:
		if !a.streaming {
			a.push(kindAssistant, "")
			a.streaming = true
		}
		a.entries[len(a.entries)-1].text += e.Text
	case core.EvText:
		// Deltas already built the text; just close the streaming entry.
		if !a.streaming {
			a.push(kindAssistant, e.Text)
		}
		a.streaming = false
	case core.EvToolCall:
		if e.Name == "read_file" {
			var in struct {
				Path *string `json:"path"`
			}
			if json.Unmarshal(e.Input, &in) == nil && in.Path != nil {
				a.pendingPaths[e.ID] = *in.Path
			}
		}
		a.push(kindTool, "\U0001f527 "+e.Name+" "+compactJSON(e.Input))
	case core.EvToolResult:
		path, hasPath := a.pendingPaths[e.ID]
		delete(a.pendingPaths, e.ID)
		head := firstLines(e.Output, 8)
		kind, mark := kindTool, "✓"
		if e.IsError {
			kind, mark = kindToolError, "✖"
		}
		// Numbered read_file output gets syntax-highlighted by extension.
		if !e.IsError && hasPath {
			if code := highlightNumbered(head, path); code != nil {
				markStyle := fg(tcell.ColorOlive)
				for i := range code {
					m := "  "
					if i == 0 {
						m = mark + " "
					}
					code[i] = append(Line{styled(m, markStyle)}, code[i]...)
				}
				a.streaming = false
				a.entries = append(a.entries, entry{kind: kind, code: code})
				return
			}
		}
		// Continuation lines are indented past the mark so multi-line output
		// (e.g. numbered file contents) stays column-aligned.
		a.push(kind, mark+" "+strings.Join(head, "\n  "))
	case core.EvTurnComplete:
		slog.Debug("turn", "input_tokens", e.InputTokens, "output_tokens", e.OutputTokens, "elapsed_ms", e.ElapsedMs)
		a.speed = speedLine(e.OutputTokens, e.ElapsedMs, e.FirstTokenMs)
	case core.EvCompacted:
		a.push(kindInfo, compactedLine(e.BeforeTokens, e.MessagesDropped))
	}
}

// firstLines returns up to n lines (Rust str::lines semantics).
func firstLines(s string, n int) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func (a *App) HandleKey(ev *tcell.EventKey) Action {
	if req := a.modal; req != nil {
		var ans Answer
		switch {
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'y' || ev.Rune() == 'Y'):
			ans = AnswerAllow
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'a' || ev.Rune() == 'A'):
			ans = AnswerAlways
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'n' || ev.Rune() == 'N'):
			ans = AnswerDeny
		case ev.Key() == tcell.KeyEscape:
			ans = AnswerCancel
		default:
			return nil
		}
		a.modal = nil
		req.Reply <- ans
		return nil
	}
	switch ev.Key() {
	case tcell.KeyCtrlC, tcell.KeyCtrlD:
		a.quit = true
		return actQuit{}
	case tcell.KeyCtrlU:
		a.input, a.cursor = "", 0
	case tcell.KeyEnter:
		line := strings.TrimSpace(a.input)
		if line == "" || a.busy {
			return nil
		}
		a.input, a.cursor, a.histIdx, a.scroll = "", 0, -1, 0
		if len(a.history) == 0 || a.history[len(a.history)-1] != line {
			a.history = append(a.history, line)
			if a.historyFile != "" {
				if err := saveHistory(a.historyFile, a.history); err != nil {
					slog.Warn("could not save history", "err", err)
				}
			}
		}
		if line == "/exit" || line == "/quit" {
			a.quit = true
			return actQuit{}
		}
		if !strings.HasPrefix(line, "/") {
			a.push(kindUser, line)
		}
		a.busy = true
		a.busySince = a.now()
		a.activity = "Thinking"
		if strings.HasPrefix(line, "/") {
			a.activity = "Working"
		}
		return actSubmit{line}
	case tcell.KeyRune:
		r := string(ev.Rune())
		a.input = a.input[:a.cursor] + r + a.input[a.cursor:]
		a.cursor += len(r)
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if a.cursor > 0 {
			_, n := utf8.DecodeLastRuneInString(a.input[:a.cursor])
			a.input = a.input[:a.cursor-n] + a.input[a.cursor:]
			a.cursor -= n
		}
	case tcell.KeyDelete:
		if a.cursor < len(a.input) {
			_, n := utf8.DecodeRuneInString(a.input[a.cursor:])
			a.input = a.input[:a.cursor] + a.input[a.cursor+n:]
		}
	case tcell.KeyLeft:
		if a.cursor > 0 {
			_, n := utf8.DecodeLastRuneInString(a.input[:a.cursor])
			a.cursor -= n
		}
	case tcell.KeyRight:
		if a.cursor < len(a.input) {
			_, n := utf8.DecodeRuneInString(a.input[a.cursor:])
			a.cursor += n
		}
	case tcell.KeyHome:
		a.cursor = 0
	case tcell.KeyEnd:
		a.cursor = len(a.input)
	case tcell.KeyUp:
		a.recall(-1)
	case tcell.KeyDown:
		a.recall(1)
	case tcell.KeyPgUp:
		a.scroll += 10
	case tcell.KeyPgDn:
		a.scroll = max(a.scroll-10, 0)
	}
	return nil
}

// recall steps up/down through earlier prompts, like the REPL.
func (a *App) recall(dir int) {
	if len(a.history) == 0 {
		return
	}
	next := -1
	switch {
	case a.histIdx < 0 && dir < 0:
		next = len(a.history) - 1
	case a.histIdx < 0:
		next = -1
	case dir < 0:
		next = max(a.histIdx-1, 0)
	case a.histIdx+1 >= len(a.history):
		next = -1
	default:
		next = a.histIdx + 1
	}
	a.histIdx = next
	a.input = ""
	if next >= 0 {
		a.input = a.history[next]
	}
	a.cursor = len(a.input)
}

// HandleMouse: the wheel scrolls the transcript; a left click on a link opens it.
func (a *App) HandleMouse(ev *tcell.EventMouse) Action {
	b := ev.Buttons()
	x, y := ev.Position()
	switch {
	case b&tcell.WheelUp != 0:
		a.scroll += 3
	case b&tcell.WheelDown != 0:
		a.scroll = max(a.scroll-3, 0)
	case b&tcell.Button1 != 0 && a.modal == nil:
		for _, h := range a.linkHits {
			if h.y == y && x >= h.x0 && x < h.x1 {
				return actOpenURL{h.url}
			}
		}
	}
	return nil
}

// ---- drawing ---------------------------------------------------------------------

// drawLine writes styled spans at (x, y), clipped to w cells. Returns cells used.
func drawLine(s tcell.Screen, x, y, w int, line Line) int {
	col := 0
	for _, sp := range line {
		for _, r := range sp.Text {
			rw := runewidth.RuneWidth(r)
			if rw == 0 {
				continue
			}
			if col+rw > w {
				return col
			}
			s.SetContent(x+col, y, r, nil, sp.Style)
			col += rw
		}
	}
	return col
}

func fill(s tcell.Screen, x, y, w, h int, st tcell.Style) {
	for row := y; row < y+h; row++ {
		for col := x; col < x+w; col++ {
			s.SetContent(col, row, ' ', nil, st)
		}
	}
}

// drawBox draws a bordered box with a title on its top edge.
func drawBox(s tcell.Screen, x, y, w, h int, border tcell.Style, title Line) {
	if w < 2 || h < 2 {
		return
	}
	for col := x + 1; col < x+w-1; col++ {
		s.SetContent(col, y, '─', nil, border)
		s.SetContent(col, y+h-1, '─', nil, border)
	}
	for row := y + 1; row < y+h-1; row++ {
		s.SetContent(x, row, '│', nil, border)
		s.SetContent(x+w-1, row, '│', nil, border)
	}
	s.SetContent(x, y, '┌', nil, border)
	s.SetContent(x+w-1, y, '┐', nil, border)
	s.SetContent(x, y+h-1, '└', nil, border)
	s.SetContent(x+w-1, y+h-1, '┘', nil, border)
	drawLine(s, x+1, y, w-2, title)
}

func (a *App) Draw(s tcell.Screen) {
	s.Clear()
	w, h := s.Size()
	transcriptH := max(h-4, 1)
	a.drawTranscript(s, 0, 0, w, transcriptH)

	// Input box (3 rows) and status bar (1 row).
	iy := transcriptH
	title := Line{raw(" prompt ")}
	if a.busy {
		title = a.busyTitle()
	}
	drawBox(s, 0, iy, w, 3, tcell.StyleDefault, title)
	inner := max(w-2, 1)
	// Keep the cursor visible in a long line by scrolling the input horizontally.
	before := width(a.input[:a.cursor])
	off := max(before-(inner-1), 0)
	visible := Line{raw(a.input)}
	if off > 0 {
		visible = hardWrapOffset(a.input, off)
	}
	drawLine(s, 1, iy+1, inner, visible)
	if a.modal == nil {
		s.ShowCursor(1+min(before-off, inner-1), iy+1)
	} else {
		s.HideCursor()
	}

	bar := Line{raw(a.status)}
	if a.speed != "" {
		bar = append(bar, styled("⏱ "+a.speed+" ", fg(tcell.ColorGreen)))
	}
	bar = append(bar, styled("  Enter send · ↑↓ history · PgUp/PgDn scroll · click links · Ctrl-C quit", fg(tcell.ColorGray)))
	rev := tcell.StyleDefault.Reverse(true)
	fill(s, 0, h-1, w, 1, rev)
	for i := range bar {
		bar[i].Style = bar[i].Style.Reverse(true)
	}
	drawLine(s, 0, h-1, w, bar)

	if a.modal != nil {
		a.drawModal(s, w, h)
	}
}

// hardWrapOffset drops the first `off` cells of s.
func hardWrapOffset(s string, off int) Line {
	col := 0
	for i, r := range s {
		if col >= off {
			return Line{raw(s[i:])}
		}
		col += runewidth.RuneWidth(r)
	}
	return nil
}

var spinner = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}
var dots = []string{"   ", ".  ", ".. ", "..."}

// busyTitle is the animated prompt-box title while the agent works, e.g.
// " ✻ Thinking...  12s ". The UI loop redraws every 50 ms, so the frame is
// derived from elapsed time.
func (a *App) busyTitle() Line {
	ms := a.now().Sub(a.busySince).Milliseconds()
	spin := spinner[(ms/120)%int64(len(spinner))]
	d := dots[(ms/350)%int64(len(dots))]
	secs := ms / 1000
	elapsed := fmt.Sprintf("%ds", secs)
	if secs >= 60 {
		elapsed = fmt.Sprintf("%dm %02ds", secs/60, secs%60)
	}
	accent := fg(tcell.NewRGBColor(0xE0, 0x8A, 0x5B)).Bold(true)
	return Line{
		styled(" "+spin+" ", accent),
		styled(a.activity+d+" ", accent),
		styled(elapsed+" ", fg(tcell.ColorGray)),
	}
}

func (a *App) drawTranscript(s tcell.Screen, x, y, w, h int) {
	w = max(w, 1)
	var lines []Line
	type linkAt struct {
		line, start, end int
		url              string
	}
	var links []linkAt
	for _, e := range a.entries {
		if e.code != nil {
			for _, line := range e.code {
				for i, piece := range hardWrap(line, w) {
					if i > 0 {
						piece = append(Line{raw("  ")}, piece...)
					}
					lines = append(lines, piece)
				}
			}
			lines = append(lines, nil)
			continue
		}
		if e.kind == kindAssistant {
			md, mdLinks := renderMarkdown(e.text, w)
			for _, l := range mdLinks {
				links = append(links, linkAt{len(lines) + l.Line, l.Start, l.End, l.URL})
			}
			lines = append(lines, md...)
			lines = append(lines, nil)
			continue
		}
		prefix, st := "", tcell.StyleDefault
		switch e.kind {
		case kindUser:
			prefix, st = "> ", fg(tcell.ColorTeal).Bold(true)
		case kindTool:
			prefix, st = "  ", fg(tcell.ColorOlive)
		case kindToolError:
			prefix, st = "  ", fg(tcell.ColorMaroon)
		case kindInfo:
			// Light gray: readable on dark themes, still distinct from replies.
			prefix, st = "· ", fg(tcell.NewRGBColor(0xC8, 0xC8, 0xCC))
		case kindError:
			prefix, st = "! ", fg(tcell.ColorMaroon)
		}
		indent := strings.Repeat(" ", len(prefix))
		first := true
		for _, rawLine := range strings.Split(e.text, "\n") {
			// An empty line yields one empty piece, so blank lines in the text
			// come through as exactly one blank row.
			for _, piece := range wrapText(rawLine, max(w-len(prefix), 1)) {
				p := indent
				if first {
					p = prefix
				}
				first = false
				lines = append(lines, Line{styled(p+piece, st)})
			}
		}
		lines = append(lines, nil)
	}
	end := len(lines) - min(a.scroll, max(len(lines)-h, 0))
	start := max(end-h, 0)
	a.linkHits = a.linkHits[:0]
	for _, l := range links {
		if l.line >= start && l.line < end {
			a.linkHits = append(a.linkHits, linkHit{x + min(l.start, w), x + min(l.end, w), y + l.line - start, l.url})
		}
	}
	for i, l := range lines[start:end] {
		drawLine(s, x, y+i, w, l)
	}
}

func (a *App) drawModal(s tcell.Screen, sw, sh int) {
	req := a.modal
	w := max(min(sw-4, 80), 20)
	body := firstLines(req.Input, 12)
	h := max(min(len(body)+4, sh-2), 5)
	x, y := max((sw-w)/2, 0), max((sh-h)/2, 0)
	fill(s, x, y, w, h, tcell.StyleDefault)
	drawBox(s, x, y, w, h, fg(tcell.ColorOlive), Line{raw(" \u26a0 " + req.Tool + " wants to run ")})
	row := y + 1
	for _, l := range body {
		if row >= y+h-1 {
			break
		}
		drawLine(s, x+1, row, w-2, Line{raw(l)})
		row++
	}
	row++
	if row < y+h-1 {
		drawLine(s, x+1, row, w-2, Line{styled(fmt.Sprintf("[y]es  [n]o  [a]lways for `%s`  [esc] skip", req.Tool), tcell.StyleDefault.Bold(true))})
	}
}

// ---- event loop ------------------------------------------------------------------

// runTUI is the blocking UI loop: drains agent messages, redraws, and forwards
// submitted lines. Runs until the user quits or the agent side hangs up.
func runTUI(app *App, q *uiQueue, submit chan<- string) error {
	s, err := tcell.NewScreen()
	if err != nil {
		return err
	}
	if err := s.Init(); err != nil {
		return err
	}
	// Mouse reporting so link clicks reach us (wheel scrolling is handled too).
	s.EnableMouse()
	defer s.Fini()

	events := make(chan tcell.Event, 64)
	go func() {
		for {
			ev := s.PollEvent()
			if ev == nil {
				return
			}
			events <- ev
		}
	}()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	defer func() {
		// Never leave the agent blocked on a permission prompt.
		if app.modal != nil {
			app.modal.Reply <- AnswerDeny
		}
		close(submit)
	}()
	for {
		msgs, closed := q.Drain()
		for _, m := range msgs {
			app.Apply(m)
		}
		if closed {
			return nil
		}
		app.Draw(s)
		s.Show()
		select {
		case ev := <-events:
			var act Action
			switch e := ev.(type) {
			case *tcell.EventKey:
				act = app.HandleKey(e)
			case *tcell.EventMouse:
				act = app.HandleMouse(e)
			case *tcell.EventResize:
				s.Sync()
			}
			switch a := act.(type) {
			case actQuit:
				return nil
			case actSubmit:
				submit <- a.Line
			case actOpenURL:
				if err := openURL(a.URL); err != nil {
					app.Apply(uiError{fmt.Sprintf("could not open %s: %v", a.URL, err)})
				} else {
					app.Apply(uiLine{"↗ " + a.URL})
				}
			}
		case <-q.wake:
		case <-tick.C:
		}
		if app.Quit() {
			return nil
		}
	}
}

// openURL opens url with the platform's default handler.
func openURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/C", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it
	return nil
}
