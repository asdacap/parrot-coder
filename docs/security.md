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
- Structured file tools remain confined by canonical workspace capabilities.

## Permissions

Permissions are `allow`, `ask`, or `deny` decisions over a canonical operation.
An approval is bound to an operation hash containing tool input, resolved
resources, and review data. Changed arguments, paths, commands, or file hashes
invalidate the approval.

Canonical reads and searches, bounded `web_fetch` GET/HEAD requests, and
reviewed `edit`/`apply_patch` mutations inside the current workspace are allowed
by the default workspace policy. An explicit permission mode overrides the web
fetch and mutation defaults. Workspace containment, SSRF protections, preimage
revalidation, snapshots, and undo/redo still apply.

Permission dialogs show only the tool's human-readable description, flattened
to one line. They do not show policy metadata, resource records, canonical
input, or structured review JSON. Those values remain part of the verified
operation hash.

Hard denies cannot be overridden by remembered grants. A reply may allow once
or explicitly remember an in-memory process, session, or workspace grant; grants
are not persisted across process restarts. Questions and security permissions
use separate APIs and presentation.

The explicit `enable yolo` reply disables all subsequent permission policy
checks, including hard denies, for that session. It also allows permission
requests already pending for the session. YOLO mode is held only in memory and
ends when the session runtime or Parrot process exits.

## Processes

Shell permission allows arbitrary process execution inside a mandatory platform
sandbox. On Linux, Parrot uses Bubblewrap with a read-only host filesystem, a
writable workspace and Git metadata, read-only existing Parrot metadata (`.parrot`
and project `parrot.jsonc` along the configuration path), a private `/tmp`, user
and PID namespaces, and no capabilities. Linked-worktree common Git directories
outside the workspace are writable. On macOS it applies equivalent filesystem
write restrictions through the system `/usr/bin/sandbox-exec` and grants a
writable `/tmp`. Both backends retain host network access. Parrot fails shell
execution when the platform sandbox is unavailable; permission approval does
not bypass the sandbox.

The process runner also:

- does not inherit interactive stdin;
- creates a process group;
- terminates descendants on cancellation;
- bounds duration and output;
- constructs the environment deliberately;
- separates nonzero exit status from infrastructure failure.

Shell subprocesses may read host files that are readable by the invoking user,
but can write only the workspace, its linked-worktree Git metadata, and private
temporary area. Parrot metadata inside the workspace is read-only to shell
commands; use reviewed structured tools for intended changes. Network
destinations are not restricted. Configured formatter, LSP, and MCP processes
are trusted local services and do not use the agent shell sandbox. Command
parsing is never represented as a security boundary.

## Changes and Recovery

Edits and patches are planned before permission. The approved operation includes
the exact diff and preimage hashes. Multi-file changes are staged and runtime
failures trigger reverse rollback. Filesystem state is not transactionally
atomic across a process or machine crash.

Undo and redo refuse to overwrite divergent current state. Snapshot blobs are
private, bounded, and retained according to policy.

Tool calls are durable before side effects. Cancellation and restart repair all
nonterminal local tool calls to an explicit terminal failure. Uncertain provider
requests are not retried automatically.

## Resource Limits

Request bodies, tool-input JSON depth, SSE events, tool arguments, file reads,
search results, process output, managed output, concurrent tools, subagents, MCP
calls, and provider streams have explicit bounds. Slow live subscribers are
disconnected rather than allowed to grow an unbounded queue.
