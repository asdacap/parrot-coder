# Changelog

All notable changes to Parrot Coder will be documented in this file.

## Unreleased

### Changed

- Interactive assistant output now renders a terminal-safe Markdown subset and
  syntax-highlights recognized fenced code on color-capable TTYs, with bounded
  plain-text fallback for large, unknown-language, no-color, and non-TTY code.
- Final assistant messages now use a dim rule instead of an empty leading gap
  in interactive chat.
- Reviewed `edit` and `apply_patch` operations inside the current workspace are
  allowed by default; `run --permission deny|ask` remains available as an
  explicit override.
- Sandboxed shell commands are now allowed without a permission prompt by the
  default workspace policy; `run --permission deny|ask` remains available as an
  explicit override.
- Added an `unrestricted_shell` tool that requires permission under the default
  policy and executes with the invoking user's local authority without the OS
  sandbox.
- Sandboxed shell commands may write Git metadata, including the external
  common Git directory used by a linked worktree, so Git commits work from
  worktree-based workspaces.

### Added

- Startup output now lists every loaded `AGENTS.md` file, or explicitly reports
  when none were loaded.
- Initial local-first Go CLI with append-only interactive chat and
  noninteractive text/JSONL execution.
- ChatGPT OAuth and explicitly configured OpenAI-compatible Responses and Chat
  Completions providers.
- Durable SQLite projects, sessions, event projections, context epochs, tool
  state, compaction, recovery, and idempotent prompt admission.
- Canonical permission binding, bounded read/search/process tools,
  transactional edits and strict patches, snapshots, undo, and redo.
- Versioned HTTP/JSON/SSE API with in-process transport and loopback-only server.
- Skills, custom commands, subagents, MCP stdio/HTTP, LSP, formatter proposals,
  managed output, and SSRF-resistant web fetch.
- Nix development/build checks, cross-platform CI, fuzz targets, and tagged
  release automation for macOS and Linux amd64/arm64.

### Security

- Bounded provider streams now fail explicitly on overflow, streamed credential
  echoes are redacted, malformed terminal events fail closed, and LSP frame
  headers are aggregate-bounded with duplicate length rejection.
- Project-scoped configuration can no longer introduce startup network/process
  capabilities or redirect provider credential environment variables.
