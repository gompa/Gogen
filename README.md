# GoGen — AI-Powered Coding Agent

GoGen is a self-hosted, terminal or web-based coding assistant that can explore, read, search, and edit source code using an LLM. Think of it as a locally-run coding agent you can query and direct to make real changes to your codebase.

## Features

- **Repository Exploration** — Top-level layout summary, directory listing, glob patterns, and symbol outlines before diving into files
- **File Operations** — Read, write, patch, replace, and delete files safely
- **Code Search** — Regex and literal string search across your codebase (ripgrep with fallback)
- **Symbol Extraction** — Lists functions, methods, classes, and types via [tree-sitter](https://tree-sitter.github.io/tree-sitter/) for 15 languages
- **Safe Edits** — Prefers unified diffs (`patch_file`) over full file rewrites; syntax error detection after edits
- **Command Execution** — Run shell commands with configurable safety modes (blocklist / allowlist / off)
- **Human-in-the-Loop** — Requires explicit approval for destructive actions (file deletes)
- **Context Management** — Auto-compacts conversation history when nearing token limits to stay within model context windows
- **Project Config** — Separate `.gogen/gogen.conf` (YAML) for settings and `.gogen/gogen.md` (or `GOGEN.md`) for guidelines. Precedence: **env > .conf > CLI flags > defaults**.
- **Plan Mode** — Read-only exploration via `/plan` (CLI) or web toggle; use `/act` to implement
- **MCP Client** — Connect stdio MCP servers for extended tools (`mcp_<server>_<tool>`)
- **Session Persistence** — Auto-save/resume conversations under `.gogen/sessions/` (set `GOGEN_SESSION_PERSIST=off` to disable)
- **Project Rules** — Guidelines from project file body, or rules-only files (`.gogen/rules.md`, plain `GOGEN.md`)
- **Two Modes** — Interactive CLI or web-based UI via WebSocket

## Supported Languages

Tree-sitter is bundled for **20 languages** (syntax checking after edits):

Go, Python, JavaScript, TypeScript, TSX, Rust, Java, C, C++, C#, PHP, Ruby, HTML, CSS, JSON, Bash, YAML, TOML, Lua, HCL

**Symbol extraction** (`list_definitions`) has dedicated queries for **15** of these: Go, Python, JavaScript, TypeScript, TSX, Rust, Java, C, C++, C#, PHP, Ruby, Bash, Lua, HCL. JSON, HTML, CSS, YAML, and TOML get syntax checks only.

Tree-sitter requires **CGO** at build time (enabled by default on Linux). Set `CGO_ENABLED=0` to build without it — tree-sitter features are then stubbed out.

## Quick Start

### Prerequisites

- Go 1.26+
- A C compiler (for CGO / tree-sitter), e.g. `gcc` on Linux
- An OpenAI-compatible API key

### Build

```bash
go build -o gogen .
```

Build without tree-sitter (smaller binary, no syntax checks or symbol extraction):

```bash
CGO_ENABLED=0 go build -o gogen .
```

### Run

**CLI mode** (interactive terminal):

```bash
OPENAI_API_KEY=sk-... ./gogen --cli
```

**Web mode** (browser UI on `:8080`):

```bash
OPENAI_API_KEY=sk-... ./gogen --web
```

### Flags

| Flag | Description |
|------|-------------|
| `--cli` | Run in interactive CLI mode |
| `--web` | Run in web mode (listens on `:8080`) |
| `--host <host>` | Listen host for `--web` (e.g. `0.0.0.0`; default `127.0.0.1`; also `GOGEN_WEB_BIND` for host:port) |
| `--dir <path>` | Set the working directory |
| `--url <url>` | Override OpenAI API base URL (e.g., for local LLMs or proxies) |
| `--verbose` | Show full tool output in CLI mode |
| `--save-config` | Write effective config to `.gogen/gogen.conf` and guidelines to `.gogen/gogen.md` |
| `--save-config-secrets` | Include `openai_api_key` when using `--save-config` |
| `--save-config-path <file>` | Output path for `--save-config` config file (default `.gogen/gogen.conf`) |

### CLI commands

While in CLI mode:

| Command | Description |
|---------|-------------|
| `exit` | Quit |
| `dir <path>` | Change working directory |
| `compact` or `/compact` | Manually compact conversation history |
| `/models` | List available models |
| `/models <name>` | Switch to a different model |
| `/plan` | Enable plan mode (read-only) |
| `/act` | Enable act mode (full tools) |
| `/mode` | Show current mode |
| `/context` | Show context window usage (tokens used, limit, compact threshold) |
| `/new` | Start a fresh session; previous session is saved to disk |
| `/resume` | List saved sessions (with message count and label) |
| `/resume <id>` | Restore a saved session |
| `/resume latest` | Restore the most recent session other than the current one |
| `sessions` | Alias for `/resume` (list sessions) |
| `/save-config` | Write effective config to `.gogen/gogen.conf` |

## Configuration

Settings load from **environment variables**, **`.gogen/gogen.conf`** (pure YAML), then CLI flags. Precedence: **env > .conf > flags > defaults**.

### Project config (`.gogen/gogen.conf`)

YAML config

```yaml
command_safety: blocklist
openai_model: gpt-4o
mcp_servers:
  - name: fetch
    command: npx
    args: ["-y", "@modelcontextprotocol/server-fetch"]
```

Snapshot effective settings:

```bash
./gogen --save-config --dir /path/to/project
```

### Project guidelines (`.gogen/gogen.md` or `GOGEN.md`)

Markdown files for agent instructions and rules:

```markdown
# Project guidelines

- Run `make test` before finishing Go changes.
- Never modify files in vendor/.
```

Discovery order:
- Config: `.gogen/gogen.conf` → `GOGEN.conf` → front matter in `.gogen/gogen.md`/`GOGEN.md` (fallback)
- Guidelines: `.gogen/gogen.md` → `GOGEN.md` → `.gogen/rules.md` → `.cursor/rules/gogen.md`

Files without config (plain markdown) are treated as guidelines-only. The old combined format (`---` YAML front matter + body) still works as a fallback but `--save-config` now writes separate `.conf` and `.md` files.

### Environment variables

### API and workspace

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENAI_API_KEY` | *(required)* | API key for an OpenAI-compatible endpoint |
| `OPENAI_MODEL` | `gpt-4o` | Model to use |
| `OPENAI_BASE_URL` | *(empty)* | API base URL (e.g. `https://api.openai.com/v1` or a local proxy) |
| `GOGEN_WORKING_DIR` | `.` | Default working directory |

### Context management

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_CONTEXT_LIMIT` | `0` | Manual token limit override (`0` = resolve from model) |
| `GOGEN_COMPACT_THRESHOLD` | `0.75` | Fraction of context limit that triggers auto-compaction |
| `GOGEN_KEEP_RECENT_MESSAGES` | `12` | Recent messages preserved during compaction |
| `GOGEN_MAX_TOOL_RESULT_BYTES` | `8192` | Max bytes for tool output before truncation |
| `GOGEN_COMPACT_RESERVE_TOKENS` | `4000` | Tokens reserved for new messages after compaction |

After each agent turn, GoGen shows context usage in the CLI (dim line) and web UI (sidebar meter). Use `/context` for a detailed breakdown. When the provider returns usage stats (`prompt_tokens` from the API, including streaming with `include_usage`), the display labels this as **last request**; otherwise it falls back to a local token estimate.

### Safety

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_COMMAND_SAFETY` | `blocklist` | `blocklist`, `allowlist`, or `off` |
| `GOGEN_COMMAND_ALLOWLIST` | *(empty)* | Comma-separated allowed command prefixes (allowlist mode) |
| `GOGEN_DELETE_APPROVAL` | `required` | Set to `off` to skip delete confirmation |

### Tree-sitter

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_TREESITTER` | *(on)* | Set to `off` to disable syntax checks and symbol extraction |
| `GOGEN_TREESITTER_LANGS` | *(all)* | Comma-separated subset, e.g. `go,python,rust` |

### MCP

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_MCP` | on | Set to `off` to disable MCP |
| `GOGEN_MCP_SERVERS` | *(empty)* | JSON array of `{name, command, args, env}` (overrides file) |

### Sessions and debug

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_SESSION_PERSIST` | on | Set to `off` to disable session save/resume |
| `GOGEN_CLI_VERBOSE` | off | Verbose tool output in CLI |
| `GOGEN_DEBUG_LOG` | *(empty)* | Path to JSON debug log |
| `GOGEN_DEBUG_SESSION` | *(empty)* | Session id in debug logs |

### Web server

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_WEB_BIND` | `127.0.0.1:8080` | Listen address for `--web` (e.g. `0.0.0.0:8080` to accept remote connections) |
| `GOGEN_WEB_ALLOWED_ORIGINS` | *(empty)* | Comma-separated host allowlist for WebSocket; empty uses localhost defaults |

### Example

```bash
export OPENAI_API_KEY=sk-...
export OPENAI_MODEL=gpt-4o
export OPENAI_BASE_URL=https://api.openai.com/v1
export GOGEN_WORKING_DIR=/path/to/your/project
export GOGEN_COMMAND_SAFETY=blocklist
export GOGEN_DELETE_APPROVAL=required

./gogen --cli
```

## Architecture

```
main.go
└── internal/
    ├── agent/       — Core agent logic, tool execution, safety guards
    ├── projectfile/ — GOGEN.md front matter parse/merge/write
    ├── mcp/         — MCP stdio client and tool registry
    ├── session/     — Conversation persistence
    ├── cli/         — Interactive terminal interface
    ├── config/      — Environment-based configuration
    ├── contextmgr/  — Conversation context management and auto-compaction
    ├── llm/         — OpenAI API integration, model-aware token limits
    ├── server/      — WebSocket-based web server
    └── treesitter/  — Source code parsing, symbol extraction, syntax checking
```

### Agent Tools

The agent has access to the following tools:

| Tool | Description |
|------|-------------|
| `repo_overview` | Summarize top-level directories, file counts, and root files |
| `list_files` | List files and directories (optional `recursive=true`) |
| `glob_files` | Find files by glob pattern |
| `read_file` | Read a single file (optional `offset`/`limit` for large files) |
| `read_files` | Read multiple files at once |
| `list_definitions` | Extract functions/methods/types from source (tree-sitter) |
| `search_code` | Regex or string search across the codebase (optional `context_lines`) |
| `find_references` | Find symbol references via word-boundary search |
| `write_file` | Write content to a file |
| `patch_file` | Apply a unified diff (preferred for edits; optional `dry_run`) |
| `replace_in_file` | Replace a search string in a file |
| `delete_file` | Delete a file (requires approval) |
| `execute_command` | Run a shell command (with safety guardrails) |
| `show_diff` | Show git diff for the working tree |
| `git_log` | Show recent commit history (read-only; plan mode) |
| `git_blame` | Show line attribution for a file (read-only; plan mode) |

## Safety

- **Command Guard** — Shell commands are filtered through a safety layer. In `blocklist` mode, dangerous patterns (`sudo`, `rm -rf /`, `curl | bash`) are blocked. In `allowlist` mode, only explicitly listed commands may run. Set `GOGEN_COMMAND_SAFETY=off` to disable.
- **Delete Approval** — File deletion requires explicit user confirmation before proceeding (unless `GOGEN_DELETE_APPROVAL=off`).
- **Patch-First Edits** — The agent prefers `patch_file` (unified diffs) over full file rewrites to minimize accidental data loss.
- **Syntax Checking** — After edits, syntax errors are detected via tree-sitter for supported languages.
