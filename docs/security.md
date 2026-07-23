# Security Model

Model output, tool arguments, compatible provider responses, MCP servers,
project files, and repository configuration are untrusted inputs. Project
configuration cannot introduce endpoints, credential sources, local service
executables, or private web access; those fields require global configuration.

## Credentials

- Subscription OAuth credentials may be sent only to compiled OpenAI hosts.
- A configured compatible endpoint can never receive subscription credentials.
- API keys are referenced through the environment or secret store, not project
  configuration values.
- Secret files and directories use `0600` and `0700` permissions respectively.
- Credential updates are atomic and refresh-token rotation is persisted as one
  operation.
- Authorization headers, API keys, OAuth codes, and tokens are redacted from
  logs and errors.
- Process diagnostics do not record HTTP headers, query strings, request
  bodies, prompts, provider responses, tool inputs, tool outputs, or error
  messages. Routine errors are recorded only by concrete type and operational
  status. Go's report for an unhandled panic
  necessarily includes the panic value, so crash reports remain private state
  and should be reviewed before sharing.
- Authenticated HTTP clients reject cross-origin redirects.

ChatGPT subscription support is a standard feature but relies on endpoints and
client behavior that can change. Release qualification includes manual browser
login, device login, refresh, text generation, and tool-call canaries.

## Filesystem

- Relative paths resolve against an immutable workspace root.
- Lexical containment is followed by filesystem-aware containment.
- Existing symlink components are resolved before authorization.
- New paths validate the nearest existing parent.
- Paths and preimage hashes are revalidated immediately before mutation.
- External roots require explicit canonical capabilities.
- `read` and `grep` accept explicit absolute host paths for bounded read-only
  access; relative paths remain confined to the workspace.
- Mutating structured file tools remain confined by canonical workspace
  capabilities.

## Permissions

The operating-system sandbox is the enforcement boundary. Work it confines —
canonical reads and searches, bounded `web_fetch` GET/HEAD requests, reviewed
`edit`/`apply_patch` mutations, MCP tool calls, and sandboxed `shell` and
`exec_command` execution — runs without a prompt.

A prompt is raised only for operations the sandbox cannot contain:

- `unrestricted_shell` and `exec_command` with `sandbox_permissions:
  "disable_sandbox"`, which execute with the invoking user's local authority;
- `request_write_permission`, which adds a write path to the sandbox;
- `set_config`, which mutates persistent configuration.

Permissions are `allow` or `deny` decisions. Nothing is remembered: each reply
settles exactly the request which raised it, so an identical later call prompts
again. There are no scoped grants and no YOLO mode. In non-interactive mode a
request that would prompt is denied.

Permission dialogs show only the tool's human-readable description, flattened
to one line. They do not show resource records, canonical input, or structured
review JSON.

The `request_write_permission` tool uses a specialized Grant/Reject dialog.
Granting it adds the exact existing file or directory to the sandbox's writable
paths for shell calls in that session only. Reject with reason denies the tool
call and returns the supplied text to the agent as its tool error. These grants
are held in memory, are not shared with other sessions, and disappear when
Parrot restarts. Questions and security permissions use separate APIs and
presentation.

## Processes

The default `shell` tool allows arbitrary process execution inside a mandatory
platform sandbox. On Linux, Parrot uses Bubblewrap with a read-only host
filesystem, a writable workspace and Git metadata, read-only existing Parrot
metadata (`.parrot` and project `parrot.yaml` along the configuration path), a
session-private `/tmp` shared across that session's shell commands, user and
PID namespaces, and no capabilities. Linked-worktree
common Git directories outside the workspace are writable. On macOS it applies
equivalent filesystem
write restrictions through the system `/usr/bin/sandbox-exec` and grants a
session-private writable `TMPDIR`. Both backends retain host network access.
Parrot fails shell execution when the platform sandbox is unavailable;
permission approval does not bypass that tool's sandbox.

The separate `unrestricted_shell` tool always requires approval. Once approved,
it executes directly with the invoking user's
local filesystem and process authority. It retains process timeouts, bounded
output, cancellation, and deliberate environment construction, but it has no
operating-system sandbox or workspace-bound working-directory restriction.

The process runner also:

- does not inherit interactive stdin;
- creates a process group;
- terminates descendants on cancellation;
- bounds duration and output;
- constructs the environment deliberately;
- separates nonzero exit status from infrastructure failure.

Sandboxed shell subprocesses may use any existing directory as their working
directory and read host files that are readable by the invoking user, but can
write only the workspace, its linked-worktree Git
metadata, and private temporary area. Parrot metadata inside the workspace is
read-only to shell commands; use reviewed structured tools for intended changes.
Network destinations are not restricted. Unrestricted shell, configured
formatter and MCP processes are trusted local execution and do not use the
agent shell sandbox. Command parsing is never represented as a security boundary.

Configured `sandbox_rules` in global `parrot.yaml` apply ordered filesystem
rules after the base mounts. Each rule maps a path to an action —
`allow_write`, `deny_read`, `allow_read`, or `deny_write` — and later rules
override earlier ones. Project-scope configuration cannot define sandbox rules.

## Changes and Recovery

Edits and patches are planned before they are applied, and the plan records the
exact diff and preimage hashes. Paths and preimage hashes are revalidated
immediately before mutation, so a file changed after planning fails as stale
rather than being overwritten. Multi-file changes are staged and runtime
failures trigger reverse rollback. Filesystem state is not transactionally
atomic across a process or machine crash.

Tool calls are durable before side effects. Cancellation and restart repair all
nonterminal local tool calls to an explicit terminal failure. Uncertain provider
requests are not retried automatically.

## Resource Limits

Request bodies, tool-input JSON depth, SSE events, tool arguments, file reads,
search results, process output, managed output, concurrent tools, subagents, MCP
calls, and provider streams have explicit bounds. Slow live subscribers are
disconnected rather than allowed to grow an unbounded queue.
