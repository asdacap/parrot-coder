# Architecture

Parrot Coder is a local-first coding agent implemented as a single Go binary.
It follows OpenCode's service boundaries without preserving OpenCode's wire or
configuration formats.

## Principles

1. A prompt is durable before execution is requested.
2. At most one foreground drain owns a session in one process.
3. A provider turn is an explicit, cancellable boundary.
4. System context is immutable within a context epoch.
5. A tool call is durable before its side effects begin.
6. Every local tool call settles before the next provider turn.
7. Permissions authorize a canonical operation, not only a tool name.
8. Authorization and operating-system containment are separate concerns.
9. Durable events and their query projections commit atomically.
10. Live token deltas are disposable; final message state is durable.
11. The local CLI and remote clients use the same HTTP contract.
12. Local mode invokes the HTTP handler through an in-process streaming
    transport and does not bind a port.

## Components

```text
CLI / API client
       |
       v
HTTP and SSE contract
       |
       v
Application services
  |       |       |
sessions tools providers
  |       |       |
  +-------+-------+
          |
      SQLite/events
```

## Storage Layout

State is divided so that no file is written by two machines. A home directory
may be shared over a network filesystem, and a shared filesystem cannot be
assumed to provide working locks: a mount may grant every advisory lock locally
and tell no other host, so two machines both believe they hold an exclusive lock
and both write. SQLite corrupts silently under that, as would any embedded
database, because they all depend on the same locking primitive. The division is
therefore structural, not lock-based.

```text
<state>/sessions/<session_id>/session.db   one database per session
<state>/sessions/<session_id>/meta.json    published index entry
<state>/owners/<hash>/v<N>.json            interactive owner, per host
```

Three rules keep it correct:

1. **A session database is written by one machine.** It holds every table
   belonging to that session, so its foreign keys stay inside one file and
   SQLite still enforces them. It uses `journal_mode=TRUNCATE`: WAL coordinates
   through a memory-mapped `-shm` file, and two hosts mapping one file get
   incoherent private views rather than shared state. No `-shm` or `-wal` file
   may ever appear under the state directory.
2. **Listing reads `meta.json`, never another host's database.** Entries are
   published by rename, which a reader cannot observe half-written. The database
   remains the source of truth; the entry is a projection.
3. **Owner records are per host.** A working directory is a host-local name, so
   a record from another host describes a directory this host cannot see. It is
   never read to decide a claim and never written. Claims use `link()` onto a
   version-named target, which reports `EEXIST` instead of overwriting: rename
   would silently discard a competing claim, and no lock manager is involved.
Anything abandoned by a dead process is repaired per session, when that session
is opened. Repair must not range across sessions: a process cannot tell whether
work in another machine's session is abandoned or in flight.

The application owns storage, providers, the event broker, tool registrations,
and session execution. The HTTP listener is optional and has a separate
lifecycle. Request handlers must not construct long-lived dependencies.

## Session Runtime

Input has two delivery modes:

- `steer`: promote at the next safe provider-turn boundary.
- `queue`: promote one item when the current continuation would otherwise stop.

A session drain is process-local coordination, not a durable entity. Recovery
uses admitted prompts, projected messages, context epochs, and terminal tool
states. The runtime must not claim exactly-once provider execution after an
uncertain process failure.

Interactive terminal ownership is stored separately from conversation data.
It binds a canonical working directory and host-key/PID owner to a session.
Startup atomically reclaims an abandoned binding, while a live binding causes
a second session to be created. `/clear` moves the current process binding to
a fresh durable session; it does not delete the old one. This ownership only
controls terminal session selection and does not replace drain coordination.

A safe provider-turn boundary performs these operations in order:

1. Initialize or reconcile the context epoch.
2. Promote eligible input.
3. Resolve the selected agent and model.
4. Load active history.
5. Materialize an immutable tool registry snapshot.
6. Compact history if required.
7. Invoke the provider.

Tool calls are recorded before execution. Tools may execute concurrently, but
event publication is serialized and all tools settle before continuation.

## Sessions and Tasks

A session is a user session; it has an id and exactly one main task. A task is
a unit of work within a session: the main task runs the session's own turns, and
a task starts other tasks — agent tasks through `agent_spawn` and shell tasks
through backgrounded `exec_command` processes. A yielded shell task directly
steers a completion message into its owning session when its process exits;
`monitor` is reserved for observing child-agent tasks. A task may have a parent
task, so tasks form a tree rooted at the session's main task. The main task of a
subagent child session is the subagent task itself.

Tasks emit a flat lifecycle on their session's event stream: `task.start`,
`task.working`, `task.idle`, and `task.finished`. Task events are never
nested inside a parent task's event. Every event instead carries the
`task_id` of the task which produced it and the `session_id` of its stream,
and `task.start` carries the `parent_task_id`. A child session's events are
republished on its parent's stream as-is, keeping their own type, data, and
task attribution, so a client needs one subscription regardless of subagent
recursion.

Clients own the task tree. The server emits only flat events; the client
tracks which task is a child of which from `parent_task_id`, and an event
referencing a task the client has never seen produces an unknown-task error.

## Context Epochs

A context epoch stores the exact baseline text sent to the provider, typed
source snapshots, and a history cutoff. Context sources are sampled only at a
safe provider-turn boundary. Changes become durable chronological system
messages. Completed compaction starts a new epoch.

Initial context sources are the merged configuration's base agent prompt, date,
platform, working directory, project metadata, `AGENTS.md` files, available
skills, and tool guidance.

## Extension Boundaries

Provider protocols, secret storage, tools, MCP transports, and
formatters are replaceable I/O boundaries. Internal domain behavior should use
concrete types until an interface is required by one of those boundaries.

Parrot does not execute JavaScript or TypeScript plugins. External extension is
provided by MCP, configured processes, skills, commands, and compile-time Go
tool registration.

## Initial Platform Scope

The first stable release supports macOS and Linux. Windows-specific paths,
credential storage, process trees, and terminal behavior remain explicit future
work rather than partially supported behavior.
