package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

// DomainBackend maps the existing application services to the stable API.
// All fields are long-lived dependencies owned by the application.
type DomainBackend struct {
	Version            string
	Sessions           *session.Service
	Coordinator        *agent.Coordinator
	Agents             *agent.Registry
	Providers          []provider.Provider
	Permissions        *permission.Broker
	Questions          *question.Broker
	Todos              *session.TodoService
	Snapshots          *snapshot.Service
	Workspace          *workspace.Workspace
	Events             *event.Repository
	Live               *event.Broker
	EventQueue         int
	DefaultSelection   session.Selection
	ProviderResolver   agent.ProviderResolver
	CompactSessionFunc func(context.Context, string) (v1.Compaction, error)
}

func (b *DomainBackend) CompactSession(ctx context.Context, id string) (v1.Compaction, error) {
	if _, err := b.GetSession(ctx, id); err != nil {
		return v1.Compaction{}, err
	}
	if b.CompactSessionFunc == nil {
		return v1.Compaction{}, errors.New("httpapi: compaction is unavailable")
	}
	return b.CompactSessionFunc(ctx, id)
}

func (b *DomainBackend) Runtime(context.Context) (v1.Runtime, error) {
	out := v1.Runtime{Version: b.Version, Active: []v1.RuntimeSession{}}
	if b.Coordinator != nil {
		for _, item := range b.Coordinator.Active() {
			out.Active = append(out.Active, v1.RuntimeSession{SessionID: item.SessionID, Status: string(item.Status)})
		}
	}
	return out, nil
}

func (b *DomainBackend) ListSessions(ctx context.Context) (v1.SessionList, error) {
	items, err := b.Sessions.List(ctx)
	if err != nil {
		return v1.SessionList{}, err
	}
	out := v1.SessionList{Items: make([]v1.Session, len(items))}
	for i, item := range items {
		out.Items[i] = sessionDTO(item)
	}
	return out, nil
}

func (b *DomainBackend) CreateSession(ctx context.Context, request v1.CreateSessionRequest) (v1.Session, error) {
	selection := b.DefaultSelection
	if request.Agent != "" {
		selection.Agent = request.Agent
	}
	if request.Model != "" {
		providerID, modelID, qualified := strings.Cut(request.Model, "/")
		if qualified {
			selection.Provider, selection.Model = providerID, modelID
		} else {
			selection.Model = request.Model
		}
	}
	if !completeSelection(selection) {
		return v1.Session{}, ErrModelRequired
	}
	if err := b.validateSelection(selection); err != nil {
		return v1.Session{}, err
	}
	item, err := b.Sessions.CreateSelected(ctx, session.CreateParams{ProjectID: request.ProjectID, Title: request.Title}, selection)
	return sessionDTO(item), err
}

func (b *DomainBackend) GetSession(ctx context.Context, id string) (v1.Session, error) {
	item, err := b.Sessions.Get(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return v1.Session{}, ErrNotFound
	}
	return sessionDTO(item), err
}

func (b *DomainBackend) UpdateSessionSelection(ctx context.Context, id string, request v1.UpdateSessionSelectionRequest) (v1.SessionSelection, error) {
	if request.Agent == "" && request.Model == "" {
		return v1.SessionSelection{}, ErrInvalid
	}
	if b.Coordinator != nil && b.Coordinator.Status(id) != agent.StatusIdle {
		return v1.SessionSelection{}, ErrSessionActive
	}
	patch := session.SelectionPatch{
		Agent: request.Agent, FallbackAgent: b.DefaultSelection.Agent,
		FallbackProvider: b.DefaultSelection.Provider,
	}
	if request.Model != "" {
		providerID, modelID, qualified := strings.Cut(request.Model, "/")
		if qualified {
			patch.Provider, patch.Model = providerID, modelID
		} else {
			patch.Model = request.Model
		}
	}
	item, err := b.Sessions.UpdateSelection(ctx, id, patch, b.validateSelection)
	if errors.Is(err, session.ErrNotFound) {
		return v1.SessionSelection{}, ErrNotFound
	}
	if err != nil {
		if errors.Is(err, session.ErrSelectionRequired) {
			return v1.SessionSelection{}, ErrModelRequired
		}
		return v1.SessionSelection{}, err
	}
	return selectionDTO(item), nil
}

func (b *DomainBackend) DeleteSession(ctx context.Context, id string) error {
	err := b.Sessions.Delete(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func (b *DomainBackend) ListMessages(ctx context.Context, id string) (v1.MessageList, error) {
	if _, err := b.GetSession(ctx, id); err != nil {
		return v1.MessageList{}, err
	}
	items, err := b.Sessions.ListMessages(ctx, id)
	if err != nil {
		return v1.MessageList{}, err
	}
	out := v1.MessageList{Items: make([]v1.Message, len(items))}
	for i, item := range items {
		parts, usage := item.Parts, item.Usage
		if len(parts) == 0 {
			parts = json.RawMessage(`[]`)
		}
		if len(usage) == 0 {
			usage = json.RawMessage(`{}`)
		}
		out.Items[i] = v1.Message{ID: item.ID, SessionID: item.SessionID, Role: item.Role, Content: item.Content, Parts: parts, Status: item.Status, FinishReason: item.FinishReason, Error: item.Error, Usage: usage, InputID: item.InputID, Sequence: item.Sequence, CreatedAt: item.CreatedAt}
	}
	return out, nil
}

func (b *DomainBackend) ListTodos(ctx context.Context, id string) (v1.TodoList, error) {
	if _, err := b.GetSession(ctx, id); err != nil {
		return v1.TodoList{}, err
	}
	if b.Todos == nil {
		return v1.TodoList{}, errors.New("httpapi: todo service is unavailable")
	}
	items, err := b.Todos.List(ctx, id)
	if err != nil {
		return v1.TodoList{}, err
	}
	out := v1.TodoList{Items: make([]v1.Todo, len(items))}
	for i, item := range items {
		out.Items[i] = v1.Todo{ID: item.ID, Content: item.Content, Status: string(item.Status), Priority: string(item.Priority), Position: item.Position}
	}
	return out, nil
}

func (b *DomainBackend) AdmitPrompt(ctx context.Context, id string, request v1.PromptRequest) (v1.PromptAccepted, error) {
	selected, err := b.GetSession(ctx, id)
	if err != nil {
		return v1.PromptAccepted{}, err
	}
	if selected.Agent == "" || selected.Provider == "" || selected.Model == "" {
		return v1.PromptAccepted{}, ErrModelRequired
	}
	admission, err := b.Sessions.Admit(ctx, id, session.AdmitParams{MessageID: request.MessageID, Content: request.Content, Delivery: session.Delivery(request.Delivery)})
	if errors.Is(err, session.ErrInvalidDelivery) {
		return v1.PromptAccepted{}, ErrInvalid
	}
	if errors.Is(err, session.ErrIdempotencyConflict) {
		return v1.PromptAccepted{}, ErrIdempotencyConflict
	}
	if err != nil {
		return v1.PromptAccepted{}, err
	}
	return v1.PromptAccepted{InputID: admission.Input.ID, MessageID: admission.Input.MessageID, Delivery: string(admission.Input.Delivery), Status: admission.Input.Status, Created: admission.Created}, nil
}

func (b *DomainBackend) Wake(id string) {
	if b.Coordinator == nil {
		return
	}
	b.Coordinator.Wake(id)
	if b.Live != nil {
		data, _ := json.Marshal(v1.SessionStatus{Kind: "running"})
		b.Live.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: id, Data: data})
	}
}

func (b *DomainBackend) Interrupt(ctx context.Context, id string) error {
	if _, err := b.GetSession(ctx, id); err != nil {
		return err
	}
	if b.Coordinator == nil {
		return nil
	}
	if err := b.Coordinator.Interrupt(ctx, id); err != nil {
		return err
	}
	return nil
}

func (b *DomainBackend) OpenEvents(ctx context.Context, id string, after int64) (*EventStream, error) {
	if _, err := b.GetSession(ctx, id); err != nil {
		return nil, err
	}
	if b.Events == nil {
		return nil, errors.New("httpapi: event repository is unavailable")
	}
	capacity := b.EventQueue
	if capacity <= 0 {
		capacity = 64
	}
	var live <-chan v1.Event
	closeLive := func() {}
	if b.Live != nil {
		live, closeLive = b.Live.Subscribe(id, capacity)
	}
	replay, subscription, err := b.Events.ReplayAndSubscribe(ctx, id, after, capacity)
	if err != nil {
		closeLive()
		return nil, err
	}
	out := &EventStream{Replay: make([]v1.Event, len(replay)), Durable: make(chan v1.Event), Live: live}
	durable := make(chan v1.Event)
	out.Durable = durable
	stop := make(chan struct{})
	go func() {
		defer close(durable)
		for {
			select {
			case item, ok := <-subscription.Events:
				if !ok {
					return
				}
				select {
				case durable <- durableEvent(item):
				case <-stop:
					return
				}
			case <-stop:
				return
			}
		}
	}()
	for i, item := range replay {
		out.Replay[i] = durableEvent(item)
	}
	var once sync.Once
	out.Close = func() {
		once.Do(func() {
			close(stop)
			subscription.Close()
			closeLive()
		})
	}
	return out, nil
}

func (b *DomainBackend) ListPermissions(ctx context.Context, id string) (v1.PermissionList, error) {
	if _, err := b.GetSession(ctx, id); err != nil {
		return v1.PermissionList{}, err
	}
	out := v1.PermissionList{Items: []v1.Permission{}}
	if b.Permissions == nil {
		return out, nil
	}
	for _, item := range b.Permissions.Pending() {
		if item.Request.SessionID != id {
			continue
		}
		resources := make([]v1.PermissionResource, len(item.Request.Resources))
		for i, resource := range item.Request.Resources {
			resources[i] = v1.PermissionResource{Kind: resource.Kind, Identifier: resource.Identifier, Operation: resource.Operation, Attributes: resource.Attributes}
		}
		out.Items = append(out.Items, v1.Permission{ID: item.ID, ToolID: item.Request.ToolID, CanonicalInput: item.Request.CanonicalInput, Resources: resources, Review: item.Request.Review, OperationHash: item.Request.OperationHash, Reason: item.Reason})
	}
	return out, nil
}

func (b *DomainBackend) ReplyPermission(ctx context.Context, sessionID, requestID string, reply v1.PermissionReply) error {
	items, err := b.ListPermissions(ctx, sessionID)
	if err != nil {
		return err
	}
	if !containsPermission(items.Items, requestID) || b.Permissions == nil {
		return ErrPermissionNotFound
	}
	switch reply.Decision {
	case "deny":
		if reply.Scope != "" {
			return ErrInvalid
		}
		err = b.Permissions.Reject(requestID)
	case "allow":
		switch reply.Scope {
		case "":
			err = b.Permissions.ReplyOnce(requestID)
		case "process":
			err = b.Permissions.ReplyProcess(requestID)
		case "session":
			err = b.Permissions.ReplySession(requestID)
		case "workspace":
			err = b.Permissions.ReplyWorkspace(requestID)
		case "yolo":
			err = b.Permissions.EnableYolo(requestID)
		default:
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if err != nil {
		return ErrPermissionNotFound
	}
	if b.Live != nil {
		data, _ := json.Marshal(v1.PermissionResolved{RequestID: requestID, Decision: reply.Decision})
		b.Live.PublishEvent(v1.Event{Type: v1.EventPermissionReply, SessionID: sessionID, Data: data})
	}
	return nil
}

func (b *DomainBackend) ListQuestions(ctx context.Context, id string) (v1.QuestionList, error) {
	if _, err := b.GetSession(ctx, id); err != nil {
		return v1.QuestionList{}, err
	}
	out := v1.QuestionList{Items: []v1.QuestionRequest{}}
	if b.Questions == nil {
		return out, nil
	}
	for _, item := range b.Questions.Pending() {
		if item.Request.SessionID != id {
			continue
		}
		request := v1.QuestionRequest{ID: item.ID, Questions: make([]v1.Question, len(item.Request.Questions))}
		for i, question := range item.Request.Questions {
			options := make([]v1.Option, len(question.Options))
			for j, option := range question.Options {
				options[j] = v1.Option{ID: option.ID, Label: option.Label, Description: option.Description}
			}
			request.Questions[i] = v1.Question{ID: question.ID, Header: question.Header, Prompt: question.Prompt, Options: options, Multiple: question.Multiple, Custom: question.Custom}
		}
		out.Items = append(out.Items, request)
	}
	return out, nil
}

func (b *DomainBackend) ReplyQuestion(ctx context.Context, sessionID, requestID string, reply v1.QuestionReply) error {
	items, err := b.ListQuestions(ctx, sessionID)
	if err != nil {
		return err
	}
	found := false
	for _, item := range items.Items {
		found = found || item.ID == requestID
	}
	if !found || b.Questions == nil {
		return ErrQuestionNotFound
	}
	if reply.Reject {
		if len(reply.Answers) != 0 {
			return ErrInvalid
		}
		if err := b.Questions.Reject(requestID); err != nil {
			return ErrQuestionNotFound
		}
		if b.Live != nil {
			data, _ := json.Marshal(v1.QuestionResolved{RequestID: requestID, Rejected: true})
			b.Live.PublishEvent(v1.Event{Type: v1.EventQuestionReply, SessionID: sessionID, Data: data})
		}
		return nil
	}
	response := question.Response{Answers: make([]question.Answer, len(reply.Answers))}
	for i, answer := range reply.Answers {
		response.Answers[i] = question.Answer{QuestionID: answer.QuestionID, OptionIDs: answer.OptionIDs, Custom: answer.Custom}
	}
	if err := b.Questions.Reply(requestID, response); err != nil {
		return ErrInvalid
	}
	if b.Live != nil {
		data, _ := json.Marshal(v1.QuestionResolved{RequestID: requestID})
		b.Live.PublishEvent(v1.Event{Type: v1.EventQuestionReply, SessionID: sessionID, Data: data})
	}
	return nil
}

func (b *DomainBackend) Undo(ctx context.Context, id string) (v1.SnapshotTransaction, error) {
	return b.moveSnapshot(ctx, id, false)
}

func (b *DomainBackend) Redo(ctx context.Context, id string) (v1.SnapshotTransaction, error) {
	return b.moveSnapshot(ctx, id, true)
}

func (b *DomainBackend) moveSnapshot(ctx context.Context, id string, redo bool) (v1.SnapshotTransaction, error) {
	if _, err := b.GetSession(ctx, id); err != nil {
		return v1.SnapshotTransaction{}, err
	}
	if b.Snapshots == nil || b.Workspace == nil {
		return v1.SnapshotTransaction{}, errors.New("httpapi: snapshot service is unavailable")
	}
	var item snapshot.Transaction
	var err error
	if redo {
		item, err = b.Snapshots.Redo(ctx, b.Workspace, id)
	} else {
		item, err = b.Snapshots.Undo(ctx, b.Workspace, id)
	}
	if errors.Is(err, snapshot.ErrNoUndo) {
		return v1.SnapshotTransaction{}, ErrNoUndo
	}
	if errors.Is(err, snapshot.ErrNoRedo) {
		return v1.SnapshotTransaction{}, ErrNoRedo
	}
	if errors.Is(err, snapshot.ErrConflict) {
		return v1.SnapshotTransaction{}, ErrConflict
	}
	if err != nil {
		return v1.SnapshotTransaction{}, err
	}
	paths := make([]string, len(item.Entries))
	for i, entry := range item.Entries {
		paths[i] = entry.Path
	}
	return v1.SnapshotTransaction{ID: item.ID, SessionID: item.SessionID, Position: item.Position, CreatedAt: item.CreatedAt, Paths: paths}, nil
}

func (b *DomainBackend) ListModels(context.Context) (v1.ModelList, error) {
	out := v1.ModelList{Items: []v1.Model{}}
	for _, provider := range b.Providers {
		if provider == nil {
			continue
		}
		for _, model := range provider.Models() {
			out.Items = append(out.Items, v1.Model{Provider: provider.ID(), ID: model.ID, Name: model.Name, ContextWindow: model.ContextWindow, MaxOutputTokens: model.MaxOutputTokens, Tools: model.Capabilities.Tools, Reasoning: model.Capabilities.Reasoning, Output: append([]string(nil), model.Capabilities.Output...)})
		}
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Provider == out.Items[j].Provider {
			return out.Items[i].ID < out.Items[j].ID
		}
		return out.Items[i].Provider < out.Items[j].Provider
	})
	return out, nil
}

func (b *DomainBackend) ListAgents(context.Context) (v1.AgentList, error) {
	out := v1.AgentList{Items: []v1.Agent{}}
	if b.Agents == nil {
		return out, nil
	}
	for _, profile := range b.Agents.List() {
		out.Items = append(out.Items, v1.Agent{ID: profile.ID, ReadOnly: profile.ReadOnly, MaxTurns: profile.MaxTurns})
	}
	return out, nil
}

func sessionDTO(item session.Session) v1.Session {
	return v1.Session{ID: item.ID, ProjectID: item.ProjectID, Title: item.Title, Agent: item.Agent, Provider: item.Provider, Model: item.Model, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func selectionDTO(item session.Session) v1.SessionSelection {
	return v1.SessionSelection{Agent: item.Agent, Provider: item.Provider, Model: item.Model}
}

func completeSelection(selection session.Selection) bool {
	return selection.Agent != "" && selection.Provider != "" && selection.Model != ""
}

func (b *DomainBackend) validateSelection(selection session.Selection) error {
	if !completeSelection(selection) {
		return ErrModelRequired
	}
	if b.Agents == nil {
		return errors.New("httpapi: agent registry is unavailable")
	}
	if _, err := b.Agents.Get(selection.Agent); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSelection, err)
	}
	resolver := b.ProviderResolver
	if resolver == nil {
		registry, err := agent.NewProviderRegistry(b.Providers...)
		if err != nil {
			return err
		}
		resolver = registry
	}
	if _, _, err := resolver.Resolve(selection.Provider, selection.Model); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSelection, err)
	}
	return nil
}

func durableEvent(item event.Event) v1.Event {
	sequence, created := item.Sequence, item.CreatedAt
	return v1.Event{ID: item.ID, Type: item.Type, SessionID: item.SessionID, Sequence: &sequence, Data: item.Data, CreatedAt: &created}
}

func containsPermission(items []v1.Permission, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
