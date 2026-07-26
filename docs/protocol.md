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

## Runtime lifecycle

Sessions start child agent sessions via `agent_spawn` and retained processes
via `exec_command`. Lifecycle events are flat: they are never nested inside a
parent event, and the event namespace determines the payload type.

- User sessions emit `user_session.start`, `user_session.working`, and
  `user_session.idle`, carrying `session_id`.
- Agent sessions emit `agent_session.start`, `agent_session.working`,
  `agent_session.idle`, and `agent_session.finished`, carrying `session_id` and
  their optional `parent_session_id`, agent, name, status, and error.
- Only commands that outlive the initial `exec_command` yield become managed
  processes. They emit `process.start` and `process.finished`, carrying their
  owning `session_id`, distinct `process_id`, name, status, and error.

Child-session events are also published on the parent's stream, so clients
subscribe once while retaining original producer attribution. Clients rebuild
a presentation task tree from agent parentage and process ownership; “task” is
a client presentation concept rather than a common lifecycle wire payload.
`task.progress` remains the separate agent progress event. References to an
unknown session or process indicate a client-side tracking gap.

## Tool lifecycle

Durable tool activity uses `session.tool.pending`, `session.tool.running`,
`session.tool.success`, `session.tool.failure`, and
`session.tool.interrupted`. Each decodes as a `ToolEvent` with canonical
lower-snake-case fields: `call_id`, optional `tool_name`, `input`, `status`,
`result`, `error`, and `output_tail`. Pending events establish the tool name and
input. Later events may omit `tool_name`; clients retain the pending state and
correlate lifecycle updates by `call_id`. Presentation redaction applies to the
typed input, result, error, and output fields before they are rendered or
exported.

## Local Transport

Local CLI commands use the same API client and handler through a streaming
in-process `http.RoundTripper`. The transport must support `http.Flusher`, return
after headers are committed, propagate request cancellation, and close handler
goroutines when response bodies are abandoned.

Only `parrot serve` binds a TCP listener.
