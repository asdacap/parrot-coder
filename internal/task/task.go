// Package task defines the public model shared by managed shell and agent work.
package task

import "time"

type Kind string

const (
	KindAgent Kind = "agent"
	KindShell Kind = "shell"
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
