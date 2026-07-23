# Changelog

All notable changes to Parrot Coder will be documented in this file.

## Unreleased

### Changed

- Removed the permission policy engine and the canonical-operation model. The
  OS sandbox is now the only enforcement boundary, and the permission broker
  prompts only for operations it cannot contain: `unrestricted_shell`,
  `exec_command` with `disable_sandbox`, `request_write_permission`, and
  `set_config`. Reads, searches, bounded web fetches, `edit`/`apply_patch`,
  sandboxed shell execution, and **MCP tool calls** now run without a prompt;
  MCP servers are local processes outside the sandbox, so this widens what runs
  unattended.
- Removed `run --permission deny|ask`. Non-interactive runs still deny anything
  that would prompt.
- Removed remembered permission grants and YOLO mode. Every reply settles only
  the request which raised it, so approvals are no longer bound to an operation
  hash. Breaking `v1` API change: `Permission` drops `resources`,
  `operation_hash`, and `reason`; `PermissionChoice` and `PermissionReply` drop
  `scope`.
- Breaking tool contract: `edit` now replaces the entire file content guarded by
  `expected_sha256` instead of exact-text substitution; the `old` and
  `replace_all` parameters are removed. File `read` results end with a
  whole-file `sha256:` line suitable for `edit`'s `expected_sha256`, and
  successful `edit` results end with the after-edit `sha256:` line. Directory
  listings do not include a hash, and `apply_patch` results are unchanged.
- Removed filesystem journaling and the public undo/redo commands and API.
- Interactive assistant output now renders a terminal-safe Markdown subset and
  syntax-highlights recognized fenced code on color-capable TTYs, with bounded
  plain-text fallback for large, unknown-language, no-color, and non-TTY code.
- User and assistant messages now use distinct foreground and background colors
  instead of leading rules in interactive chat.
- Reviewed `edit` and `apply_patch` operations inside the current workspace and
  sandboxed shell commands run without a permission prompt.
- Added an `unrestricted_shell` tool that requires permission and executes with
  the invoking user's local authority without the OS sandbox.
- Sandboxed shell commands may write Git metadata, including the external
  common Git directory used by a linked worktree, so Git commits work from
  worktree-based workspaces.
- Sandboxed shell commands may use readable directories outside the workspace
  as their working directory; those directories remain read-only unless write
  access is explicitly granted.
- The bounded `read` and `grep` tools now accept explicit absolute paths outside
  the workspace; relative paths remain workspace-confined.

### Added

- The `openrouter` provider now accepts `provider_preferences` in `parrot.yaml`,
  forwarded verbatim as the top-level `provider` object of each request body to
  steer OpenRouter routing, fallback, sorting, and data-collection behavior.
  The field is OpenRouter-only; other providers never receive it.
- Startup output now lists every loaded `AGENTS.md` file, or explicitly reports
  when none were loaded.
- Initial local-first Go CLI with append-only interactive chat and
  noninteractive text/JSONL execution.
- ChatGPT OAuth and explicitly configured OpenAI-compatible Responses and Chat
  Completions providers.
- Durable SQLite projects, sessions, event projections, context epochs, tool
  state, compaction, recovery, and idempotent prompt admission.
- Canonical permission binding, bounded read/search/process tools,
  transactional edits and strict patches.
- Versioned HTTP/JSON/SSE API with in-process transport and loopback-only server.
- Skills, custom commands, subagents, MCP stdio/HTTP, LSP, formatter proposals,
  managed output, and SSRF-resistant web fetch.
- Nix development/build checks, cross-platform CI, fuzz targets, and tagged
  release automation for macOS and Linux amd64/arm64.

### Fixed

- Structured provider overload failures now retry at the provider level up to
  five times. Every retry emits a live status event and both terminal UIs show
  an immediate, explicit provider-error and retry-progress message.
- Provider streams no longer fail with "sse: read: context deadline exceeded"
  after ten minutes: the whole-request HTTP timeout no longer applies to
  streaming bodies, matching the documented header-timeout-only behavior.
  Non-streaming provider calls keep the ten-minute ceiling.

### Security

- Bounded provider streams now fail explicitly on overflow, streamed credential
  echoes are redacted, malformed terminal events fail closed, and LSP frame
  headers are aggregate-bounded with duplicate length rejection.
- Project-scoped configuration can no longer introduce startup network/process
  capabilities or redirect provider credential environment variables.
