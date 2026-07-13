# Security Model

Model output, tool arguments, compatible provider responses, MCP servers,
project files, and repository configuration are untrusted inputs.

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

Hard denies cannot be overridden by remembered grants. Remembered grants default
to session scope. Persistent workspace grants require a separate explicit
choice. Questions and security permissions use separate APIs and presentation.

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
the exact diff and preimage hashes. Multi-file changes are staged and either
commit completely or roll back.

Undo and redo refuse to overwrite divergent current state without a new,
explicit force confirmation. Snapshot blobs are private, bounded, and retained
according to policy.

Tool calls are durable before side effects. Cancellation and restart repair all
nonterminal local tool calls to an explicit terminal failure. Uncertain provider
requests are not retried automatically.

## Resource Limits

Bound all request bodies, JSON depth, SSE events, tool arguments, file reads,
search results, process output, managed output, attachments, concurrent tools,
subagents, MCP calls, and provider streams. No client subscription or tool may
cause unbounded memory growth.
