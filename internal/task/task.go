// Package task defines the public model shared by managed shell and agent work.
package task

import "time"

type Kind string

const (
	KindMain  Kind = "main"
	KindAgent Kind = "agent"
	KindShell Kind = "shell"
)

// MainTaskID identifies the main task every session starts with. It is unique
// within one session's event stream; subtasks receive generated identifiers.
const MainTaskID = "task_main"

// Lifecycle statuses emitted as flat task events on a session's event stream.
const (
	EventStart    = "task.start"
	EventWorking  = "task.working"
	EventIdle     = "task.idle"
	EventFinished = "task.finished"
)

type Active struct {
	ID        string    `json:"task_id"`
	Kind      Kind      `json:"kind"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Turn      int       `json:"turn,omitempty"`
	Depth     int       `json:"depth,omitempty"`
}
