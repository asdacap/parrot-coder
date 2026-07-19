# Configuration

Parrot reads JSON with comments and trailing commas from files named
`parrot.jsonc`. Duplicate keys and unknown typed fields are rejected. Maps merge
recursively; arrays and scalar values replace lower-precedence values.

## Discovery And Precedence

Files are loaded from lowest to highest precedence:

1. `$XDG_CONFIG_HOME/parrot/parrot.jsonc`, defaulting to
   `~/.config/parrot/parrot.jsonc`.
2. `parrot.jsonc` in the project root.
3. `.parrot/parrot.jsonc` in the project root.
4. The same two project files in each directory from the project root to the
   current working directory.

Within one directory, `.parrot/parrot.jsonc` wins. The project root is the Git
worktree root when available, otherwise the current directory. Discovery never
walks above that root.

Project files are untrusted repository content. They may select `model` and
override provider model metadata, but they cannot define provider endpoints or
credential sources, MCP servers, LSP commands, formatters, or private web-fetch
access. Those capability-bearing fields are accepted only from the global
config file. This prevents opening a repository from starting local processes,
making startup network calls, or redirecting an environment credential.

## Complete Typed Schema

All fields are optional at the document root, but coding commands require a
selected model. Durations are integer milliseconds.

```jsonc
{
  "model": "provider/model",
  "providers": {
    "provider": {
      "type": "compatible", // compatible or openai-compatible
      "protocol": "responses", // responses or chat-completions
      "base_url": "https://api.example.test/v1",
      "api_key_env": "PROVIDER_API_KEY",
      "headers": {"X-Tenant": "non-secret-value"},
      "allow_insecure_localhost": false,
      "header_timeout_ms": 10000,
      "models": {
        "model": {
          "name": "Display Name",
          "context": 128000,
          "max_tokens": 16384,
          "tools": true,
          "reasoning": false,
          "output": ["text"]
        }
      }
    }
  },
  "mcp": {
    "server-name": {
      "transport": "stdio", // stdio or http
      "command": "/absolute/path/to/server",
      "args": ["--stdio"],
      "env": {"NAME": "value"},
      "cwd": "/absolute/working/directory",
      "url": "https://mcp.example.test/rpc",
      "headers": {"X-Tenant": "non-secret-value"},
      "enabled": true,
      "allow_insecure_localhost": false,
      "startup_timeout_ms": 15000,
      "call_timeout_ms": 30000
    }
  },
  "lsp": {
    "go": {
      "command": "/absolute/path/to/gopls",
      "args": ["serve"],
      "env": {"GOTOOLCHAIN": "local"},
      "extensions": [".go"],
      "languages": {".go": "go"},
      "timeout_ms": 15000
    }
  },
  "formatters": {
    "gofmt": {
      "extensions": [".go"],
      "command": ["/absolute/path/to/gofmt"],
      "mode": "stdin" // stdin or file
    }
  },
  "web_fetch": {
    "allow_private": false
  }
}
```

### Providers

The built-in provider ID is `chatgpt`; it is not configured in `providers`.
`parrot auth login openai` stores its OAuth credential under `chatgpt`.
Parrot refreshes this provider's model metadata from the ChatGPT Codex model
catalog at startup and uses bundled metadata if that refresh is unavailable.

Some provider IDs also carry built-in presets, so supplying a credential is
enough to use them and no `providers` entry is required:

| ID | Auth | Preset |
| --- | --- | --- |
| `kimi` | API key: `MOONSHOT_API_KEY` or `parrot auth login kimi --api-key-stdin` | `https://api.moonshot.ai/v1`, `chat-completions`, Kimi K2 models |
| `openai` | API key: your `api_key_env` or credential store entry | 10-second `header_timeout_ms` only; `base_url` is still required |

Presets are compiled in, not configuration, so a project-scope file cannot
redirect a preset provider's connection fields. Adding a `providers` entry for a
preset ID overrides it field by field: anything you set wins, anything you leave
out keeps the preset value. Setting `models` replaces the preset catalog
outright rather than merging into it. A preset provider with no configuration
entry and no credential is skipped silently; a provider you do configure still
requires a key.

Each configured provider ID must have a nonempty API key from `api_key_env` or
the credential store. The environment wins. `base_url` may contain a path but
not user information, query parameters, or a fragment. Parrot appends
`responses` or `chat/completions`. HTTPS is required unless the hostname is
loopback and `allow_insecure_localhost` is true. Authenticated redirects remain
on the exact scheme and authority.

`headers` cannot override `Authorization`, `Cookie`, `Host`, or
`Proxy-Authorization`. Header values are literal and are stored in plaintext in
the config file; do not use this field for credentials. `context` and
`max_tokens` are token counts used for selection and compaction budgeting.

`header_timeout_ms` limits how long Parrot waits for HTTP response headers.
For configured providers, zero disables this provider-specific deadline and
negative values are invalid. The provider ID `openai` presets 10 seconds when
the field is omitted; explicitly setting zero disables that preset.
The timer stops as soon as headers arrive and does not limit streaming the
response body. A timeout is retried with exponential backoff (2 seconds,
doubling to a maximum of 30 seconds) until the turn is interrupted. The
built-in `chatgpt` provider uses a fixed 10-second header timeout. Because the
provider may process a request even when its response headers never reach
Parrot, retries can repeat provider-side work or billing.

Provider connection and credential fields must originate in global config;
project scopes may add or override entries below `providers.ID.models` only.

### MCP

MCP entries start only when `enabled` is true. A `stdio` entry requires an
absolute executable `command`; `args`, `env`, and absolute `cwd` are optional.
It must not contain HTTP fields. An `http` entry requires `url` and must not
contain process fields. HTTP requires TLS, except explicit loopback HTTP.

Timeout zero selects the default: 15 seconds for startup and 30 seconds for a
call. Negative values are invalid. Environment and header values are literal;
there is no `${VAR}` interpolation. Unsafe loader/startup environment variables
such as `LD_PRELOAD`, `DYLD_*`, `BASH_ENV`, and `NODE_OPTIONS` are rejected.
HTTP header values are plaintext configuration, so use only non-secret values.
All MCP fields require global configuration.

### LSP

Every LSP entry requires an absolute executable `command`. The workspace is the
resolved project root. `extensions` may omit the leading dot; absent explicit
`languages` entries derive the language ID from the extension. `languages`
wins when both specify an extension. Timeout zero defaults to 15 seconds;
negative values are invalid. Unsafe process-loader environment variables are
rejected.
All LSP fields require global configuration.

### Formatters

Every formatter requires at least one extension and an argv-style `command`
whose first element is an absolute executable path. No shell parses the argv.
Mode defaults to `stdin`. In `file` mode at least one argument must contain
`{file}`; Parrot substitutes a private temporary copy and uses its output as the
reviewed proposal. The executable still runs with the user's local filesystem
authority and is not a sandbox boundary.
The formatter definition requires global configuration. A configured formatter
is still arbitrary local code with the invoking user's filesystem authority.

### Web Fetch

`allow_private` defaults to false. When false, loopback, private, link-local,
multicast, carrier-grade NAT, and selected special-use destinations are blocked.
DNS answers are validated and pinned for the request, and every redirect target
is independently checked. Enabling this option permits access to private
services and materially increases SSRF impact.
`allow_private` can be enabled only in global configuration.

## Credential Rules

- Never place API keys, OAuth tokens, passwords, or bearer headers in a project
  config file.
- `api_key_env` names an environment variable; it is not the key value.
- `parrot auth login PROVIDER --api-key-stdin` stores a compatible-provider key
  in `credentials.json` with private file permissions.
- `PARROT_API_KEY` is accepted only by the compatible-provider login command.
- API keys are never accepted as `parrot` command-line arguments, where they
  would land in shell history and in the process argument list.
- The in-chat `/auth login PROVIDER KEY` form does accept a key, because builtin
  slash commands are handled locally: the text never reaches the model or the
  session transcript, and chat input history is not persisted. The key is still
  shown on screen, so prefer `PARROT_API_KEY` or `--api-key-stdin` when others
  can see the display.
- Project configuration, MCP environment maps, and custom headers are plaintext
  and may be committed accidentally.

## Skills

Skills are discovered as `skills/**/SKILL.md` under the global config directory
and `.parrot/skills/**/SKILL.md` at each project scope. Higher-precedence skills
with the same `name` replace lower ones. Files are bounded, symlink escapes are
rejected, and the content hash is rechecked when a skill is loaded.

```markdown
---
name: review
description: Review code and report concrete findings
agent: explorer
model: local/code-model
allowed-tools:
  - read
  - grep
---
Review the requested code and report concrete findings first.
```

`name` and `description` are required. `agent`, `model`, and `allowed-tools` are
optional. Names contain only ASCII letters, digits, `_`, or `-`.

## Built-in subagents

Parrot includes the reusable child-agent profiles `explorer`, `worker`, and
`review`. Start them with `agent_spawn`, which returns a `task_id`; observe a
task with `monitor` or discover active tasks with `task_list_active`, send
follow-up input with `agent_send`, and stop an active turn with
`task_interrupt`. Task-targeting tools accept that `task_id`. Use `explorer` for
specific, well-scoped codebase questions; it is runtime-enforced read-only. Use
`worker` for implementation
and production work; it can modify files and run commands within the authorized
workspace. `explore` is accepted as an alias of `explorer`.

### Review subagent

Parrot includes a built-in `review` worker and a parent-callable `review` tool.
The tool accepts a review prompt and an optional model override, launches an
isolated child session, waits for it, and returns its final findings to the
parent agent. The worker is defect-first and runtime-enforced read-only; it has
repository inspection and LSP tools, plus a bounded read-only `git_diff` tool,
but no mutation, shell, network, or nested delegation tools. It is a
child-agent-only profile, not a selectable foreground mode.

## Commands

Commands are discovered from `commands/**/*.md` globally and
`.parrot/commands/**/*.md` at project scopes. Their slash-command name is the
relative path without `.md`, for example `commands/review/security.md` becomes
`/review/security`.

```markdown
---
description: Review a change
agent: explorer
model: local/code-model
subtask: false
---
Review $ARGUMENTS. Focus first on $1 and include @path/to/context.txt.
```

`description` is required. `agent`, `model`, and boolean `subtask` are optional.
Templates support `$ARGUMENTS`, `$1` through `$9`, `${1}` through `${9}`, and
bounded `@relative/path` file inclusion. Shell substitution is forbidden. The
command file and included files are hash-checked between discovery and use.
When `subtask: true`, the foreground agent is instructed to start the configured
child agent with `agent_spawn`, wait for it with `monitor`, and return its
output.
