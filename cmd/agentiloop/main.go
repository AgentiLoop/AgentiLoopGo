// Command agentiloop — a cross-platform agentic coding loop for your terminal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AgentiLoop/AgentiLoopGo/core"
	"github.com/AgentiLoop/AgentiLoopGo/mcp"
	"github.com/AgentiLoop/AgentiLoopGo/provider"
	"github.com/AgentiLoop/AgentiLoopGo/tools"
	"github.com/peterh/liner"
	"github.com/spf13/pflag"
)

const version = "0.0.1"

type cliArgs struct {
	provider, model, cwd, resume              string
	yes, continueLast, newSession, tui, noTUI bool
	noMCP                                     bool
	maxTurns                                  *int
	compactAt                                 *uint64
	prompt                                    []string
}

func main() {
	setupLogging()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// setupLogging mirrors tracing's RUST_LOG filter: errors only unless
// RUST_LOG / AGENTILOOP_LOG asks for more.
func setupLogging() {
	lvl := slog.LevelError
	spec := strings.ToLower(os.Getenv("AGENTILOOP_LOG") + "," + os.Getenv("RUST_LOG"))
	switch {
	case strings.Contains(spec, "trace") || strings.Contains(spec, "debug"):
		lvl = slog.LevelDebug
	case strings.Contains(spec, "info"):
		lvl = slog.LevelInfo
	case strings.Contains(spec, "warn"):
		lvl = slog.LevelWarn
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off", "n", "f":
		return false
	}
	return true
}

func parseArgs(argv []string, stdout io.Writer) (*cliArgs, bool, error) {
	fs := pflag.NewFlagSet("agentiloop", pflag.ContinueOnError)
	fs.SortFlags = false
	var c cliArgs
	var maxTurns int
	var compactAt uint64
	fs.StringVarP(&c.provider, "provider", "p", "", "Model backend (`PROVIDER`): anthropic, openai (OpenAI-compatible: OpenAI, Ollama,\nLM Studio, Groq, OpenRouter, … via OPENAI_BASE_URL), or omlx (local\noMLX server, http://localhost:8000/v1). Defaults to the last one used,\nthen auto-detected from which credentials are set. [env: AGENTILOOP_PROVIDER]")
	fs.StringVarP(&c.model, "model", "m", "", "`MODEL` id to use. Defaults to the last model used with this provider\n(~/.agentiloop/settings.json), then the provider's default. [env: AGENTILOOP_MODEL]")
	fs.BoolVar(&c.yes, "yes", false, "Skip all permission prompts (dangerous; intended for CI). Never remembered. [env: AGENTILOOP_YES]")
	fs.IntVar(&maxTurns, "max-turns", 0, "Max provider round-trips (`N`) per prompt [default: last used, then 50]")
	fs.Uint64Var(&compactAt, "compact-at", 0, "Summarize the conversation once a request reaches this many input `TOKENS` (0 = never)\n[default: last used, then 150000] [env: AGENTILOOP_COMPACT_AT]")
	fs.StringVarP(&c.cwd, "cwd", "C", "", "Working directory (`DIR`) the agent operates in (defaults to cwd)")
	fs.StringVarP(&c.resume, "resume", "r", "", "Resume a saved session by `ID` (see /sessions)")
	fs.BoolVarP(&c.continueLast, "continue", "c", false, "Resume the most recent session for this working directory\n(the default for interactive launches; kept for scripts)")
	fs.BoolVar(&c.newSession, "new", false, "Start a new session instead of continuing the last one in this directory")
	fs.BoolVar(&c.tui, "tui", false, "Full-screen terminal UI instead of the line REPL. Remembered. [env: AGENTILOOP_TUI]")
	fs.BoolVar(&c.noTUI, "no-tui", false, "Use the line REPL even if the TUI was used last time")
	fs.BoolVar(&c.noMCP, "no-mcp", false, "Don't start MCP servers from ~/.agentiloop/mcp.json / ./.mcp.json. [env: AGENTILOOP_NO_MCP]")
	help := fs.BoolP("help", "h", false, "Print help")
	ver := fs.BoolP("version", "V", false, "Print version")
	fs.Usage = func() {}
	if err := fs.Parse(argv); err != nil {
		return nil, false, err
	}
	if *help {
		fmt.Fprintf(stdout, "AgentiLoop — a cross-platform agentic coding loop for your terminal.\n\n"+
			"Usage: agentiloop [OPTIONS] [PROMPT]...\n\nArguments:\n  [PROMPT]...  One-shot prompt. If omitted, starts an interactive REPL\n\nOptions:\n%s", fs.FlagUsages())
		return nil, true, nil
	}
	if *ver {
		fmt.Fprintf(stdout, "agentiloop %s\n", version)
		return nil, true, nil
	}
	c.prompt = fs.Args()

	// Environment fallbacks for flags that weren't given.
	env := func(name, key string, set func(string) error) error {
		if v, ok := os.LookupEnv(key); ok && !fs.Changed(name) {
			if err := set(v); err != nil {
				return fmt.Errorf("invalid value '%s' for %s: %v", v, key, err)
			}
		}
		return nil
	}
	errs := []error{
		env("provider", "AGENTILOOP_PROVIDER", func(v string) error { c.provider = v; return nil }),
		env("model", "AGENTILOOP_MODEL", func(v string) error { c.model = v; return nil }),
		env("yes", "AGENTILOOP_YES", func(v string) error { c.yes = truthy(v); return nil }),
		env("tui", "AGENTILOOP_TUI", func(v string) error { c.tui = truthy(v); return nil }),
		env("no-mcp", "AGENTILOOP_NO_MCP", func(v string) error { c.noMCP = truthy(v); return nil }),
		env("compact-at", "AGENTILOOP_COMPACT_AT", func(v string) error {
			n, err := strconv.ParseUint(v, 10, 64)
			compactAt = n
			fs.Lookup("compact-at").Changed = err == nil
			return err
		}),
	}
	if err := errors.Join(errs...); err != nil {
		return nil, false, err
	}
	if fs.Changed("max-turns") {
		c.maxTurns = &maxTurns
	}
	if fs.Changed("compact-at") {
		c.compactAt = &compactAt
	}
	conflict := func(a, b string) error { return fmt.Errorf("the argument '%s' cannot be used with '%s'", a, b) }
	switch {
	case c.resume != "" && c.continueLast:
		return nil, false, conflict("--resume <RESUME>", "--continue")
	case c.newSession && c.resume != "":
		return nil, false, conflict("--new", "--resume <RESUME>")
	case c.newSession && c.continueLast:
		return nil, false, conflict("--new", "--continue")
	case c.tui && len(c.prompt) > 0:
		return nil, false, conflict("--tui", "[PROMPT]...")
	case c.tui && c.noTUI:
		return nil, false, conflict("--no-tui", "--tui")
	}
	return &c, false, nil
}

func run() error {
	cli, done, err := parseArgs(os.Args[1:], os.Stdout)
	if err != nil || done {
		return err
	}
	ctx := context.Background()

	cwd := cli.cwd
	if cwd != "" {
		if cwd, err = filepath.Abs(cwd); err == nil {
			cwd, err = filepath.EvalSymlinks(cwd)
		}
	} else {
		cwd, err = os.Getwd()
	}
	if err != nil {
		return err
	}

	// Anything not given on the command line comes from the last interactive launch,
	// so a bare `agentiloop` reopens with the same provider, model, UI and session.
	saved := loadSettings()
	interactive := len(cli.prompt) == 0
	last := saved.Last
	useTUI := interactive && !cli.noTUI && (cli.tui || last.TUI)
	maxTurns := 50
	if cli.maxTurns != nil {
		maxTurns = *cli.maxTurns
	} else if last.MaxTurns != nil {
		maxTurns = *last.MaxTurns
	}
	compactAt := uint64(150_000)
	if cli.compactAt != nil {
		compactAt = *cli.compactAt
	} else if last.CompactAt != nil {
		compactAt = *last.CompactAt
	}

	provName := cli.provider
	if provName == "" && last.Provider != nil {
		provName = *last.Provider
	}
	prov, err := provider.FromEnv(provName)
	if err != nil {
		return err
	}
	registry := tools.DefaultRegistry()
	mcp.Version = version
	mgr := &mcp.Manager{}
	if !cli.noMCP {
		mgr = mcp.Start(ctx, mcp.DefaultPaths(mcpConfigPath(), cwd), cwd)
	}
	mgr.RegisterTools(registry)
	if !mgr.IsEmpty() {
		fmt.Fprintf(os.Stderr, "mcp: %d server(s) connected, %d tool(s)\n", len(mgr.Servers), mgr.ToolCount())
		for _, e := range mgr.Errors {
			fmt.Fprintf(os.Stderr, "mcp: %s failed: %s\n", e.Name, e.Err)
		}
	}
	defer mgr.Shutdown()

	// The TUI answers permission prompts through its own queue-backed policy.
	q := newUIQueue()
	var policy core.PermissionPolicy
	if useTUI && !cli.yes {
		policy = NewChannelPolicy(q)
	} else {
		policy = newPolicy(cli.yes)
	}
	sdir := sessionsDir()

	// Resume, if asked — or automatically for a plain interactive launch, as long as the
	// last session here used the same provider. The session's model wins unless --model was given.
	autoContinue := interactive && cli.resume == "" && !cli.continueLast && !cli.newSession
	var resumed *core.Session
	switch {
	case sdir == "":
	case cli.resume != "":
		if resumed, err = core.LoadSession(sdir, cli.resume); err != nil {
			return err
		}
	case cli.continueLast || autoContinue:
		if resumed, err = core.LatestSessionFor(sdir, cwd); err != nil {
			return err
		}
	}
	if autoContinue && resumed != nil && resumed.Provider != prov.Name() {
		resumed = nil
	}
	if cli.continueLast && resumed == nil {
		fmt.Fprintf(os.Stderr, "no previous session for %s; starting fresh\n", cwd)
	}

	model := cli.model
	if model == "" && resumed != nil {
		model = resumed.Model
	}
	if model == "" {
		model = saved.ModelFor(prov.Name())
	}
	if model == "" {
		if prov.DefaultModel() == "" {
			// Local servers (oMLX) have no fixed catalog: take whatever is served first.
			models, err := prov.ListModels(ctx)
			if err != nil {
				return fmt.Errorf("could not get the model list from %s: %w", prov.Name(), err)
			}
			if len(models) == 0 {
				return fmt.Errorf("%s serves no models; load one or pass --model", prov.Name())
			}
			model = models[0].ID
		} else {
			model = prov.DefaultModel()
		}
	}
	config := core.DefaultAgentConfig()
	config.Model, config.MaxTurns, config.CompactAtTokens = model, maxTurns, compactAt

	// Remember this launch (model per provider always; UI options only for interactive runs).
	saved.SetModel(prov.Name(), config.Model)
	if interactive {
		name := prov.Name()
		saved.Last = LastLaunch{Provider: &name, TUI: useTUI, MaxTurns: &maxTurns, CompactAt: &compactAt}
	}
	if err := saveSettings(&saved); err != nil {
		slog.Warn("could not save settings", "err", err)
	}

	agent := core.NewAgent(prov, registry, policy, config, core.ToolContext{Cwd: cwd})
	var session *core.Session
	if resumed != nil {
		fmt.Fprintf(os.Stderr, "resumed session %s (%d messages): %s\n", resumed.ID, len(resumed.History), resumed.Title())
		agent.History = append([]core.Message(nil), resumed.History...)
		session = resumed
	} else {
		session = core.NewSession(cwd, prov.Name(), agent.Model())
	}

	st := &cmdState{agent: agent, provider: prov, saved: &saved, session: session, sessionsDir: sdir, mcp: mgr}

	if !interactive {
		err := agent.Run(ctx, strings.Join(cli.prompt, " "), renderTracked(newDiffTracker(cwd)))
		st.persist()
		return err
	}
	if useTUI {
		return runTUIMode(ctx, st, q, cwd)
	}
	return runREPL(ctx, st, cwd)
}

// cmdState is what slash commands operate on.
type cmdState struct {
	agent       *core.Agent
	provider    core.Provider
	saved       *Settings
	session     *core.Session
	sessionsDir string
	mcp         *mcp.Manager
	// ask reads a line from the user (REPL only); nil in the TUI.
	ask func(prompt string) (string, error)
}

func runTUIMode(ctx context.Context, st *cmdState, q *uiQueue, cwd string) error {
	status := func() string {
		return fmt.Sprintf(" AgentiLoop  %s  %s  %s  session %s ", cwd, st.provider.Name(), st.agent.Model(), st.session.ID)
	}
	app := NewApp(status()).WithHistoryFile(historyPath())
	// A continued session shows its earlier conversation, not an empty screen.
	replayTUI(q, st.agent.History)
	submit := make(chan string, 1)
	uiDone := make(chan error, 1)
	go func() { uiDone <- runTUI(app, q, submit) }()
	tracker := newDiffTracker(cwd)
	// Agent side: one prompt or slash command at a time, until the UI hangs up.
	for line := range submit {
		if strings.HasPrefix(line, "/") {
			// Collect the command's output into one transcript entry so
			// multi-line output (the /model list) isn't double-spaced.
			var out []string
			before := st.session.ID
			err := st.slashCommand(ctx, line, func(s string) { out = append(out, s) })
			// /resume and /clear switch sessions: show the new one's conversation.
			if st.session.ID != before {
				tracker = newDiffTracker(cwd)
				q.Send(uiClear{})
				replayTUI(q, st.agent.History)
			}
			if len(out) > 0 {
				q.Send(uiLine{strings.Join(out, "\n")})
			}
			if err != nil {
				q.Send(uiError{err.Error()})
			}
			// /model, /clear and /resume change the model or session id.
			q.Send(uiStatus{status()})
		} else {
			onEvent := func(ev core.Event) {
				// Snapshot/diff before the event crosses to the UI goroutine:
				// the tool runs right after EvToolCall returns.
				change := tracker.observe(ev)
				q.Send(uiEvent{ev})
				if change != nil {
					q.Send(uiDiff{*change})
				}
			}
			if err := st.agent.Run(ctx, line, onEvent); err != nil {
				q.Send(uiError{err.Error()})
			}
			st.persist()
		}
		q.Send(uiIdle{})
	}
	q.Close()
	return <-uiDone
}

func runREPL(ctx context.Context, st *cmdState, cwd string) error {
	fmt.Fprintf(os.Stderr, "AgentiLoop — cwd: %s  provider: %s  model: %s  session: %s  (/help for commands)\n",
		cwd, st.provider.Name(), st.agent.Model(), st.session.ID)
	replayREPL(st.agent.History)
	renderLine := renderTracked(newDiffTracker(cwd))

	// liner gives us line editing plus up/down arrow recall of earlier prompts.
	ln := liner.NewLiner()
	defer ln.Close()
	ln.SetCtrlCAborts(true)
	hist := historyPath()
	entries := loadHistory(hist)
	for _, h := range entries {
		ln.AppendHistory(h)
	}
	st.ask = func(prompt string) (string, error) { return ln.Prompt(prompt) }
	// Permission prompts go through the same line editor that owns stdin.
	if p, ok := st.agent.Policy().(*interactivePolicy); ok {
		p.ask = st.ask
	}
	for {
		fmt.Fprintln(os.Stderr)
		line, err := ln.Prompt("> ")
		if errors.Is(err, liner.ErrPromptAborted) || errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(entries) == 0 || entries[len(entries)-1] != line {
			entries = append(entries, line)
			ln.AppendHistory(line)
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		if strings.HasPrefix(line, "/") {
			before := st.session.ID
			if err := st.slashCommand(ctx, line, func(s string) { fmt.Fprintln(os.Stderr, s) }); err != nil {
				return err
			}
			if st.session.ID != before {
				replayREPL(st.agent.History)
			}
			continue
		}
		if err := st.agent.Run(ctx, line, renderLine); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		st.persist()
	}
	if hist != "" {
		if err := saveHistory(hist, entries); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save history: %v\n", err)
		}
	}
	return nil
}

// replay turns saved history back into what was on screen: user prompts (via
// user) and assistant text / tool calls / tool results (via event).
func replay(history []core.Message, user func(string), event func(core.Event)) {
	names := map[string]string{}
	for _, m := range history {
		for _, b := range m.Content {
			switch b.Type {
			case core.BlockText:
				switch {
				case strings.TrimSpace(b.Text) == "":
				case m.Role == core.RoleUser:
					user(b.Text)
				default:
					event(core.EvText{Text: b.Text})
				}
			case core.BlockToolUse:
				names[b.ID] = b.Name
				event(core.EvToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
			case core.BlockToolResult:
				event(core.EvToolResult{ID: b.ToolUseID, Name: names[b.ToolUseID], Output: b.Content, IsError: b.IsError})
			}
		}
	}
}

func replayTUI(q *uiQueue, history []core.Message) {
	replay(history, func(s string) { q.Send(uiUser{s}) }, func(ev core.Event) { q.Send(uiEvent{ev}) })
}

func replayREPL(history []core.Message) {
	replay(history,
		func(s string) { fmt.Fprintf(os.Stderr, "\n> %s\n", s) },
		func(ev core.Event) {
			// render expects deltas before EvText; print whole text here.
			if t, ok := ev.(core.EvText); ok {
				fmt.Println(t.Text)
				return
			}
			render(ev)
		})
	if len(history) > 0 {
		fmt.Fprintln(os.Stderr, "\n── end of previous conversation ──")
	}
}

// persist snapshots the agent's history into the session file. Empty histories are not written.
func (st *cmdState) persist() {
	if st.sessionsDir == "" {
		return
	}
	st.session.History = append([]core.Message(nil), st.agent.History...)
	st.session.Model = st.agent.Model()
	if len(st.session.History) == 0 {
		return
	}
	if _, err := st.session.Save(st.sessionsDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save session: %v\n", err)
	}
}

// fallbackModels is used when /v1/models can't be reached (newest first).
var fallbackModels = []core.ModelInfo{
	{ID: "claude-fable-5-1", DisplayName: "Claude Fable 5.1"},
	{ID: "claude-opus-5", DisplayName: "Claude Opus 5"},
	{ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5"},
	{ID: "claude-fable-5", DisplayName: "Claude Fable 5"},
	{ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8"},
	{ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7"},
	{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
	{ID: "claude-opus-4-6", DisplayName: "Claude Opus 4.6"},
	{ID: "claude-opus-4-5-20251101", DisplayName: "Claude Opus 4.5"},
	{ID: "claude-haiku-4-5-20251001", DisplayName: "Claude Haiku 4.5"},
	{ID: "claude-sonnet-4-5-20250929", DisplayName: "Claude Sonnet 4.5"},
}

// fetchModels returns the live model list. Anthropic falls back to the static
// catalog when /v1/models can't be reached; other providers report the error.
func fetchModels(ctx context.Context, p core.Provider) []core.ModelInfo {
	list, err := p.ListModels(ctx)
	switch {
	case err == nil && len(list) > 0:
		return list
	case p.Name() == "anthropic":
		if err != nil {
			slog.Warn("model list fetch failed, using fallback", "err", err)
		}
		return fallbackModels
	case err != nil:
		fmt.Fprintf(os.Stderr, "could not list models: %v\n", err)
	default:
		fmt.Fprintln(os.Stderr, "provider returned no models")
	}
	return nil
}

const helpText = "/model [n|id]   show picker, or pick #n / set id directly\n" +
	"/mcp            list MCP servers and their tools\n" +
	"/compact        summarize the conversation to free context\n" +
	"/sessions       list saved sessions (newest first)\n" +
	"/resume <id|n>  load a saved session into this REPL\n" +
	"/clear          clear context and start a new session\n" +
	"/exit           quit"

// slashCommand runs a /command. Output lines go through say so the REPL
// (stderr) and the TUI (transcript) share one implementation; st.ask lets the
// /model picker read a choice in the REPL.
func (st *cmdState) slashCommand(ctx context.Context, line string, say func(string)) error {
	cmd, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	agent := st.agent
	switch cmd {
	case "/clear":
		agent.Clear()
		st.session = core.NewSession(st.session.Cwd, st.provider.Name(), agent.Model())
		say("context and tool history cleared; new session " + st.session.ID)
	case "/model":
		models := fetchModels(ctx, st.provider)
		// `/model 2` picks entry #2 directly; `/model <id>` sets an id.
		pick := arg
		if arg == "" {
			for i, m := range models {
				mark := " "
				if m.ID == agent.Model() {
					mark = "*"
				}
				date := ""
				if len(m.CreatedAt) >= 10 {
					date = m.CreatedAt[:10]
				}
				say(fmt.Sprintf("%s %2d. %-22s %-28s %s", mark, i+1, m.DisplayName, m.ID, date))
			}
			if st.ask == nil {
				say(fmt.Sprintf("current: %s  — pick with /model <n|id>", agent.Model()))
				return nil
			}
			answer, err := st.ask(fmt.Sprintf("select [1-%d] or type a model id (enter to keep %s): ", len(models), agent.Model()))
			if err != nil && !errors.Is(err, liner.ErrPromptAborted) && !errors.Is(err, io.EOF) {
				return err
			}
			pick = strings.TrimSpace(answer)
		}
		if pick == "" {
			return nil
		}
		if n, err := strconv.ParseUint(pick, 10, 64); err == nil {
			if n < 1 || int(n) > len(models) {
				say(fmt.Sprintf("out of range [1-%d]", len(models)))
				return nil
			}
			agent.SetModel(models[n-1].ID)
		} else {
			agent.SetModel(pick)
		}
		st.saved.SetModel(st.provider.Name(), agent.Model())
		if err := saveSettings(st.saved); err != nil {
			say(fmt.Sprintf("warning: could not save settings: %v", err))
		}
		say("model: " + agent.Model())
	case "/compact":
		ev, err := agent.Compact(ctx)
		switch {
		case err != nil:
			say(fmt.Sprintf("compaction failed: %v", err))
		case ev == nil:
			say("nothing to compact")
		default:
			say(compactedLine(ev.BeforeTokens, ev.MessagesDropped))
			st.persist()
		}
	case "/sessions":
		if st.sessionsDir == "" {
			say("no home directory; sessions are not saved")
			return nil
		}
		list, err := core.ListSessions(st.sessionsDir)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			say("no saved sessions")
		}
		for i, s := range list {
			if i == 20 {
				break
			}
			mark := " "
			if s.ID == st.session.ID {
				mark = "*"
			}
			say(fmt.Sprintf("%s %2d. %-22s %3d msgs  %-24s %s", mark, i+1, s.ID, len(s.History), s.Model, s.Title()))
		}
	case "/resume":
		if st.sessionsDir == "" {
			say("no home directory; sessions are not saved")
			return nil
		}
		if arg == "" {
			say("usage: /resume <id|n>  (see /sessions)")
			return nil
		}
		id := arg
		// `/resume 2` picks entry #2 from the /sessions listing.
		if n, err := strconv.ParseUint(arg, 10, 64); err == nil {
			list, err := core.ListSessions(st.sessionsDir)
			if err != nil {
				return err
			}
			if n < 1 || int(n) > len(list) {
				say(fmt.Sprintf("no session #%d", n))
				return nil
			}
			id = list[n-1].ID
		}
		s, err := core.LoadSession(st.sessionsDir, id)
		if err != nil {
			say(err.Error())
			return nil
		}
		agent.Clear()
		agent.History = append([]core.Message(nil), s.History...)
		agent.SetModel(s.Model)
		say(fmt.Sprintf("resumed session %s (%d messages, model %s): %s", s.ID, len(s.History), s.Model, s.Title()))
		st.session = s
	case "/mcp":
		if st.mcp.IsEmpty() {
			say("no MCP servers configured (add `mcpServers` to ~/.agentiloop/mcp.json or ./.mcp.json)")
		}
		for _, l := range st.mcp.StatusLines() {
			say(l)
		}
	case "/help":
		say(helpText)
	default:
		say(fmt.Sprintf("unknown command %s (try /help)", cmd))
	}
	return nil
}

func compactedLine(beforeTokens uint64, messagesDropped int) string {
	return fmt.Sprintf("\U0001f4e6 context compacted (%d tokens, %d messages → summary)", beforeTokens, messagesDropped)
}

// renderTracked is render plus a -/+ diff after each successful write_file / edit_file.
func renderTracked(t *diffTracker) func(core.Event) {
	color := colorStderr()
	return func(ev core.Event) {
		change := t.observe(ev)
		render(ev)
		if change != nil {
			fmt.Fprint(os.Stderr, ansiDiff(change.Edit, inlineMax, color))
		}
	}
}

// render prints agent events for the REPL and one-shot mode.
func render(ev core.Event) {
	switch e := ev.(type) {
	case core.EvTextDelta:
		fmt.Print(e.Text)
		os.Stdout.Sync()
	case core.EvText:
		// Deltas already printed the text; terminate the line.
		fmt.Println()
	case core.EvToolCall:
		fmt.Fprintf(os.Stderr, "\U0001f527 %s %s\n", e.Name, compactJSON(e.Input))
	case core.EvToolResult:
		mark := "✓"
		if e.IsError {
			mark = "✖"
		}
		fmt.Fprintf(os.Stderr, "   %s %s\n", mark, strings.Join(firstLines(e.Output, 8), "\n   "))
	case core.EvTurnComplete:
		slog.Debug("turn", "input_tokens", e.InputTokens, "output_tokens", e.OutputTokens, "elapsed_ms", e.ElapsedMs)
		if _, ok := os.LookupEnv("AGENTILOOP_SPEED"); ok {
			// Leading newline: streamed text hasn't been terminated yet.
			fmt.Fprintf(os.Stderr, "\n   ⏱ %s\n", speedLine(e.OutputTokens, e.ElapsedMs, e.FirstTokenMs))
		}
	case core.EvCompacted:
		fmt.Fprintln(os.Stderr, compactedLine(e.BeforeTokens, e.MessagesDropped))
	}
}

// tokensPerSec is output tokens per second for one model call. With a
// first-token time the rate covers only the generation window (after TTFT);
// tool-call-only turns stream no text, so the whole call is used.
func tokensPerSec(outputTokens, elapsedMs uint64, firstTokenMs *uint64) (float64, bool) {
	gen := elapsedMs
	if firstTokenMs != nil && elapsedMs > *firstTokenMs {
		gen = elapsedMs - *firstTokenMs
	}
	if outputTokens == 0 || gen == 0 {
		return 0, false
	}
	return float64(outputTokens) * 1000 / float64(gen), true
}

// speedLine renders e.g. "42.3 tok/s · ttft 0.61s · 312 tok in 7.9s".
func speedLine(outputTokens, elapsedMs uint64, firstTokenMs *uint64) string {
	var parts []string
	if r, ok := tokensPerSec(outputTokens, elapsedMs, firstTokenMs); ok {
		parts = append(parts, fmt.Sprintf("%.1f tok/s", r))
	}
	if firstTokenMs != nil {
		parts = append(parts, fmt.Sprintf("ttft %.2fs", float64(*firstTokenMs)/1000))
	}
	parts = append(parts, fmt.Sprintf("%d tok in %.1fs", outputTokens, float64(elapsedMs)/1000))
	return strings.Join(parts, " · ")
}

// compactJSON renders tool input on one line, trimmed to 120 bytes.
func compactJSON(raw json.RawMessage) string {
	s := "null"
	if len(raw) > 0 {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			s = core.MarshalString(v, "")
		} else {
			s = string(raw)
		}
	}
	if len(s) <= 120 {
		return s
	}
	cut := 120
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
