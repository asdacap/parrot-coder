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
- A working directory is not a sandbox.

## Permissions

Permissions are `allow`, `ask`, or `deny` decisions over a canonical operation.
An approval is bound to an operation hash containing tool input, resolved
resources, and review data. Changed arguments, paths, commands, or file hashes
invalidate the approval.

Permission dialogs do not dump canonical input or structured review JSON. Each
tool decodes and describes its own parameters for the human-facing prompt;
canonical input and review data remain part of the verified operation hash.

Hard denies cannot be overridden by remembered grants. A reply may allow once
or explicitly remember an in-memory process, session, or workspace grant; grants
are not persisted across process restarts. Questions and security permissions
use separate APIs and presentation.

The explicit `enable yolo` reply disables all subsequent permission policy
checks, including hard denies, for that session. It also allows permission
requests already pending for the session. YOLO mode is held only in memory and
ends when the session runtime or Parrot process exits.

## Processes

Shell permission means arbitrary process execution. The process runner:

- does not inherit interactive stdin;
- creates a process group;
- terminates descendants on cancellation;
- bounds duration and output;
- constructs the environment deliberately;
- separates nonzero exit status from infrastructure failure.

Platform sandboxing may be added, but command parsing is never represented as a
security boundary.

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
