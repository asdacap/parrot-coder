# Parrot Coder

Parrot Coder is a local-first coding agent for macOS and Linux, implemented as
a single Go binary. It provides scrollback-preserving terminal chat, durable SQLite
sessions and event history, ChatGPT subscription OAuth, OpenAI-compatible
providers, permission-bound tools, transactional file changes with undo/redo,
session compaction, MCP, LSP, formatters, and bounded web fetching.

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

## Quick Start

### ChatGPT OAuth

Log in with a ChatGPT subscription, then select one of the models printed by
`parrot models`:

```sh
parrot auth login openai
# On a machine without a usable browser:
parrot auth login openai --no-browser

parrot models
parrot chat --model chatgpt/gpt-5.6-sol
```

OAuth credentials are stored in Parrot's private data directory. They are sent
only to the compiled OpenAI authentication and ChatGPT endpoints.
`parrot usage` (or `/usage` in chat) shows the remaining percentage and reset
time for the subscription's available rate-limit windows. This relies on an
upstream ChatGPT endpoint and requires a stored ChatGPT OAuth credential.

### Compatible Endpoint

Create `~/.config/parrot/parrot.jsonc`. Configuration stores the name of an
environment variable, never the API key itself:

```jsonc
{
  "model": "local/code-model",
  "providers": {
    "local": {
      "type": "compatible",
      "protocol": "responses",
      "base_url": "https://models.example.test/v1",
      "api_key_env": "LOCAL_MODEL_API_KEY",
      "models": {
        "code-model": {
          "name": "Code Model",
          "context": 128000,
          "max_tokens": 16384,
          "tools": true
        }
      }
    }
  }
}
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
parrot chat [PROMPT] [--continue | --session ID] [--model PROVIDER/MODEL]
            [--mode ID] [--thinking]
parrot run [PROMPT] [--continue | --session ID] [--model PROVIDER/MODEL]
           [--mode ID] [--thinking] [--format text|jsonl]
           [--permission deny|ask] [--interactive-prompts]
parrot models [--format lines|json]
parrot usage [--format lines|json]
parrot modes
parrot agents  # task subagents, including the built-in read-only reviewer
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
stdin is always prompt data. Reviewed `edit` and `apply_patch` operations inside
the current workspace are allowed by default. Use `run --permission deny` to
disable mutations or `run --permission ask --interactive-prompts` to prompt;
`--interactive-prompts` explicitly opens `/dev/tty` for permission and question
replies. `serve` refuses non-loopback addresses because the HTTP API has no
authentication layer.

On a real terminal, chat uses a bounded inline editor without an alternate
screen. User messages start with `$`, assistant messages start with `-`, and a
dim rule separates every committed message and forms the input area's top
border. The editor remains active while the agent works: Enter queues a
follow-up, safe slash commands run immediately, and a spinner marks the busy
prompt. Human-facing outcomes pair icons with text, such as `✓ Done`, `✗ Failed`,
and `■ Interrupted`. Complete rows of a streaming assistant response are written directly
to normal terminal scrollback while only its unfinished row remains live. Chat can start
without a configured model. Submitting a normal prompt with no model opens a
picker and preserves the draft until a model is selected. Explicit non-TTY
`parrot chat` retains the same role markers in a deterministic, ANSI-free line
REPL. `/status` shows the active agent, model, session, and project.

Interactive chat has an equivalent for every top-level CLI action: `/help`,
`/version`, `/run PROMPT`, `/chat`, `/models`, `/usage`, `/modes`, `/agents`,
`/session`, `/auth`, and `/serve`. It also supports `/model`, `/effort`,
`/mode`, `/sessions`, `/resume`, `/new`, `/clear`, `/compact`, `/connect`, `/thinking`,
`/undo`, `/redo`, `/status`, custom commands, and `/exit`.

The management namespaces mirror their CLI forms: `/session list`, `/session
show ID`, `/session compact [ID]`, `/session delete ID`, `/auth list`, `/auth
login PROVIDER [--no-browser]`, and `/auth logout PROVIDER`. Compatible-provider
slash login reads `PARROT_API_KEY`; secrets are never accepted in slash-command
text. `/serve` starts the existing local runtime in the background and supports
`/serve status` and `/serve stop`. Credential changes take effect in a new chat.
Use `/effort` to pick from the active model's reasoning levels, or pass one
directly, for example `/effort high`.

Interactive chat remembers the active conversation by canonical working
directory. On startup, Parrot reopens the previous session when its recorded
host and PID are no longer running. A second live Parrot in the same directory
gets a separate session. `/clear` (and its `/new` alias) immediately opens a
fresh durable session without deleting the previous conversation.

Keybindings: Enter submits, Ctrl-J inserts a newline, arrows edit/navigate,
Ctrl-A/Ctrl-E move to the beginning/end of the current line,
history and choices, Tab completes slash commands, Shift-Tab or Ctrl-X switches modes,
Escape cancels a picker or halts an active turn, idle Ctrl-C clears the current
edit, and Ctrl-D on an empty prompt exits. During a turn, the first Ctrl-C
requests cancellation and a second exits with status 130. Unbound control keys
and malformed keyboard input are ignored rather than terminating the chat.

## Data Paths

Parrot follows XDG variables on both macOS and Linux:

| Purpose | Default path |
| --- | --- |
| Configuration, skills, commands | `~/.config/parrot/` |
| Credentials | `~/.local/share/parrot/credentials.json` |
| SQLite state | `~/.local/state/parrot/parrot.db` |
| Process diagnostics | `~/.local/state/parrot/diagnostics/` |
| Managed tool output | `~/.cache/parrot/outputs/` |

`XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, and `XDG_CACHE_HOME`
replace their corresponding parent directories. Application directories are
created with mode `0700`; credential, database, and diagnostic files use mode
`0600`.

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
servers, language servers, formatters, and fetched web content are untrusted.
Parrot bounds network bodies and streams, rejects cross-origin authenticated
provider redirects, pins validated DNS answers for each web-fetch host, strips
terminal controls, and binds approvals to canonical inputs, resources, and
operation hashes. File mutations revalidate canonical paths and preimage hashes,
stage writes in destination directories, and journal undo/redo state.

Permission to run a shell, MCP server, LSP server, or formatter is permission to
run local code. Parrot is not an OS sandbox, cannot protect against another
same-user process racing filesystem operations, and cannot guarantee exactly-once
remote provider execution after an uncertain crash. Multi-file filesystem
rollback is best effort across process or machine failure; the SQLite event and
projection transaction remains atomic.

Project-scoped configuration cannot introduce provider endpoints or credential
sources, MCP servers, LSP or formatter executables, or private web access. These
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
- LSP and formatter commands must be absolute executable paths.
- Web fetch accepts bounded textual content only and blocks private destinations
  unless explicitly enabled.

## Development

Testing commands, race and fuzz gates, cross-build checks, and the local Nix
availability caveat are documented in [docs/testing.md](docs/testing.md).
