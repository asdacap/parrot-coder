# Parrot Coder

Parrot Coder is a local-first coding agent for macOS and Linux, implemented as
a single Go binary. It provides scrollback-preserving terminal chat, durable SQLite
sessions and event history, ChatGPT subscription OAuth, OpenAI-compatible
providers, permission-bound tools, transactional file changes,
session compaction, MCP, formatters, and bounded web fetching.

Local CLI commands use the same versioned HTTP/SSE API as `parrot serve`, but
invoke it in-process without opening a socket. Parrot does not use an alternate
terminal screen or execute a JavaScript plugin runtime.

## Install And Build

The easiest installation on macOS or Linux downloads the latest release binary
to `~/.local/bin` and verifies its checksum:

```sh
curl -fsSL https://raw.githubusercontent.com/asdacap/parrot-coder/main/install.sh | sh
```

Make sure `~/.local/bin` is on `PATH`, then confirm the installation:

```sh
parrot version
```

To select a release or installation directory, download the script and use
`--version VERSION` or `--bin-dir DIRECTORY`. The equivalent environment
variables `PARROT_VERSION` and `PARROT_INSTALL_DIR` also work when piping it to
`sh`.

The canonical build uses the Nix flake:

```sh
nix build
./result/bin/parrot version
```

For development:

```sh
nix develop
nix flake check
go build -o bin/parrot ./cmd/parrot
```

Without Nix, install Go 1.25 or newer and build directly:

```sh
./build.sh
./bin/parrot version
```

Tagged releases contain pure-Go binaries for macOS and Linux on amd64 and
arm64. Windows is not currently supported.

Parrot expects `awk`, `bash`, `bwrap`, `curl`, `find`, `git`, `grep`, `jq`, `rg`,
`sed`, `stat`, `tar`, and `xargs` to be available on `PATH` for agent commands.
It continues to start when one is absent, but prints a warning because
`exec_command` calls that use the missing utility may fail. The utilities detected
at startup are also included in a dedicated agent system-context source. Parrot
also detects optional development utilities, including common language
toolchains, build systems, package managers, and developer tools such as `go`,
`nix`, `cargo`, `rustc`, `gcc`, `clang`, `cmake`, `ninja`, `node`, `npm`,
`python3`, `java`, `docker`, and `kubectl`. Available optional utilities are
listed in agent context without warnings for tools that are absent.

`exec_command` runs in an OS sandbox without a permission prompt by default: the
sandbox is the boundary, so confined work is not asked about. Setting
`sandbox_permissions` to `disable_sandbox` always requires permission and runs
with the invoking user's local authority. When a command outlives
`exec_command`'s yield window, it remains available as a shell process and sends
a completion message to the session that started it. Use the returned
`process_id` with `write_stdin`, `wait_process`, or `task_interrupt` to interact
with it.
Linux requires Bubblewrap and
unprivileged user namespaces; the Nix package and development shell include
Bubblewrap. macOS uses the system Seatbelt executable. The host filesystem is
read-only, the workspace and its Git metadata are writable except for existing
`.parrot` and project configuration metadata along the startup working-directory
path, and host network access is retained. Linked-worktree Git metadata is also
made writable outside the workspace. Sandboxed shell commands in the same
session share a private temporary directory, exposed as `/tmp` on Linux and
through `TMPDIR` on macOS. Shell commands fail closed when
the sandbox is unavailable.

## Quick Start

### ChatGPT OAuth

Log in with a ChatGPT subscription, then select one of the models printed by
`parrot models`:

```sh
parrot auth login openai
# On a machine without a usable browser:
parrot auth login openai --no-browser

parrot models
parrot chat --model chatgpt/gpt-5.6-sol/high
```

OAuth credentials are stored in Parrot's private data directory. They are sent
only to the compiled OpenAI authentication and ChatGPT endpoints.
At startup, Parrot requests the ChatGPT Codex model catalog to obtain current
model names, context windows, and reasoning variants. If the catalog is
unavailable, Parrot keeps using its bundled model metadata. Model selectors use
the canonical `provider/model[/effort-variant]` form. Interactive `/model` and
`/effort` selections save that complete selector in the global `model` field and
make it the default for later sessions.
`parrot usage` (or `/usage` in chat) shows the remaining percentage and reset
time for the subscription's available rate-limit windows. This relies on an
upstream ChatGPT endpoint and requires a stored ChatGPT OAuth credential.

### Kimi

Kimi ships as two built-in provider presets, because they are two products with
two different keys. A credential is all either one needs — no `providers` entry.

**`kimi-code` — the Kimi For Coding subscription.** Its endpoint serves your
plan:

```sh
printf '%s' "$KIMI_API_KEY" | parrot auth login kimi-code --api-key-stdin
# Or simply export KIMI_API_KEY before starting Parrot.

parrot models
parrot chat --model kimi-code/kimi-k2-thinking
```

**`kimi-api` — the Moonshot platform API,** billed against a prepaid balance:

```sh
printf '%s' "$MOONSHOT_API_KEY" | parrot auth login kimi-api --api-key-stdin
parrot chat --model kimi-api/kimi-k2-thinking
```

Pick the one matching the key you hold: a subscription key on `kimi-api` fails
with an insufficient-balance error, because the plan does not fund the
pay-as-you-go endpoint. `parrot usage` reports the remaining balance for
`kimi-api`; `kimi-code` has no balance route, so it reports nothing.

Both presets speak the chat-completions protocol. `parrot models` lists whatever
the endpoint serves, fetched from `/v1/models` at startup; the presets only
carry the metadata a model list cannot express — context windows and reasoning
variants — for the Kimi K2 models. Correct or extend any of it with a
`providers.kimi-code` or `providers.kimi-api` entry, for example to use a
regional endpoint or to give a newly served model its real context window.

### OpenRouter

OpenRouter is a built-in provider preset that exposes a large, frequently
changing catalog through the OpenAI-compatible chat-completions protocol. No
`providers` entry is needed:

```sh
printf '%s' "$OPENROUTER_API_KEY" | parrot auth login openrouter --api-key-stdin
# Or simply export OPENROUTER_API_KEY before starting Parrot.

parrot models
parrot chat --model openrouter/openai/gpt-4o
```

`parrot models` lists whatever OpenRouter serves from `/api/v1/models` at
startup. OpenRouter model IDs include a vendor prefix (for example
`openai/gpt-4o`, `anthropic/claude-3.7-sonnet`), which model selection keeps as
the model portion of `openrouter/<vendor>/<model>`. Correct or extend any
catalog metadata with a `providers.openrouter.models` entry. To set the
optional ranking headers, add them under `providers.openrouter.headers`:

```yaml
providers:
  openrouter:
    headers:
      HTTP-Referer: https://your-app.example
      X-Title: Parrot Coder
```

OpenRouter has no usage route, so `parrot usage` reports nothing for it.

### OpenCode Go

OpenCode Go is a built-in provider preset for the OpenCode Go low-cost
subscription, which serves a curated, frequently changing set of open coding
models (GLM, Kimi, Qwen, DeepSeek, MiniMax, MiMo, Grok) through the
OpenAI-compatible chat-completions protocol. No `providers` entry is needed:

```sh
printf '%s' "$OPENCODE_GO_API_KEY" | parrot auth login opencode-go --api-key-stdin
# Or simply export OPENCODE_GO_API_KEY before starting Parrot.

parrot models
parrot chat --model opencode-go/glm-5.2
```

`parrot models` lists whatever OpenCode Go serves from `/v1/models` at startup.
Model IDs carry no vendor prefix (for example `glm-5.2`, `kimi-k3`,
`deepseek-v4-flash`), so model selection splits `opencode-go/<model-id>`
cleanly on the first slash. Correct or extend any catalog metadata with a
`providers.opencode-go.models` entry.

OpenCode Go now has a usage endpoint (`/zen/go/v1/usage`), so `parrot usage`
shows the current subscription windows and credit balance.

### Alibaba Cloud Model Studio

Alibaba ships as two built-in provider presets, because its subscriptions are
two isolated billing channels with two different keys — both of which look
alike, starting with `sk-sp-`. A credential is all either one needs.

**`alibaba-token-plan` — the Token Plan (Team Edition),** which serves the
current flagship models (Qwen3.x, GLM, DeepSeek):

```sh
printf '%s' "$ALIBABA_TOKEN_PLAN_API_KEY" | parrot auth login alibaba-token-plan --api-key-stdin
# Or simply export ALIBABA_TOKEN_PLAN_API_KEY before starting Parrot.

parrot models
parrot chat --model alibaba-token-plan/qwen3.7-plus
```

**`alibaba-coding-plan` — the Coding Plan,** a separate subscription serving a
coding-focused catalog:

```sh
printf '%s' "$ALIBABA_CODING_PLAN_API_KEY" | parrot auth login alibaba-coding-plan --api-key-stdin
parrot chat --model alibaba-coding-plan/qwen3-coder-plus
```

Pick the one matching the key you hold. The plans do not share endpoints: a
Coding Plan key on the Token Plan endpoint fails with `401 invalid_api_key`,
and a pay-as-you-go Model Studio key (`sk-`) works on neither. Neither plan
exposes a usage route, so `parrot usage` reports nothing for either.

Both presets speak the chat-completions protocol, and both endpoints serve a
model list carrying nothing but IDs, so the presets supply the context windows,
output limits, and reasoning efforts the catalog cannot express. Model IDs
carry no vendor prefix, so selection splits `alibaba-token-plan/<model-id>`
cleanly on the first slash. `/effort` offers the levels each model actually
accepts — the endpoint validates `reasoning_effort` per model and rejects the
rest. The Token Plan preset points at the Singapore region; for a Beijing
account, override `base_url`:

```yaml
providers:
  alibaba-token-plan:
    base_url: https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
```

### Compatible Endpoint

Create `~/.config/parrot/parrot.yaml`. Configuration stores the name of an
environment variable, never the API key itself:

```yaml
model: local/code-model
providers:
  local:
    type: compatible
    protocol: responses
    base_url: https://models.example.test/v1
    api_key_env: LOCAL_MODEL_API_KEY
    models:
      code-model:
        name: Code Model
        context: 128000
        max_tokens: 16384
        tools: true
```

Then export the key for the Parrot process and start chat:

```sh
export LOCAL_MODEL_API_KEY='...'
parrot chat
```

Alternatively, store a compatible-provider key without putting it in command
arguments or shell history:

```sh
printf '%s' "$LOCAL_MODEL_API_KEY" | parrot auth login local --api-key-stdin
```

Plain HTTP is rejected except for an explicitly opted-in loopback endpoint.
See [Configuration](docs/configuration.md) for the complete schema and service
examples.

## CLI Reference

`parrot` starts chat only when stdin is a terminal; otherwise it prints help.
The global `--no-color` flag, `NO_COLOR`, and `TERM=dumb` disable interactive
styling.

```text
parrot chat [PROMPT] [--continue | --session ID]
            [--model PROVIDER/MODEL[/EFFORT-VARIANT]] [--mode ID] [--thinking]
parrot run [PROMPT] [--continue | --session ID]
           [--model PROVIDER/MODEL[/EFFORT-VARIANT]] [--mode ID] [--thinking]
           [--format text|jsonl]
           [--interactive-prompts]
parrot models [--format lines|json]
parrot usage [--format lines|json]
parrot modes
parrot agents  # reusable child agents: explorer, worker, and read-only reviewer
parrot session list
parrot session show ID
parrot session compact ID
parrot session delete ID
parrot auth list
parrot auth login openai [--no-browser]
parrot auth login PROVIDER --api-key-stdin
parrot auth logout PROVIDER
parrot serve [--host 127.0.0.1] [--port 4096]
parrot version
parrot help
```

Prompt text may be supplied as one argument, piped on stdin, or both. Piped
stdin is always prompt data. Reviewed `apply_patch` operations inside the current
workspace, bounded web fetches, MCP calls, and sandboxed `exec_command` calls run
without a prompt; only `exec_command` with `disable_sandbox`, `set_config`, and
`request_write_permission` ask.
`--interactive-prompts` explicitly opens `/dev/tty` for permission and question
replies; without it a prompt is denied. `serve` refuses non-loopback addresses because the HTTP API has no
authentication layer.

On a real terminal, chat uses a bounded inline editor without an alternate
screen. User messages start with `$` and use a dark-blue background with green
text; assistant messages start with `-` and use a dark-green background
with pale-cyan text. Editable user input is green. The input area has a separate
top border. The editor remains active while the agent works:
Enter steers the active turn at the next safe provider-turn boundary, safe slash
commands run immediately, and a spinner
marks the busy prompt. Human-facing outcomes pair icons with text, such as
`✓ Done`, `✗ Failed`, and `■ Interrupted`. Complete rows of a streaming assistant
response are written directly to normal terminal scrollback while only its
unfinished row remains live. Chat can start without a configured model.
Submitting a normal prompt with no model opens a
picker and preserves the draft until a model is selected. Explicit non-TTY
`parrot chat` retains the same role markers in a deterministic, ANSI-free line
REPL. `/status` shows the active agent, model, session, and project.

Interactive chat has an equivalent for every top-level CLI action: `/help`,
`/version`, `/run PROMPT`, `/chat`, `/models`, `/usage`, `/modes`, `/agents`,
`/session`, `/auth`, and `/serve`. It also supports `/model`, `/effort`,
`/mode`, `/sessions`, `/resume`, `/new`, `/clear`, `/compact`, `/connect`, `/thinking`,
`/status`, custom commands, and `/exit`.

The management namespaces mirror their CLI forms: `/session list`, `/session
show ID`, `/session compact [ID]`, `/session delete ID`, `/auth list`, `/auth
login [PROVIDER [KEY|--no-browser]]`, and `/auth logout PROVIDER`.

Bare `/auth login` is interactive: it offers the built-in providers plus any
that already have a credential or a model, then prompts for the key. Naming the
provider skips the picker, and supplying the key or exporting `PARROT_API_KEY`
skips the prompt. A key entered either way is handled locally — builtin slash
commands never reach the model or the session transcript, and chat input history
is only held in memory — but it is echoed on screen, since there is no masked
input. On a shared display prefer `PARROT_API_KEY` or the CLI's
`--api-key-stdin`.
`/serve` starts the existing local runtime in the background and supports
`/serve status` and `/serve stop`. Credential changes take effect in a new chat.
Use `/effort` to pick from the active model's reasoning variants, or pass a
variant name directly, for example `/effort high`. The command rewrites the
selected model string: with `chatgpt/gpt-5.6-sol` active, `/effort high` selects
and persists `chatgpt/gpt-5.6-sol/high`. A variant name is Parrot's stable
selector suffix; its model metadata maps that name to the reasoning-effort value
sent to the provider.

Interactive chat remembers the active conversation by canonical working
directory. On startup, Parrot reopens the previous session when its recorded
host and PID are no longer running. A second live Parrot in the same directory
gets a separate session. `/clear` (and its `/new` alias) immediately opens a
fresh durable session without deleting the previous conversation.

Keybindings: Enter submits, Ctrl-J inserts a newline, arrows edit/navigate,
Ctrl-A/Ctrl-E move to the beginning/end of the current line,
Ctrl-K clears from the cursor to the end of the line, history and choices,
Tab completes slash commands, Shift-Tab or Ctrl-X switches modes,
Escape cancels a picker or halts an active turn, idle Ctrl-C clears the current
edit, and Ctrl-D on an empty prompt exits. During a turn, the first Ctrl-C
requests cancellation and a second exits with status 130. Unbound control keys
and malformed keyboard input are ignored rather than terminating the chat.

## Data Paths

Parrot follows XDG variables on both macOS and Linux:

| Purpose | Default path |
| --- | --- |
| Configuration, skills, commands | `~/.config/parrot/` |
| Credentials | `~/.config/parrot/credentials.json` |
| SQLite state | `~/.local/state/parrot/parrot.db` |
| Process diagnostics | `~/.local/state/parrot/diagnostics/` |
| Managed tool output | `~/.cache/parrot/outputs/` |

`XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, and `XDG_CACHE_HOME`
replace their corresponding parent directories. Application directories are
created with mode `0700`; credential, database, and diagnostic files use mode
`0600`.

### Migration from previous versions

Parrot migrates state from earlier layouts automatically:

- A legacy `credentials.json` that was stored under the data directory is moved
  to the config directory, unless a credentials file already exists at the new
  location.
- A legacy shared `parrot.db` in the state directory is adopted once: its
  sessions are copied into per-session databases, and the original file is
  renamed aside with a `.migrated-` timestamp suffix rather than deleted.

### Process Diagnostics

`diagnostics/parrot.jsonl` is a rotating structured log of process starts,
commands, signals, application startup and shutdown, maintenance, session runs,
provider requests and retries, compaction, tool execution, HTTP requests,
recovered panics with stack traces, and exit codes. `diagnostics/crash.log`
receives a duplicate of Go's report for an unhandled panic or fatal runtime
error, even when stderr belongs to a terminal
or supervisor that later disappears. Each file rotates at 4 MiB and retains
three backups.

While running, Parrot also keeps a small marker under `diagnostics/runs/`. An
orderly exit removes it. If `SIGKILL`, an out-of-memory kill, power loss, or a
similar event prevents cleanup, a later invocation records
`unclean_previous_exit`. No in-process logger can write at the moment of
`SIGKILL` or power loss, so OS logs may still be needed to identify the cause.

## Security Model

Provider output, model tool calls, project files, project configuration, MCP
servers, formatters, and fetched web content are untrusted.
Parrot bounds network bodies and streams, rejects cross-origin authenticated
provider redirects, pins validated DNS answers for each web-fetch host, strips
terminal controls, and binds approvals to canonical inputs, resources, and
operation hashes. File mutations revalidate canonical paths and preimage hashes,
stage writes in destination directories, and roll back runtime commit failures.

Permission to run an MCP server or formatter is permission to run
local code with the invoking user's authority. Parrot's shell sandbox cannot
protect against another
same-user process racing filesystem operations, and cannot guarantee exactly-once
remote provider execution after an uncertain crash. Multi-file filesystem
rollback is best effort across process or machine failure; the SQLite event and
projection transaction remains atomic.

Project-scoped configuration cannot introduce provider endpoints or credential
sources, MCP servers, formatter executables, or private web access. These
capabilities must be placed in the user's global config.

See [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md) for
reporting and detailed boundaries.

## Limitations

- macOS and Linux are supported; Windows process, path, and terminal semantics
  are not implemented.
- ChatGPT subscription endpoints and model availability are upstream behavior
  and may change independently of Parrot.
- The HTTP server is unauthenticated and loopback-only.
- Compatible providers vary; malformed or unsupported streaming dialects fail
  closed rather than being guessed.
- MCP supports configured stdio and HTTP transports, not arbitrary plugin code.
- Formatter commands must be absolute executable paths.
- Web fetch accepts bounded textual content only and blocks private destinations
  unless explicitly enabled.

## Development

Testing commands, race and fuzz gates, cross-build checks, and the local Nix
availability caveat are documented in [docs/testing.md](docs/testing.md).
