package httpapi

import (
	"context"
	"errors"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
)

var (
	ErrNotFound            = errors.New("httpapi: resource not found")
	ErrConflict            = errors.New("httpapi: conflict")
	ErrInvalid             = errors.New("httpapi: invalid request")
	ErrNoUndo              = errors.New("httpapi: nothing to undo")
	ErrNoRedo              = errors.New("httpapi: nothing to redo")
	ErrIdempotencyConflict = errors.New("httpapi: idempotency conflict")
	ErrPermissionNotFound  = errors.New("httpapi: permission request not found")
	ErrQuestionNotFound    = errors.New("httpapi: question request not found")
	ErrModelRequired       = errors.New("httpapi: model selection required")
	ErrInvalidSelection    = errors.New("httpapi: invalid selection")
	ErrSessionActive       = errors.New("httpapi: session is active")
)

type EventStream struct {
	Replay  []v1.Event
	Durable <-chan v1.Event
	Live    <-chan v1.Event
	Close   func()
}

// Backend is the application boundary used by HTTP. It deliberately speaks
// only v1 DTOs so domain structs cannot accidentally become wire contracts.
type Backend interface {
	Runtime(context.Context) (v1.Runtime, error)
	ListSessions(context.Context) (v1.SessionList, error)
	CreateSession(context.Context, v1.CreateSessionRequest) (v1.Session, error)
	ClaimSession(context.Context, v1.ClaimSessionRequest) (v1.ClaimSessionResponse, error)
	GetSession(context.Context, string) (v1.Session, error)
	UpdateSessionSelection(context.Context, string, v1.UpdateSessionSelectionRequest) (v1.SessionSelection, error)
	DeleteSession(context.Context, string) error
	ListMessages(context.Context, string) (v1.MessageList, error)
	ListTodos(context.Context, string) (v1.TodoList, error)
	AdmitPrompt(context.Context, string, v1.PromptRequest) (v1.PromptAccepted, error)
	Wake(string)
	Interrupt(context.Context, string) error
	OpenEvents(context.Context, string, int64) (*EventStream, error)
	ListPermissions(context.Context, string) (v1.PermissionList, error)
	ReplyPermission(context.Context, string, string, v1.PermissionReply) error
	ListQuestions(context.Context, string) (v1.QuestionList, error)
	ReplyQuestion(context.Context, string, string, v1.QuestionReply) error
	Undo(context.Context, string) (v1.SnapshotTransaction, error)
	Redo(context.Context, string) (v1.SnapshotTransaction, error)
	ListModels(context.Context) (v1.ModelList, error)
	SubscriptionUsage(context.Context) (v1.SubscriptionUsage, error)
	ListAgents(context.Context) (v1.AgentList, error)
}
