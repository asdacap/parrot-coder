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
data: {"id":"evt_...","type":"message.part.delta","session_id":"ses_...","sequence":42,"data":{}}

```

The server sends comment heartbeats, flushes every event, and uses bounded
subscriber queues. A slow subscriber is disconnected instead of blocking the
session runner. Clients recover authoritative state through message queries;
token deltas are not authoritative.

The event manifest is the source of truth for event names, payload codecs,
OpenAPI schemas, and client dispatch.

## Task lifecycle

Sessions start child-agent sessions via `agent_spawn` and shell processes via
backgrounded `exec_command` calls. Agents are identified by `session_id`; shell
lifecycle and process tools use `process_id`.

Lifecycle events — `task.start`, `task.working`, `task.idle`, and
`task.finished` — are flat: they are never nested inside a parent's event. Each
event identifies its producer by `session_id`. Agent `task.start` events carry
`parent_session_id`, allowing clients to rebuild the agent-session hierarchy.
Shell lifecycle events carry `process_id` in addition to the owning
`session_id`. Child-session events are also published on the parent's stream,
so clients subscribe to one session while retaining the original producer
attribution. Events referencing an unknown `session_id` indicate a client-side
tracking gap and are reported as unknown-session errors.

## Local Transport

Local CLI commands use the same API client and handler through a streaming
in-process `http.RoundTripper`. The transport must support `http.Flusher`, return
after headers are committed, propagate request cancellation, and close handler
goroutines when response bodies are abandoned.

Only `parrot serve` binds a TCP listener.
