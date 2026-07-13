package mcp

import (
	"context"
	"encoding/json"
)

type endpoint interface {
	call(context.Context, string, any) (json.RawMessage, error)
	notify(context.Context, string, any) error
	setProtocolVersion(string)
	close(context.Context) error
	done() <-chan struct{}
	notificationChannel() <-chan Notification
	pid() int
}
