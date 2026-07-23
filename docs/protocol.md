# HTTP and Event Protocol

The public protocol is versioned under `/api/v1`. Prompt submission is
asynchronous: successful admission returns `202 Accepted` after durable storage,
not after model completion.

## Initial Routes

```text
GET    /api/v1/health
GET    /api/v1/runtime
GET    /api/v1/sessions
POST   /api/v1/sessions
POST   /api/v1/interactive-sessions/claim
GET    /api/v1/sessions/{id}
DELETE /api/v1/sessions/{id}
GET    /api/v1/sessions/{id}/messages
POST   /api/v1/sessions/{id}/prompts
POST   /api/v1/sessions/{id}/interrupt
GET    /api/v1/sessions/{id}/events
GET    /api/v1/sessions/{id}/permissions
POST   /api/v1/sessions/{id}/permissions/{request}/reply
POST   /api/v1/sessions/{id}/questions/{request}/reply
GET    /api/v1/models
GET    /api/v1/agents
GET    /openapi.json
```

Resources use opaque string IDs. List operations use opaque cursors rather than
database offsets.

## Errors

Errors use `application/problem+json` and contain a stable `code`:

```json
{
  "type": "https://parrot.invalid/problems/session-not-found",
  "title": "Session not found",
  "status": 404,
  "detail": "The requested session does not exist.",
  "code": "session_not_found",
  "request_id": "req_..."
}
```

Unexpected failures receive an opaque error reference. Responses never expose
stack traces, credentials, or raw provider bodies.

## Server-Sent Events

Session events are streamed from:

```text
GET /api/v1/sessions/{id}/events
```

Each event carries its ID in both the SSE field and JSON envelope:

```text
id: evt_...
event: message.part.delta
data: {"id":"evt_...","type":"message.part.delta","sequence":42,"data":{}}

```

The server sends comment heartbeats, flushes every event, and uses bounded
subscriber queues. A slow subscriber is disconnected instead of blocking the
session runner. Clients recover authoritative state through message queries;
token deltas are not authoritative.

The event manifest is the source of truth for event names, payload codecs,
OpenAPI schemas, and client dispatch.

## Tasks

Every session has a main task, and tasks start other tasks: agent tasks via
`agent_spawn` and shell tasks via backgrounded shell processes. A child session
ID is the canonical ID of its agent task; shell tasks use their process IDs.
Task lifecycle events — `task.start`, `task.working`, `task.idle`,
`task.finished` — are flat: they are never nested inside a parent task's event.
Each event envelope carries the `task_id` of the task which produced it
alongside the `session_id`, and `task.start` carries the `parent_task_id`. For
all child-session events, `TaskID` (encoded as `task_id`) is the child session
ID. These events are also published on the parent's stream, so clients subscribe
to one session and rebuild the task tree themselves from `parent_task_id`.
Events referencing an unknown `task_id` indicate a client-side tracking gap
and are reported as unknown-task errors.

## Local Transport

Local CLI commands use the same API client and handler through a streaming
in-process `http.RoundTripper`. The transport must support `http.Flusher`, return
after headers are committed, propagate request cancellation, and close handler
goroutines when response bodies are abandoned.

Only `parrot serve` binds a TCP listener.
