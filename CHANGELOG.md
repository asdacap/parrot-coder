# Changelog

All notable changes to Parrot Coder will be documented in this file.

## Unreleased

### Changed

- Reviewed `edit` and `apply_patch` operations inside the current workspace are
  allowed by default; `run --permission deny|ask` remains available as an
  explicit override.

### Added

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
