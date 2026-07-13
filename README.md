# Parrot Coder

Parrot Coder is a local-first coding agent written in Go. It keeps the agent
runtime reviewable, uses a normal append-only terminal interface, and supports
ChatGPT subscription login and OpenAI-compatible endpoints.

The architectural contracts are in [`docs/`](docs/). Parrot uses ordinary
terminal scrollback and does not enter an alternate screen or redraw output.

## Configuration

Create `~/.config/parrot/parrot.jsonc` and select a model explicitly as
`provider/model`. Compatible providers can use the Responses or Chat
Completions protocol. API keys are read from the configured environment
variable first, then from Parrot's private credential store.

```jsonc
{
  "model": "local/code-model",
  "providers": {
    "local": {
      "type": "compatible",
      "protocol": "responses",
      "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LOCAL_API_KEY",
      "allow_insecure_localhost": true,
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

Optional local services are configured by name. Executable paths must be
absolute; MCP HTTP endpoints require HTTPS unless loopback HTTP is explicitly
enabled.

```jsonc
{
  "mcp": {
    "docs": {
      "transport": "http",
      "url": "http://127.0.0.1:9000/mcp",
      "enabled": true,
      "allow_insecure_localhost": true,
      "startup_timeout_ms": 15000,
      "call_timeout_ms": 30000
    }
  },
  "lsp": {
    "go": {
      "command": "/absolute/path/to/gopls",
      "extensions": [".go"],
      "languages": {".go": "go"}
    }
  },
  "formatters": {
    "gofmt": {
      "extensions": [".go"],
      "command": ["/absolute/path/to/gofmt"],
      "mode": "stdin"
    }
  },
  "web_fetch": {"allow_private": false}
}
```

Skills are discovered from `skills/**/SKILL.md` under the global config
directory and `.parrot/skills/**/SKILL.md` in project scopes. Custom commands
are discovered similarly from `commands/**/*.md` and
`.parrot/commands/**/*.md`; they appear in chat `/help` and run as ordinary
typed prompts.

For a compatible provider, store a key without placing it in shell history:

```sh
printf '%s' "$PROVIDER_API_KEY" | parrot auth login local --api-key-stdin
```

Parrot never accepts API keys as command-line arguments. Without
`--api-key-stdin`, set `PARROT_API_KEY` for the login command. ChatGPT
subscription access uses OpenAI OAuth:

```sh
parrot auth login openai
parrot auth login openai --no-browser  # device authorization
parrot auth list
```

## Usage

`parrot` starts chat only when stdin is a terminal. Otherwise it prints help.

```sh
parrot chat
parrot chat "Inspect this project" --model local/code-model --agent explore
parrot run "Fix the failing tests"
git diff | parrot run "Review this diff" --format text
parrot run "Run the checks" --permission ask --interactive-prompts
parrot run "Summarize" --format jsonl

parrot models --format lines
parrot models --format json
parrot agents
parrot session list
parrot session show SESSION_ID
parrot session delete SESSION_ID
parrot serve --host 127.0.0.1 --port 4096
```

Noninteractive `run` denies mutating tool permissions by default. Piped stdin
is always prompt data and never answers a permission or model question.
`--interactive-prompts` explicitly reads those replies from `/dev/tty`.
Unauthenticated `serve` is restricted to loopback addresses.

Chat reads input only while the session is idle. It supports `/help`, `/model`,
`/agent`, `/new`, `/resume`, `/connect`, `/thinking`, `/undo`, `/redo`, and
`/exit`. During an active turn, the first Ctrl-C requests cancellation and
returns to the prompt; a second Ctrl-C exits with status 130. Ctrl-D exits from
an idle prompt.

## Development

The canonical development environment is the Nix flake:

```sh
nix develop
```

Run the local quality gates:

```sh
nix flake check
```

The equivalent direct Go checks require Go 1.25 or newer:

```sh
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
```

Build the binary:

```sh
nix build
# or, inside `nix develop`:
go build -o bin/parrot ./cmd/parrot
```
