# AgentiLoopGo

> **Temporary README.** It was put together from the current source and will be replaced by full documentation later.

AgentiLoopGo is a Go library for building an autonomous coding agent. The agent runs in a loop: it observes, thinks, acts, and repeats. It works with several model providers, has built-in filesystem and shell tools, and can call tools on MCP servers.

Module: `github.com/AgentiLoop/AgentiLoopGo` (Go 1.23)

## Packages

| Package | What it does |
|---|---|
| `core` | The agent loop (`Agent.Run`), the provider and tool interfaces, messages and content blocks, permission policies, history compaction, and sessions saved to disk |
| `provider` | Model backends: Anthropic Messages API, OpenAI-compatible Chat Completions (OpenAI, Ollama, LM Studio, Groq, OpenRouter, …), and oMLX (local MLX server) |
| `tools` | Built-in tools: `read_file`, `write_file`, `edit_file`, `list_dir`, `bash` |
| `mcp` | MCP client with stdio and HTTP transports, plus config loading from a user file and the project's `.mcp.json` |
| `examples/mcp-example-server` | A sample MCP server |

## Provider selection

`provider.FromEnv(name)` builds a backend by name (`anthropic`, `openai`, `omlx`). If no name is given, it picks one from the environment in this order:

1. **Anthropic**: `ANTHROPIC_API_KEY` or `ANTHROPIC_OAUTH_TOKEN` (optional `ANTHROPIC_BASE_URL`)
2. **OpenAI-compatible**: `OPENAI_API_KEY` or `OPENAI_BASE_URL` (for example `http://localhost:11434/v1` for Ollama)
3. **oMLX**: `OMLX_BASE_URL`, `OMLX_PORT`, or `OMLX_API_KEY`

## Agent defaults

From `core.DefaultAgentConfig()`:

- Model: `claude-sonnet-5`
- Max tokens: 8192
- Max turns per run: 50
- The history is compacted once input reaches 150,000 tokens

## Minimal usage (sketch)

```go
p, err := provider.FromEnv("")
if err != nil { log.Fatal(err) }

agent := core.NewAgent(p, tools.DefaultRegistry(), core.AllowAll{},
    core.DefaultAgentConfig(), core.ToolContext{ /* cwd, etc. */ })

err = agent.Run(ctx, "List the files in this project", func(ev core.Event) {
    if t, ok := ev.(core.EvTextDelta); ok { fmt.Print(t.Text) }
})
```

## Tests

```sh
go test ./...
```

## License

PolyForm Noncommercial License 1.0.0. Noncommercial and personal use is permitted. See [LICENSE](LICENSE).
