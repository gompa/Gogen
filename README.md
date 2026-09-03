# GoGen — AI-Powered Coding Agent

GoGen is a self-hosted coding agent that works on real codebases. Point it at
a project and describe the change you want in plain English: it explores the
layout, searches across files, edits code, runs commands, and checks the
results — from your terminal, a web browser, or a one-shot CLI prompt.

It's built to be handed real work:

- **Runs anywhere** — full-screen TUI, browser UI with multiple chat panes and
  a built-in terminal, or `-p "..."` for a single prompt
- **Any OpenAI-compatible API** — OpenAI, local llama.cpp/Ollama servers, or a
  proxy; register several endpoints and pick a model per session
- **Safe by default** — read-only plan mode, a command guard (blocklist or
  allowlist), sandboxed commands, and explicit approval for destructive
  actions
- **Remembers** — sessions auto-save and resume, history auto-compacts to fit
  the model's context window, and todos/pins survive across turns
- **Knows your project** — project guidelines, `AGENTS.md`/`CLAUDE.md`
  instructions, skills, and auto-detected test/lint commands shape how it
  works in your repo
- **Extensible** — connect MCP servers for extra tools, spawn subagents for
  parallel work, and track tickets on a project kanban board

## Quick start

### Prerequisites

- Go 1.27+
- A C compiler (for CGO / tree-sitter), e.g. `gcc` on Linux
- An OpenAI-compatible API key

### Build and run

```bash
go build -o gogen .
OPENAI_API_KEY=sk-... ./gogen /path/to/project
```

The TUI starts with your project as the working directory. Try prompts like:

```
> What test command does this project use?
> Find the flag parser in main.go and add a --dry-run flag
> Run the tests for the config package
```

Prefer a browser? Run with `--web` (http://127.0.0.1:8081). For a single
prompt without any UI, use `-p`:

```bash
OPENAI_API_KEY=sk-... ./gogen --web
OPENAI_API_KEY=sk-... ./gogen -p "List the top-level packages"
```

Non-loopback binds (e.g. `--host 0.0.0.0`) require a token. Without
`GOGEN_WEB_TOKEN`, one is auto-generated and persisted (`.gogen/web_token`,
0600) so **logins persist across restarts** — an already-paired device keeps
its session. Startup prints a short-lived pairing link, the pairing code in
plain text, and a QR code: the link opens the UI on this machine, the QR
(encoding the LAN address) logs a phone in on scan. The pairing code is
ephemeral by design — it expires after 15 minutes and is replaced at every
server start, so after any restart scan the freshly printed QR; the
long-lived token itself is never printed or logged. Set `GOGEN_WEB_TOKEN`
to use your own token:

```bash
GOGEN_WEB_TOKEN=secret ./gogen --web --host 0.0.0.0
# then open http://host:8081/?token=secret
```

Global mode (use `~/.config/gogen/` instead of project `.gogen/`):

```bash
GOGEN_MODE=global ./gogen
```

Build without tree-sitter (smaller binary, no syntax checks or symbol
extraction):

```bash
CGO_ENABLED=0 go build -o gogen-nocgo .
```

### Flags

| Flag | Description |
|------|-------------|
| `--web` | Run in web mode (listens on `:8081`) |
| `--host <host>` | Listen host for `--web` (e.g. `0.0.0.0`; falls back to `GOGEN_WEB_BIND` or `127.0.0.1:8081`) |
| `--dir <path>` | Set the working directory |
| `--global` | Ignore project `.gogen/`, use `~/.config/gogen/` instead |
| `--url <url>` | Override OpenAI API base URL (e.g., for local LLMs or proxies) |
| `--verbose` | Show full tool output in CLI mode |
| `-p <prompt>` | Run a single prompt and exit (non-interactive) |
| `--save-config` | Write effective config to `.gogen/gogen.conf` and guidelines to `.gogen/gogen.md` |
| `--save-config-secrets` | Include secrets when using `--save-config` (`openai_api_key`, `web_auth_token`, MCP `env`, provider keys) |
| `--save-config-path <file>` | Output path for `--save-config` config file (default `.gogen/gogen.conf`; project mode only — global mode always writes to `~/.config/gogen/config.yaml`) |

The first positional argument is also treated as the working directory (overridden by `--dir`); a second positional argument runs as a single prompt, equivalent to `-p`.

### TUI commands

While in the TUI:

| Command | Description |
|---------|-------------|
| `help` or `/help` | Show available commands |
| `exit`, `quit`, `/exit`, or `/quit` | Quit |
| `dir <path>` | Change working directory |
| `compact` or `/compact` | Manually compact conversation history |
| `/models` | List available models |
| `/models <name>` | Switch to a different model |
| `/plan` | Enable plan mode (read-only) |
| `/act` | Enable act mode (full tools) |
| `/mode` | Show current mode |
| `/think` | Set thinking/reasoning level (`off`/`low`/`medium`/`high`) or show the current level |
| `/context` | Show context window usage (tokens used, limit, compact threshold) |
| `/new` | Start a fresh session; previous session is saved to disk |
| `/subagents` | Show nested (subagent) sessions that ran in this session, with their final reports (when `subagent: on`) |
| `/resume` | List saved sessions (with message count and label) |
| `/resume <id>` | Restore a saved session |
| `/resume latest` | Restore the most recent session other than the current one |
| `/resume del <id>` | Delete a saved session |
| `sessions` | Alias for `/resume` (list sessions) |
| `/fork` | Fork a new session from the last assistant message (`/fork <N>` from message N) |
| `/verbose` | Toggle verbose tool output |
| `/save-config` | Write effective config to `.gogen/gogen.conf` |

Type `/help` for the full list.

## Supported languages

Tree-sitter is bundled for **26 languages** (syntax checking after edits):

Go, Python, JavaScript, TypeScript, TSX, Rust, Java, Kotlin, C, C++, C#, PHP, Ruby, Scala, SQL, HTML, CSS, JSON, Bash, YAML, TOML, Lua, HCL, Zig, Dockerfile, Make

**Symbol extraction** (`list_definitions`) has dedicated queries for **19** of
these: Go, Python, JavaScript, TypeScript, TSX, Rust, Java, Kotlin, C, C++,
C#, PHP, Ruby, Scala, SQL, Bash, Lua, HCL, Zig. JSON, HTML, CSS, YAML, TOML,
Dockerfile, and Make get syntax checks only.

Tree-sitter requires **CGO** at build time (enabled by default on Linux); set
`CGO_ENABLED=0` to build without it — tree-sitter features are then stubbed
out.

## Configuration

Settings load from **CLI flags**, **environment variables**, and **`.gogen/gogen.conf`** (pure YAML). Precedence: **env > CLI flags > .conf > defaults**.

### Project config (`.gogen/gogen.conf`)

YAML config

```yaml
command_safety: blocklist
openai_model: gpt-4o
mcp: on
mcp_servers:
  - name: fetch
    command: npx
    args: ["-y", "@modelcontextprotocol/server-fetch"]
board: on          # project kanban board (board tool + web board tab)
subagent: on       # subagent tool (nested sessions)
subagent_max_depth: 2  # max nesting; 1 (default) = subagents cannot spawn subagents
subagent_max_concurrent: 4  # max subagents running at once per session (web); 4 (default)
subagent_model: gpt-4o-mini    # default subagent model (empty = inherit the parent's model)
subagent_thinking_level: high  # subagent reasoning effort (empty = inherit the parent's level)
job_notices: off   # notify the session when a background command finishes
skills: off        # skill tool (list/read over .gogen/skills + ~/.config/gogen/skills)
agent_instructions: off  # load AGENTS.md / CLAUDE.md workspace instruction files
web_bind: 0.0.0.0:8080  # web listen address (applies on next start; also GOGEN_WEB_BIND / --host)
# Configurable prompt templates ("" = the built-in default):
board_start_prompt: "You have been assigned board ticket #{id}: {title}..."  # board "Start agent" prompt
system_prompt: "You are a coding agent in {working_dir}..."                  # replaces the base system prompt
subagent_prompt: "You are a subagent...\n\nJob:\n{job}"                      # wraps subagent jobs
```

Config values follow a strict schema: unknown keys are ignored, `on`/`off`
settings are strings, booleans are `true`/`false`, list settings are
comma-separated strings (e.g. `command_allowlist: "go,git"`,
`treesitter_langs: "go,python"`), integers are YAML integers, and `mcp_servers`
is a YAML list of objects (`name`, `command`, `args`, `env`). An empty or zero
value means "use the default", with these exceptions where `0` is a real
setting:

| Key | `0` means |
|-----|-----------|
| `compact_threshold` | auto-compaction disabled (never compact automatically; manual `/compact` still works) |
| `compact_keep_recent_messages` | keep nothing recent: only the first user message survives compaction, everything after it is summarized |
| `max_tool_result_bytes` | no truncation cap on tool output |
| `compact_reserve_tokens` | reserve no tokens for new messages after compaction |

Negative values are invalid for these settings and fall back to defaults
(`session_max_age_days` is the exception — a negative value keeps sessions
forever). `compact_threshold` must be in `[0, 1]` when set (including the
explicit `0` that disables auto-compaction); values outside that range fall
back to `0.85`.
Note that disabling auto-compaction means the full conversation is sent to the
model each turn, so it will eventually exceed the model's context window.

The legacy `keep_recent_messages` key (and `GOGEN_KEEP_RECENT_MESSAGES` env
var) is still accepted and mapped onto `compact_keep_recent_messages` with a
deprecation warning, so existing project files keep working after the rename.

`--save-config` writes pure YAML (no `---` front-matter markers); `on`/`off`
values may be emitted quoted, which parses identically. Secrets
(`openai_api_key`, `web_auth_token`, MCP server `env`, provider `api_key`) are
omitted unless `--save-config-secrets` is passed, and options equal to their
built-in default are omitted entirely, so a saved config never bakes in a
default.

Snapshot effective settings:

```bash
./gogen --save-config --dir /path/to/project
```

### Project guidelines (`.gogen/gogen.md` or `GOGEN.md`)

Markdown files for agent instructions and rules:

```markdown
# Project guidelines

- Run `make test` before finishing Go changes. Use `make test-debug` (or `make check`) when touching debug-tagged packages (`internal/agent` view-drift, `internal/profiling`).
- Never modify files in vendor/.
```

Discovery order:
- Config: `.gogen/gogen.conf` → `GOGEN.conf` → front matter in `.gogen/gogen.md`/`GOGEN.md` (fallback)
- Guidelines: `.gogen/gogen.md` → `GOGEN.md` → `.gogen/rules.md` → `.cursor/rules/gogen.md`

Files without config (plain markdown) are treated as guidelines-only. The old combined format (`---` YAML front matter + body) still works as a fallback but `--save-config` now writes separate `.conf` and `.md` files.

### API and workspace

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENAI_API_KEY` | *(required)* | API key for an OpenAI-compatible endpoint |
| `OPENAI_MODEL` | *(empty)* | Model to use; leave empty to use the endpoint's default model or pick one with `/models` |
| `OPENAI_BASE_URL` | *(empty)* | API base URL (e.g. `https://api.openai.com/v1` or a local proxy) |
| `GOGEN_WORKING_DIR` | `.` | Default working directory |

### Multiple OpenAI-compatible providers

Beyond the default profile (`OPENAI_API_KEY` / `OPENAI_BASE_URL` /
`OPENAI_MODEL`), extra OpenAI-compatible endpoints can be registered in
`.gogen/gogen.conf`:

```yaml
openai_providers:
  - name: local
    base_url: http://localhost:11434/v1
    model: llama3.1
    api_key: ""   # endpoints that need none
```

or via the `GOGEN_OPENAI_PROVIDERS` env var (a JSON array of `{name, baseUrl,
apiKey, model}`, overriding the file). Models from all providers are
aggregated into the web model picker and routed to their owning endpoint; an
empty `base_url` means the official OpenAI endpoint. Provider `api_key`
values are never persisted by `--save-config` unless `--save-config-secrets`
is passed.

### Context management

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_CONTEXT_LIMIT` | `0` | Manual token limit override (`0` = resolve from model) |
| `GOGEN_COMPACT_THRESHOLD` | `0.85` | Fraction of context limit that triggers auto-compaction (the UI warns earlier, at 75%) |
| `GOGEN_COMPACT_KEEP_RECENT_MESSAGES` | `12` | Most recent messages kept verbatim when a compaction runs (older middle history is summarized; `0` keeps only the first user message) |
| `GOGEN_MAX_TOOL_RESULT_BYTES` | `262144` | Max bytes for tool output before truncation (matches web_fetch's 256 KB limit) |
| `GOGEN_COMPACT_RESERVE_TOKENS` | `4000` | Tokens reserved for new messages after compaction |
| `GOGEN_COMPACT_LAST_RESORT` | `condense` | `condense` or `error`: what happens when a single message cannot fit the context window even after all compaction (e.g. a fresh session whose first message is bigger than the window) |

When a single message is too large for the window, there is no middle history
to summarize — the message is the head and every request would be refused.
The last-resort condensation handles this: with `compact_last_resort: condense`
(the default) the message is condensed in place via the summarizer, the
original is archived to the session's archive sidecar
(`<session>.archive.jsonl` next to the session file), and the condensation is
announced in-band (TUI system line / web banner: message size vs window,
original archived). With `compact_last_resort: error` the request is not sent
at all: a clear diagnostic ("message is ~N tokens vs M window; shorten it or
start a fresh session (/new)") is returned instead of the raw provider
refusal. The path is strictly last-resort — it only fires when the request is
provably over the window after the forced compaction, and only when condensing
the single largest message would bring it under.

After each agent turn, GoGen shows context usage in the CLI (dim line) and web UI (sidebar meter). Use `/context` for a detailed breakdown. When the provider returns usage stats (`prompt_tokens` from the API, including streaming with `include_usage`), the display labels this as **last request**; otherwise it falls back to a local token estimate.

### Safety

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_COMMAND_SAFETY` | `blocklist` | `blocklist`, `allowlist`, or `off` |
| `GOGEN_COMMAND_ALLOWLIST` | *(empty)* | Comma-separated allowed command prefixes (allowlist mode) |
| `GOGEN_DELETE_APPROVAL` | `required` | Set to `off` to skip delete confirmation |

### Command execution

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_COMMAND_SANDBOX` | `off` | Sandbox mode: `off` or `bwrap` (bubblewrap when available) |
| `GOGEN_COMMAND_IDLE_TIMEOUT_SECS` | `120` | Foreground `execute_command` is killed after this many seconds without output (any output resets the window; background jobs are unaffected); also `command_idle_timeout_secs` in `.gogen/gogen.conf` |

The timeout is an **idle (no-output) timeout**, not a wall-clock cap: the
window is reset by any output the command produces, so a long-running command
that keeps printing is never killed. The legacy `command_timeout_secs` key (and
`GOGEN_COMMAND_TIMEOUT_SECS` env var) is still accepted and mapped onto
`command_idle_timeout_secs` with a deprecation warning; the renamed key/env
var win when both are present. Note the semantics change: the old value capped
total runtime, so an existing `command_timeout_secs: 600` now tolerates 600s
of silence rather than 600s total.

### Global mode

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_MODE` | *(empty)* | Set to `global` to use `~/.config/gogen/` instead of project `.gogen/` |

### Tree-sitter

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_TREESITTER` | *(on)* | Set to `off` to disable syntax checks and symbol extraction |
| `GOGEN_TREESITTER_LANGS` | *(all)* | Comma-separated subset, e.g. `go,python,rust` |
| `GOGEN_DEBUG_COMPARE_MESSAGES` | off | (debug builds only) Enable view-fingerprint comparison across turns |

### Project commands

| Config key | Description |
|------------|-------------|
| `test_command` | Override the test command auto-detection (e.g. `"make test"`) |
| `lint_command` | Override the lint command auto-detection (e.g. `"make vet"`) |

These can be set in `.gogen/gogen.conf` only — there is no CLI flag or environment variable equivalent.

### MCP

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_MCP` | off | Set to `on` to enable MCP (also set `mcp: on` in project config) |
| `GOGEN_MCP_SERVERS` | *(empty)* | JSON array of `{name, command, args, env}` (overrides file) |

### Agent features

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_BOARD` | off | Set to `on` to enable the project kanban board (board tool + web board tab; live-toggleable from the web settings modal) |
| `GOGEN_SUBAGENT` | off | Set to `on` to enable the subagent tool (nested sessions) |
| `GOGEN_SUBAGENT_MAX_DEPTH` | `1` | Maximum subagent nesting depth (main agent = depth 0); `1` = subagents cannot spawn subagents; values ≤ 0 fall back to the default |
| `GOGEN_SUBAGENT_MAX_CONCURRENT` | `4` | Maximum subagents running concurrently per session (web mode); spawning beyond it is refused. Interrupted (idle) subagents do not hold a slot. The TUI runs subagents one at a time, so it is unaffected; values ≤ 0 fall back to the default |
| `GOGEN_SUBAGENT_MODEL` | *(empty)* | Default model for spawned subagents (empty = inherit the parent's model; the tool's explicit `model` argument always wins) |
| `GOGEN_SUBAGENT_THINKING_LEVEL` | *(empty)* | Reasoning-effort level for spawned subagents (empty = inherit the parent session's live level; `off` = never send `reasoning_effort`). A level the subagent's final model does not accept is omitted at spawn time |
| `GOGEN_BOARD_START_PROMPT` | *(empty)* | Template for the agent started from a board ticket (`{id}` `{title}` `{description}` `{priority}` `{context}`; empty = built-in default) |
| `GOGEN_SYSTEM_PROMPT` | *(empty)* | Custom system prompt template (`{working_dir}`; replaces the built-in base prompt; project rules and plan mode still apply; empty = built-in default) |
| `GOGEN_SUBAGENT_PROMPT` | *(empty)* | Template wrapping subagent jobs (`{job}`; empty = built-in default) |
| `GOGEN_JOB_NOTICES` | off | Set to `on` to notify the session when a background command finishes (injects a summary + runs a turn) |
| `GOGEN_SKILLS` | off | Set to `on` to enable the skill tool (`skill` list/read over `.gogen/skills` + `~/.config/gogen/skills`) |
| `GOGEN_AGENT_INSTRUCTIONS` | off | Set to `on` to load `AGENTS.md` / `CLAUDE.md` workspace instruction files (below the project guidelines) |

### Session persistence

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_SESSION_PERSIST` | on | Set to `off` to disable session save/resume |
| `GOGEN_SESSION_MAX_COUNT` | `50` | Max saved sessions per working directory |
| `GOGEN_SESSION_MAX_AGE_DAYS` | `30` | Auto-delete sessions older than N days (negative = keep forever) |

`GOGEN_SESSION_MAX_COUNT` and `GOGEN_SESSION_MAX_AGE_DAYS` can also be set in `.gogen/gogen.conf` (`session_max_count`, `session_max_age_days`); env vars take precedence.

### Sessions and debug

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_CLI_VERBOSE` | off | Verbose tool output in CLI |
| `GOGEN_DEBUG_LOG` | *(empty)* | Path to JSON debug log |
| `GOGEN_DEBUG_SESSION` | *(empty)* | Session id in debug logs |

### Web server

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_WEB_BIND` | `127.0.0.1:8081` | Listen address for `--web` (e.g. `0.0.0.0:8080` to accept remote connections); also `web_bind` in `.gogen/gogen.conf` (the settings modal's web-bind field persists it for the next start) |
| `GOGEN_WEB_TOKEN` | *(empty)* | Required for non-loopback binds; auto-generated and persisted (`.gogen/web_token`) when `--web --host` is used without one — startup prints a short-lived pairing link + QR; also `web_auth_token` in `.gogen/gogen.conf` |
| `GOGEN_WEB_ALLOWED_ORIGINS` | *(empty)* | Comma-separated host allowlist for WebSocket CORS; empty uses localhost defaults; also `web_allowed_origins` in `.gogen/gogen.conf` |
| `GOGEN_WEB_TLS_CERT` | *(empty)* | Path to PEM certificate file for TLS; also `web_tls_cert_file` in `.gogen/gogen.conf` |
| `GOGEN_WEB_TLS_KEY` | *(empty)* | Path to PEM key file for TLS; also `web_tls_key_file` in `.gogen/gogen.conf` |
| `GOGEN_WEB_MAX_ACTIVE_SESSIONS` | `8` | Cap on concurrently active sessions (panes); also `web_max_active_sessions` in `.gogen/gogen.conf` |
| `GOGEN_WEB_APPROVAL_HOLD_SECS` | `0` | Keep pending delete approvals alive for N seconds after the last client detaches (a reconnecting client is re-notified and can answer); `0` auto-denies immediately on detach. Also `web_approval_hold_secs` in `.gogen/gogen.conf` |

Non-loopback binds without TLS log a warning: the auth token is sent in plain
text. Set `GOGEN_WEB_TLS_CERT` / `GOGEN_WEB_TLS_KEY` (or the file keys) to
enable TLS.

### Web UI

The web UI supports **multiple chat panes** on one page. Each pane is an
independent session with its own history, mode, thinking level, model, and
in-flight turn — changing the model in one pane never affects another, and a
pane's model is remembered when the session is resumed later. Because the
panes share one workspace, the working directory is a single server-wide
setting: it can only be changed in global mode (`gogen --global`); in project
mode it is fixed to the project directory and the input is hidden.

- The sidebar is ONE list of sessions ordered by each session's last output
  (most recent first). Whether a session is focused, open, or responding
  shows as the row's status dot/highlight — never as a different position:
  focusing or activating a session does not reorder the list, only new
  output does. **New** opens a fresh pane alongside the current one (which
  keeps streaming); clicking an open pane focuses it, and **✕** closes it —
  any
  in-flight turn is cancelled and the session's live runtime is released, but
  it stays saved and can be resumed later. Deleting a saved session (✕, with
  confirmation) removes it permanently. Typed `/new`, `/resume <id>`, and
  `/fork N` replace the *current* pane's session.
- **Background panes keep running**: a turn in a non-focused pane continues
  server-side (amber "responding" indicator in the sidebar) and notifies once
  when it starts. Focus the pane to see it.
- **Disconnect ≠ cancel**: closing the tab or losing the connection does not
  stop the current turn — it completes server-side and is saved. When you
  reconnect, open panes re-attach automatically and show "Resuming…" while a
  still-running turn finishes. The **Cancel** button (or `Esc` while
  streaming) is the only way to stop a turn, and it works even from a fresh
  connection.
- **Idle sessions release themselves**: a session whose last tab closes while
  no turn is running is dropped from memory (it stays saved). The violet
  "resume to continue" indicator only appears for sessions that are genuinely
  live — open in another tab, or with a turn still running server-side.
- A built-in **Terminal** tab provides an interactive user shell (one per
  connection, respawnable after it exits) plus per-command terminal tabs that
  stream `execute_command` output live, with running/exit status dots and a
  restart button. The editor runs on its own socket, so saving a file is never
  blocked by a streaming turn.
- The settings modal is organized into sidebar sections (Editor, Chat, Global,
  Agent, Security, Context, Tools, Sessions, Server, MCP, Providers); opening
  it auto-selects the section matching the screen you are on, and manual
  choices are remembered per browser.

#### Board and subagents

- The settings modal's **Agent** group toggles the **Project board** and
  **Subagents** features live, no restart. The toggles are server-backed:
  they persist to `.gogen/gogen.conf` and every tab stays in sync via the
  config push.
- With `board: on`, a **Board** tab appears next to Chat/Editor: a kanban
  view (backlog, ready, in_progress, in_review, blocked, done) with
  drag-and-drop moves, a "New card" form (title, acceptance criteria,
  priority), and inline card detail with an activity log. Agent board-tool
  mutations re-render the board live; the tab is hidden while the feature is
  off. Each card has a **▶ Start** button that opens a small popover with a
  per-ticket model picker, a model-aware **reasoning effort** picker
  (**Inherit** = the active pane's live level, **Off**, or the model's
  accepted values), and an editable prompt — then claims the ticket and
  starts a dedicated agent session seeded with it; the button becomes
  **Open agent** and switches to the chat tab with the session attached.
  The started session runs headless until then — it survives closing the
  tab, and its delete approvals pop the approval modal right on the board
  (if the initiating tab is closed, approvals are denied until the session
  is reopened). The choices are stored on the ticket so the popover
  pre-fills on the next start; the start prompt template is editable in
  the settings modal (`board_start_prompt`).
- With `subagent: on`, subagents appear as **nested rows** under their parent
  session in the sidebar. A colored dot marks a live child — amber
  **responding** while running, red **failed** on error, green **done** on
  success — whether it is open as a pane here or live in another tab; a
  settled child that is neither open nor live shows a muted text status
  instead of a dot. Clicking a
  nested row opens the child as a normal pane (live transcript, Cancel, ✕),
  which is the escape hatch for a subagent stuck in a loop. The Agent tab's
  **Default subagent model** picker and the **System prompt** / **Subagent
  prompt** textareas persist to `.gogen/gogen.conf` and apply to both web and
  TUI subagents (`subagent_model`, `system_prompt`, `subagent_prompt`). The
  prompt fields are pre-populated with the effective templates and a "Reset to
  default" button restores the built-in; a value equal to the built-in default
  is treated as unset, so the default text is never baked into the config
  file.
- **TUI subagents are foreground-only in v1**: the TUI's `subagent` tool runs
  the child to completion inline and returns the final report, but
  `run_in_background` and the control tools (`subagent_fork`, `list_agents`,
  `send_message`, `interrupt_agent`, `report`) are web-only — they depend on
  the web server's session registry and delivery worker, which the TUI does
  not host.
- **Max concurrent subagents** (`subagent_max_concurrent`, default `4`): caps
  how many subagents may run at the same time for one session (web mode). The
  Agent tab's numeric input live-toggles it like the depth field; spawning
  beyond the cap is refused with an error the agent can act on
  (`interrupt_agent` a running child or wait for one to finish). The TUI runs
  subagents one at a time, so the setting has no effect there.
- **Subagent reasoning effort** (`subagent_thinking_level`): the Agent tab's
  **Subagent reasoning effort** picker sits next to the model picker and is
  model-aware — its options are the selected subagent model's accepted
  values (the active session's model values while the model is "Inherit").
  The leading **Inherit** option (the default, empty value) makes spawned
  subagents run with the parent session's live thinking level; **Off** never
  sends `reasoning_effort`. A level the subagent's final model does not
  accept is omitted at spawn time (the tool's explicit `model` argument can
  override the configured model, so validity is resolved against the child's
  model, never at save time). `subagent_fork` always inherits the parent's
  level.

### Web tools (web_fetch / web_search)

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_WEB_FETCH` | on | Enable `web_fetch` tool (`on`/`off`) |
| `GOGEN_WEB_SEARCH` | on | Enable `web_search` tool (`on`/`off`) |
| `GOGEN_WEB_FETCH_MODE` | `https` | `https` (default) or `all` (allow HTTP) |
| `GOGEN_WEB_SEARCH_BACKEND` | *(empty)* | `brave` for Brave Search API; empty uses DuckDuckGo |
| `GOGEN_WEB_SEARCH_API_KEY` | *(empty)* | API key for Brave Web Search |
| `GOGEN_WEB_ALLOWED_DOMAINS` | *(empty)* | Comma-separated domain suffix allowlist for fetch |

### API compatibility

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_PRESERVE_REASONING` | `auto` | Controls `preserve_reasoning` for llama.cpp endpoints: `auto`, `on`, or `off` |

### Example
```bash
export OPENAI_API_KEY=sk-...
export OPENAI_MODEL=gpt-4o
export OPENAI_BASE_URL=https://api.openai.com/v1
export GOGEN_WORKING_DIR=/path/to/your/project
export GOGEN_COMMAND_SAFETY=blocklist
export GOGEN_DELETE_APPROVAL=required
export GOGEN_WEB_TOKEN=my-secret-token
export GOGEN_WEB_SEARCH_BACKEND=brave
export GOGEN_WEB_SEARCH_API_KEY=BSA...
export GOGEN_COMMAND_SANDBOX=bwrap
export GOGEN_PRESERVE_REASONING=on

./gogen
```

## Architecture

```
main.go
└── internal/
    ├── agent/       — Core agent logic, tool execution, safety guards
    ├── projectfile/ — .gogen/gogen.conf and .md file loading/merging/writing
    ├── mcp/         — MCP stdio client and tool registry
    ├── session/     — Conversation persistence (JSON snapshots on disk)
    ├── skills/      — Skill discovery and loading (project + user dirs)
    ├── tui/         — Interactive terminal interface (Bubble Tea)
    ├── config/      — Environment-based configuration
    ├── contextmgr/  — Token-aware context window management and auto-compaction
    ├── llm/         — OpenAI API integration, model-aware token limits
    ├── server/      — WebSocket-based web server
    ├── treesitter/  — Source code parsing, symbol extraction, syntax checking
    ├── ioutil/      — Atomic file writes and helpers
    ├── randhex/     — Random-hex ID generation (sessions, background jobs, approvals)
    ├── streamutil/  — Token batching for streaming
    ├── modelinfo/   — models.dev context limit resolver
    ├── debuglog/    — Structured debug logging
    └── profiling/   — CPU/memory profiling (debug builds)
```

### Agent Tools

The agent has access to the following tools:

| Tool | Description |
|------|-------------|
| `repo_overview` | Summarize repo layout: top-level dirs, file counts, root files |
| `list_files` | List directory contents (optional `recursive`, `tracked_only`) |
| `glob` | Find files by glob pattern |
| `read_file` | Read a single file (optional `offset`/`limit` ranges, regex `search`) |
| `read_files` | Read multiple files at once |
| `list_definitions` | List functions/types with line numbers (tree-sitter AST, text fallback) |
| `write_file` | Create a new file (refuses existing paths) |
| `execute_command` | Run a shell command (with safety guardrails) |
| `replace_in_file` | Replace a literal string in a file (optional `replace_all`) |
| `delete` | Delete a file or empty directory (requires approval; non-empty directories refused) |
| `patch_file` | Apply surgical unified diff(s) (preferred; `dry_run`, `fuzzy`) |
| `show_diff` | Show git diff (working tree or path) |
| `search_code` | Regex/literal search across the codebase (optional `context_lines`) |
| `find_symbol` | Locate a symbol: `kind=def` (definition) or `refs` (references) |
| `git` | Git history/status/diff: `action=log`, `status`, `show` (read-only; plan mode) |
| `git_commit` | Commit (requires staged files) |
| `git_stage` | Stage files (empty = stage all) |
| `web_search` | Web search (DuckDuckGo Lite; no API key needed) |
| `web_fetch` | Fetch a web page as Markdown (optional `selector`/`query` extraction) |
| `download_file` | Download a raw file into the workspace (binary-safe, SSRF-protected) |
| `find_file` | Find files by name (case-insensitive substring) |
| `rename_symbol` | Rename a symbol across files (AST or text fallback) |
| `call_graph` | Call relationships / impact analysis for a symbol (`direction=impact`) |
| `todo` | Manage todo items: add/list/done/remove/clear |
| `board` | Project kanban board (when `board: on`): list/show/add/claim/move/block/comment/done/remove (remove deletes a card entirely). Available in plan mode too — the board is the coordination exception |
| `subagent` | Spawn a nested agent session for a job (when `subagent: on`); the final report returns as the tool result. Subagents cannot spawn subagents by default (`subagent_max_depth`) |
| `subagent_fork` | Fork a child session seeded with a deep copy of this session's history and run one turn on it (web only, when `subagent: on`) |
| `list_agents` | List live nested (subagent) sessions of this session: id, label, status, depth (web only, when `subagent: on`) |
| `send_message` | Send a message to a running background subagent of this session (web only, when `subagent: on`) |
| `interrupt_agent` | Cancel a running background subagent's in-flight turn (web only, when `subagent: on`) |
| `report` | Child-scoped: return a subagent's final report to its parent (web only, when `subagent: on`) |
| `session_rename` | Rename the current session |
| `context_pin_last` | Pin the last user message to survive compaction |
| `read_image` | Attach an image to the session context for vision-capable models (png/jpeg/gif/webp up to 3.5 MB; optional `detail=auto|low|high`) |
| `background_job` | Inspect or feed a background job (`execute_command background=true`): `action=status` (output tail), `action=cancel`, or `action=input` (write to the job's stdin) |

Additional tools arrive at runtime from connected MCP servers as `mcp_<server>_<tool>`.

## Safety

- **Command Guard** — Shell commands are filtered through a safety layer. In `blocklist` mode, dangerous patterns (`sudo`, `rm -rf /`, `curl | bash`) are blocked. In `allowlist` mode, only explicitly listed commands may run. Set `GOGEN_COMMAND_SAFETY=off` to disable.
- **Delete Approval** — File deletion requires explicit user confirmation before proceeding (unless `GOGEN_DELETE_APPROVAL=off`).
- **Patch-First Edits** — The agent prefers `patch_file` (unified diffs) over full file rewrites to minimize accidental data loss.
- **Syntax Checking** — After edits, syntax errors are detected via tree-sitter for supported languages.

## Development

The Makefile covers the common tasks; `make check` runs the full local suite
sequentially (it does not auto-update dependencies — use `make update` for
that):

```bash
make check        # fmt, tidy, tests (incl. -race), vet, staticcheck, vuln,
                  # no-CGO build, web UI lint + tests
make test         # go test -race ./...
make test-debug   # debug-tagged packages (view-drift, profiling)
make vet          # go vet ./...
make staticcheck  # go tool staticcheck ./...
make build-nocgo  # verify the documented no-tree-sitter build compiles
make lint-web     # lint the hand-maintained web UI JS (skipped without node)
make test-web     # jsdom regression tests for the web UI (skipped without node)
```

`make outdated` fails when any direct dependency or declared tool has a newer
version; `make update` upgrades them and tidies `go.mod`.

## License

[GNU AGPL-3.0](LICENSE)
