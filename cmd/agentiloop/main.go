// Command agentiloop — a cross-platform agentic coding loop for your terminal.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
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

const version = "0.0.4"

type cliArgs struct {
	provider, model, cwd, resume              string
	appendPrompt                              string
	allowTools, denyTools                     []string
	yes, continueLast, newSession, tui, noTUI bool
	noMCP, setup, reset, jsonOut              bool
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
	fs.StringVarP(&c.provider, "provider", "p", "", "Model backend (`PROVIDER`): anthropic, openai (OpenAI-compatible: OpenAI, Ollama,\nLM Studio, Groq, OpenRouter, … via OPENAI_BASE_URL), omlx (local\noMLX server, http://localhost:8000/v1), or codex (ChatGPT plan via\n`codex login`). Defaults to the last one used, then auto-detected from\nwhich credentials are set. [env: AGENTILOOP_PROVIDER]")
	fs.StringVarP(&c.model, "model", "m", "", "`MODEL` id to use. Defaults to the last model used with this provider\n(~/.agentiloop/settings.json), then the provider's default. [env: AGENTILOOP_MODEL]")
	fs.BoolVar(&c.yes, "yes", false, "Skip all permission prompts (dangerous; intended for CI). Never remembered. [env: AGENTILOOP_YES]")
	fs.IntVar(&maxTurns, "max-turns", 0, "Max provider round-trips (`N`) per prompt [default: last used, then 50]")
	fs.Uint64Var(&compactAt, "compact-at", 0, "Summarize the conversation once a request reaches this many input `TOKENS` (0 = never)\n[default: last used, then 80% of the model's context window] [env: AGENTILOOP_COMPACT_AT]")
	fs.StringVarP(&c.cwd, "cwd", "C", "", "Working directory (`DIR`) the agent operates in (defaults to cwd)")
	fs.StringVarP(&c.resume, "resume", "r", "", "Resume a saved session by `ID` (see /sessions)")
	fs.BoolVarP(&c.continueLast, "continue", "c", false, "Resume the most recent session for this working directory\n(the default for interactive launches; kept for scripts)")
	fs.BoolVar(&c.newSession, "new", false, "Start a new session instead of continuing the last one in this directory")
	fs.BoolVar(&c.tui, "tui", false, "Full-screen terminal UI instead of the line REPL. Remembered. [env: AGENTILOOP_TUI]")
	fs.BoolVar(&c.noTUI, "no-tui", false, "Use the line REPL even if the TUI was used last time")
	fs.BoolVar(&c.noMCP, "no-mcp", false, "Don't start MCP servers from ~/.agentiloop/mcp.json / ./.mcp.json. [env: AGENTILOOP_NO_MCP]")
	fs.BoolVar(&c.setup, "setup", false, "Run the first-time setup wizard (provider, key, model). Runs by itself on a\nmachine with no credentials and no ~/.agentiloop.")
	fs.BoolVar(&c.reset, "reset", false, "Back to brand new: delete ~/.agentiloop (settings, env, history, sessions,\nmcp.json), the agentiloop block in your shell profile and Keychain items the\nwizard created. Asks first unless --yes.")
	fs.StringSliceVar(&c.allowTools, "allow-tool", nil, "Run this tool without asking (repeatable or comma-separated; `NAME`* matches a prefix, e.g. mcp_*)")
	fs.StringSliceVar(&c.denyTools, "deny-tool", nil, "Never run this tool `NAME`; the model is told it was denied (same name rules). Beats --allow-tool and --yes")
	fs.StringVar(&c.appendPrompt, "append-system-prompt", "", "Extra `TEXT` added to the end of the system prompt for this run (never saved). [env: AGENTILOOP_APPEND_SYSTEM_PROMPT]")
	fs.BoolVar(&c.jsonOut, "json", false, "One-shot only: print the answer as one JSON object on stdout (result, is_error, session_id,\nprovider, model, usage) instead of streaming text. Tool activity still goes to stderr.")
	help := fs.BoolP("help", "h", false, "Print help")
	ver := fs.BoolP("version", "V", false, "Print version")
	fs.Usage = func() {}
	if err := fs.Parse(argv); err != nil {
		return nil, false, err
	}
	if *help {
		fmt.Fprintf(stdout, "AgentiLoop — a cross-platform agentic coding loop for your terminal.\n\n"+
			"Usage: agentiloop [OPTIONS] [PROMPT]...\n\nArguments:\n  [PROMPT]...  One-shot prompt. If omitted, starts an interactive REPL. A lone `-` in it is replaced by\n               what is piped on stdin: git diff | agentiloop \"review this\" -\n\nOptions:\n%s", fs.FlagUsages())
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
		env("append-system-prompt", "AGENTILOOP_APPEND_SYSTEM_PROMPT", func(v string) error { c.appendPrompt = v; return nil }),
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
	case c.setup && c.reset:
		return nil, false, conflict("--setup", "--reset")
	case c.setup && len(c.prompt) > 0:
		return nil, false, conflict("--setup", "[PROMPT]...")
	case c.reset && len(c.prompt) > 0:
		return nil, false, conflict("--reset", "[PROMPT]...")
	case c.jsonOut && len(c.prompt) == 0:
		return nil, false, errors.New("the following required arguments were not provided:\n  <PROMPT>...")
	}
	return &c, false, nil
}

func run() error {
	cli, done, err := parseArgs(os.Args[1:], os.Stdout)
	if err != nil || done {
		return err
	}
	if cli.reset {
		return runReset(cli.yes, os.Stdin, os.Stdout)
	}
	loadEnvFile()
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
	useTUI := interactive && !cli.noTUI && (cli.tui || saved.Last.TUI)

	// The TUI owns the screen from the very start, so the setup wizard and any
	// startup messages go through it; without it they go to the terminal.
	q := newUIQueue()
	submit := make(chan string, 1)
	cancel := make(chan struct{}, 1)
	var uiDone chan error
	if useTUI {
		app := NewApp(fmt.Sprintf(" AgentiLoop  %s ", cwd)).WithHistoryFile(historyPath())
		uiDone = make(chan error, 1)
		go func() { uiDone <- runTUI(app, q, submit, cancel) }()
	}
	note := func(s string) {
		if useTUI {
			q.Send(uiLine{s})
		} else {
			fmt.Fprintln(os.Stderr, s)
		}
	}
	var setup prompter = newTermPrompter(os.Stdin, os.Stdout)
	if useTUI {
		setup = &setupPrompter{q: q, submit: submit}
	}

	// Everything that can fail before the agent exists runs in boot so that, with
	// the TUI up, the screen is restored before the error reaches stderr.
	st, err := boot(ctx, cli, &saved, cwd, interactive, useTUI, q, setup, note)
	if err != nil {
		if useTUI {
			q.Close()
			<-uiDone
		}
		return err
	}
	defer st.mcp.Shutdown()

	if !interactive {
		tools.BeginUndoTurn()
		prompt, err := expandStdin(cli.prompt, func() (string, error) {
			b, err := io.ReadAll(os.Stdin)
			return string(b), err
		})
		var lastText string
		if err == nil {
			track := renderTracked(newDiffTracker(cwd))
			onEvent := track
			if cli.jsonOut {
				onEvent = func(ev core.Event) {
					switch e := ev.(type) {
					case core.EvTurnComplete:
						lastText = ""
					case core.EvText:
						lastText = e.Text
						return
					case core.EvTextDelta:
						return
					}
					track(ev)
				}
			}
			err = st.agent.Run(ctx, prompt, onEvent)
		}
		st.persist()
		if cli.jsonOut {
			u := st.agent.Usage()
			fmt.Println(jsonResult(lastText, err, st.session.ID, st.provider.Name(), st.agent.Model(), u))
		}
		st.persist()
		return err
	}
	if useTUI {
		return runTUIMode(ctx, st, q, submit, cancel, uiDone, cwd)
	}
	return runREPL(ctx, st, cwd)
}

// boot runs the wizard when asked, picks provider/model/session and builds the agent.
func boot(ctx context.Context, cli *cliArgs, saved *Settings, cwd string, interactive, useTUI bool, q *uiQueue, setup prompter, note func(string)) (*cmdState, error) {
	if cli.setup || shouldRunWizard(interactive, cli.provider != "") {
		if err := runWizard(ctx, saved, setup); err != nil {
			return nil, err
		}
	}
	last := saved.Last
	maxTurns := 50
	if cli.maxTurns != nil {
		maxTurns = *cli.maxTurns
	} else if last.MaxTurns != nil {
		maxTurns = *last.MaxTurns
	}
	compactAt := cli.compactAt
	if compactAt == nil {
		compactAt = last.CompactAt
	}

	provName := cli.provider
	if provName == "" && last.Provider != nil {
		provName = *last.Provider
	}
	prov, err := provider.FromEnv(provName)
	if err != nil {
		return nil, err
	}
	registry := tools.DefaultRegistry()
	// Codex models are trained on apply_patch; other providers keep the plain edit tools.
	if prov.Name() == "codex" {
		registry.Register(tools.ApplyPatch{})
	}
	mcp.Version = version
	mgr := &mcp.Manager{}
	if !cli.noMCP {
		mgr = mcp.Start(ctx, mcp.DefaultPaths(mcpConfigPath(), cwd), cwd)
	}
	mgr.RegisterTools(registry)
	if !mgr.IsEmpty() {
		note(fmt.Sprintf("mcp: %d server(s) connected, %d tool(s)", len(mgr.Servers), mgr.ToolCount()))
		for _, e := range mgr.Errors {
			note(fmt.Sprintf("mcp: %s failed: %s", e.Name, e.Err))
		}
	}

	// The TUI answers permission prompts through its own queue-backed policy.
	var policy core.PermissionPolicy
	if useTUI && !cli.yes {
		policy = NewChannelPolicy(q)
	} else {
		policy = newPolicy(cli.yes)
	}
	if allow, deny := core.NewToolPatterns(cli.allowTools), core.NewToolPatterns(cli.denyTools); len(allow) > 0 || len(deny) > 0 {
		policy = &core.Rules{Allow: allow, Deny: deny, Inner: policy}
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
			return nil, err
		}
	case cli.continueLast || autoContinue:
		if resumed, err = core.LatestSessionFor(sdir, cwd); err != nil {
			return nil, err
		}
	}
	if autoContinue && resumed != nil && resumed.Provider != prov.Name() {
		resumed = nil
	}
	if cli.continueLast && resumed == nil {
		note(fmt.Sprintf("no previous session for %s; starting fresh", cwd))
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
				return nil, fmt.Errorf("could not get the model list from %s: %w", prov.Name(), err)
			}
			if len(models) == 0 {
				return nil, fmt.Errorf("%s serves no models; load one or pass --model", prov.Name())
			}
			model = models[0].ID
		} else {
			model = prov.DefaultModel()
		}
	}
	config := core.DefaultAgentConfig()
	config.Model, config.MaxTurns, config.CompactAtTokens = model, maxTurns, compactAt
	instructions := core.LoadInstructions(cwd, agentiloopHome())
	for _, i := range instructions {
		note("instructions: " + i.Path)
	}
	config.SystemPrompt = core.AppendInstructions(config.SystemPrompt, instructions)
	config.SystemPrompt = appendExtra(config.SystemPrompt, cli.appendPrompt)

	// Remember this launch (model per provider always; UI options only for interactive runs).
	saved.SetModel(prov.Name(), config.Model)
	if interactive {
		name := prov.Name()
		saved.Last = LastLaunch{Provider: &name, TUI: useTUI, MaxTurns: &maxTurns, CompactAt: compactAt}
	}
	if err := saveSettings(saved); err != nil {
		slog.Warn("could not save settings", "err", err)
	}

	agent := core.NewAgent(prov, registry, policy, config, core.ToolContext{Cwd: cwd})
	var session *core.Session
	if resumed != nil {
		note(fmt.Sprintf("resumed session %s (%d messages): %s", resumed.ID, len(resumed.History), resumed.Title()))
		agent.History = append([]core.Message(nil), resumed.History...)
		session = resumed
	} else {
		session = core.NewSession(cwd, prov.Name(), agent.Model())
	}

	return &cmdState{agent: agent, provider: prov, saved: saved, session: session, sessionsDir: sdir, mcp: mgr, setup: setup}, nil
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
	// setup is how /setup talks to the user: the terminal, or the TUI's transcript and input line.
	setup prompter
}

func runTUIMode(ctx context.Context, st *cmdState, q *uiQueue, submit chan string, cancel <-chan struct{}, uiDone <-chan error, cwd string) error {
	status := func() string {
		return fmt.Sprintf(" AgentiLoop  %s  %s  %s  session %s ", cwd, st.provider.Name(), st.agent.Model(), st.session.ID)
	}
	q.Send(uiStatus{status()})
	// A continued session shows its earlier conversation, not an empty screen.
	replayTUI(q, st.agent.History)
	// The last wizard answer left the UI in "Setting up"; the prompt is open now.
	q.Send(uiIdle{})
	tracker := newDiffTracker(cwd)
	// Agent side: one prompt or slash command at a time, until the UI hangs up.
	for line := range submit {
		// An MCP prompt (/mcp__<server>__<name> args) turns into the text the server returns.
		if text, ok, err := st.mcp.PromptCommand(ctx, line); ok {
			if err != nil {
				q.Send(uiError{err.Error()})
				q.Send(uiIdle{})
				continue
			}
			line = text
		}
		// A custom command (.agentiloop/commands/<name>.md) turns into its prompt.
		if p, ok := core.ResolveCommand(line, st.session.Cwd, agentiloopHome()); ok {
			line = p
		}
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
			if cancelled, err := runCancellable(ctx, st.agent, line, onEvent, cancel); cancelled {
				q.Send(uiLine{"Cancelled. Session kept; send your next prompt."})
			} else if err != nil {
				q.Send(uiError{err.Error()})
			}
			st.persist()
		}
		q.Send(uiIdle{})
	}
	q.Close()
	return <-uiDone
}

// runCancellable runs one prompt until it finishes or a signal arrives on cancel
// (Esc in the TUI). Cancelling stops the provider call and any running tool; the
// agent keeps its history, so the session continues with the next prompt.
func runCancellable(ctx context.Context, agent *core.Agent, line string, onEvent func(core.Event), cancel <-chan struct{}) (bool, error) {
	// Drop an Esc that arrived after the previous run had already finished.
	select {
	case <-cancel:
	default:
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-cancel:
			stop()
		case <-done:
		}
	}()
	tools.BeginUndoTurn()
	err := agent.Run(runCtx, line, onEvent)
	return err != nil && runCtx.Err() != nil && ctx.Err() == nil, err
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
	if p, ok := innerPolicy(st.agent.Policy()).(*interactivePolicy); ok {
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
		if text, ok, err := st.mcp.PromptCommand(ctx, line); ok {
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			line = text
		}
		if p, ok := core.ResolveCommand(line, st.session.Cwd, agentiloopHome()); ok {
			line = p
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
		// Line mode has no raw keyboard during a run, so Ctrl-C is its cancel key:
		// a signal while the agent works, or Ctrl-C at a permission prompt.
		sigCtx, stopSig := signal.NotifyContext(ctx, os.Interrupt)
		runCtx, stop := context.WithCancel(sigCtx)
		if p, ok := innerPolicy(st.agent.Policy()).(*interactivePolicy); ok {
			p.cancel = stop
		}
		tools.BeginUndoTurn()
		err = st.agent.Run(runCtx, line, renderLine)
		cancelled := runCtx.Err() != nil && ctx.Err() == nil
		stop()
		stopSig()
		if cancelled {
			fmt.Fprintln(os.Stderr, "\ncancelled; session kept")
		} else if err != nil {
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
	"/usage          tokens used since start and how full the context is\n" +
	"/export [file]  save the conversation as Markdown\n" +
	"/init           create a starter AGENTS.md for this project\n" +
	"/commands       list your custom commands (.agentiloop/commands/*.md)\n" +
	"/diff           show what changed in the git working tree\n" +
	"/todos          show the model's current task checklist\n" +
	"/undo           revert the file changes from the last prompt\n" +
	"/compact        summarize the conversation to free context\n" +
	"/sessions       list saved sessions (newest first)\n" +
	"/resume <id|n>  load a saved session into this REPL\n" +
	"/clear          clear context and start a new session\n" +
	"/setup          run the setup wizard again (provider, key, model)\n" +
	"/exit           quit"

// slashCommand runs a /command. Output lines go through say so the REPL
// (stderr) and the TUI (transcript) share one implementation; st.ask lets the
// /model picker read a choice in the REPL. /setup talks through st.setup (the
// terminal, or the TUI's transcript and input line).
func (st *cmdState) slashCommand(ctx context.Context, line string, say func(string)) error {
	cmd, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	agent := st.agent
	switch cmd {
	case "/clear":
		agent.Clear()
		tools.ClearUndo()
		tools.ClearTodos()
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
	case "/usage":
		u := agent.Usage()
		say(usageLine(u.Requests, u.InputTokens, u.OutputTokens))
		if last := agent.LastInputTokens(); last > 0 {
			if l := agent.Limits(); l != nil && l.ContextWindow != nil && *l.ContextWindow > 0 {
				say(fmt.Sprintf("context: %d of %d tokens (%d%%)", last, *l.ContextWindow, last*100 / *l.ContextWindow))
			} else {
				say(fmt.Sprintf("context: %d tokens", last))
			}
		}
	case "/export":
		st.session.History = append([]core.Message(nil), agent.History...)
		if len(st.session.History) == 0 {
			say("nothing to export yet")
			break
		}
		path := filepath.Join(st.session.Cwd, "agentiloop-"+st.session.ID+".md")
		if arg != "" {
			path = arg
			if !filepath.IsAbs(path) {
				path = filepath.Join(st.session.Cwd, arg)
			}
		}
		if err := os.WriteFile(path, []byte(st.session.ToMarkdown()), 0o644); err != nil {
			say(fmt.Sprintf("could not write %s: %v", path, err))
			break
		}
		say("exported the conversation to " + path)
	case "/init":
		path, err := core.InitInstructions(st.session.Cwd)
		if err != nil {
			say(err.Error())
			break
		}
		say("created " + path + "; edit it, then restart agentiloop to load it")
	case "/diff":
		text, err := gitChanges(st.session.Cwd)
		if err != nil {
			say(err.Error())
			break
		}
		say(text)
	case "/commands":
		say(core.CommandListing(core.LoadCommands(st.session.Cwd, agentiloopHome())))
	case "/todos":
		if t := tools.CurrentTodos(); t != "" {
			say(t)
		} else {
			say("no todo list yet (the model creates one for multi-step work)")
		}
	case "/undo":
		lines := tools.UndoLast()
		if lines == nil {
			say("nothing to undo")
			break
		}
		for _, l := range lines {
			say(l)
		}
		say("undid the file changes from the last prompt (shell commands are not undone)")
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
	case "/setup":
		// The running agent keeps its provider; a new key or provider needs a relaunch.
		if err := runWizard(ctx, st.saved, st.setup); err != nil {
			say(err.Error())
		} else {
			say("setup saved; restart agentiloop to use the new provider and credentials")
		}
	default:
		say(fmt.Sprintf("unknown command %s (try /help)", cmd))
	}
	return nil
}

// usageLine is the first line of /usage: tokens spent since agentiloop started.
func usageLine(requests, input, output uint64) string {
	return fmt.Sprintf("%d request(s) since start: %d input + %d output = %d tokens", requests, input, output, input+output)
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

// diffMaxLines is how many lines of /diff output are shown before the rest is cut.
const diffMaxLines = 200

// gitChanges is `git status --short` plus the diff against HEAD (or the working-tree diff in a repo with no commits).
func gitChanges(cwd string) (string, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return "", errors.New(msg)
			}
			return "", err
		}
		return string(out), nil
	}
	status, err := git("status", "--short")
	if err != nil {
		var nf *exec.Error
		if errors.As(err, &nf) {
			return "", fmt.Errorf("could not run git: %v", err)
		}
		return "", err
	}
	if strings.TrimSpace(status) == "" {
		return "no changes in the git working tree", nil
	}
	diff, err := git("diff", "HEAD", "--no-color")
	if err != nil {
		diff, _ = git("diff", "--no-color")
	}
	lines := strings.Split(strings.TrimRight(status, "\n"), "\n")
	if strings.TrimSpace(diff) != "" {
		dl := strings.Split(strings.TrimRight(diff, "\n"), "\n")
		lines = append(lines, "")
		if len(dl) > diffMaxLines {
			lines = append(lines, dl[:diffMaxLines]...)
			lines = append(lines, fmt.Sprintf("… %d more lines (run git diff for all)", len(dl)-diffMaxLines))
		} else {
			lines = append(lines, dl...)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// innerPolicy unwraps the --allow-tool/--deny-tool rules to reach the policy that prompts.
func innerPolicy(p core.PermissionPolicy) core.PermissionPolicy {
	if r, ok := p.(*core.Rules); ok {
		return r.Inner
	}
	return p
}

// appendExtra is base plus the --append-system-prompt text, if any non-blank text was given.
func appendExtra(base, extra string) string {
	if extra = strings.TrimSpace(extra); extra == "" {
		return base
	}
	return base + "\n\n" + extra
}

// jsonResult is the --json result object for a one-shot run (keys sorted, like the Rust twin).
func jsonResult(result string, runErr error, sessionID, provider, model string, u core.Usage) string {
	v := map[string]any{
		"result":     result,
		"is_error":   runErr != nil,
		"session_id": sessionID,
		"provider":   provider,
		"model":      model,
		"usage":      map[string]uint64{"requests": u.Requests, "input_tokens": u.InputTokens, "output_tokens": u.OutputTokens},
	}
	if runErr != nil {
		v["error"] = runErr.Error()
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(b.String(), "\n")
}

// expandStdin joins the prompt words; each lone "-" word becomes the stdin text (read once, on first use).
func expandStdin(words []string, read func() (string, error)) (string, error) {
	has := false
	for _, w := range words {
		if w == "-" {
			has = true
		}
	}
	if !has {
		return strings.Join(words, " "), nil
	}
	piped, err := read()
	if err != nil {
		return "", fmt.Errorf("could not read stdin: %w", err)
	}
	piped = strings.TrimRight(piped, " \t\r\n")
	if strings.TrimSpace(piped) == "" {
		return "", errors.New("`-` asks for the prompt text on stdin, but stdin was empty")
	}
	parts := make([]string, len(words))
	for i, w := range words {
		if w == "-" {
			w = piped
		}
		parts[i] = w
	}
	return strings.Join(parts, "\n\n"), nil
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
