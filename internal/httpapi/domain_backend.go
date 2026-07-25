package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/mode"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/processidentity"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

// DomainBackend maps the existing application services to the stable API.
// All fields are long-lived dependencies owned by the application.
type DomainBackend struct {
	Version            string
	ProjectRoot        string
	Sessions           *session.Service
	AgentSessions      AgentSessionController
	Agents             *agent.Registry
	Modes              *mode.Registry
	Providers          []provider.Provider
	Permissions        *permission.Broker
	Questions          *question.Broker
	Todos              *session.TodoService
	Goals              *session.GoalService
	Events             *event.Broker
	EventQueue         int
	DefaultSelection   session.Selection
	ProviderResolver   agent.ProviderResolver
	CompactSessionFunc func(context.Context, string) (v1.Compaction, error)
	Processes          ProcessLifecycle
	Tools              tool.Snapshot
}

func (b *DomainBackend) GetGoal(ctx context.Context, id string) (v1.Goal, error) {
	goal, err := b.Goals.Get(ctx, id)
	if errors.Is(err, session.ErrGoalNotFound) || errors.Is(err, session.ErrNotFound) {
		return v1.Goal{}, ErrNotFound
	}
	return goalDTO(goal), err
}

func (b *DomainBackend) PutGoal(ctx context.Context, id string, request v1.PutGoalRequest) (v1.Goal, error) {
	if _, err := b.GetSession(ctx, id); err != nil {
		return v1.Goal{}, err
	}
	var goal session.Goal
	var err error
	_, getErr := b.Goals.Get(ctx, id)
	if errors.Is(getErr, session.ErrGoalNotFound) {
		if request.Objective == nil || request.Status != nil {
			return v1.Goal{}, ErrInvalid
		}
		goal, err = b.Goals.Create(ctx, id, *request.Objective, request.TokenBudget)
	} else if getErr != nil {
		return v1.Goal{}, getErr
	} else {
		var status *session.GoalStatus
		if request.Status != nil {
			value := session.GoalStatus(*request.Status)
			status = &value
		}
		goal, err = b.Goals.Update(ctx, id, session.GoalMutation{Objective: request.Objective, Status: status, TokenBudget: request.TokenBudget, ClearTokenBudget: request.ClearTokenBudget})
	}
	if errors.Is(err, session.ErrGoalExists) {
		return v1.Goal{}, ErrConflict
	}
	if err != nil {
		return v1.Goal{}, ErrInvalid
	}
	return goalDTO(goal), nil
}

func (b *DomainBackend) DeleteGoal(ctx context.Context, id string) error {
	err := b.Goals.Clear(ctx, id)
	if errors.Is(err, session.ErrGoalNotFound) || errors.Is(err, session.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func goalDTO(goal session.Goal) v1.Goal {
	return v1.Goal{ID: goal.ID, SessionID: goal.SessionID, Objective: goal.Objective, Status: string(goal.Status), TokenBudget: goal.TokenBudget, TokensUsed: goal.TokensUsed, RemainingTokens: goal.RemainingTokens(), ElapsedSeconds: goal.ElapsedSeconds, CreatedAt: goal.CreatedAt, UpdatedAt: goal.UpdatedAt}
}

type AgentSessionController interface {
	Get(string) agent.AgentSession
	Active() []agent.Active
	Status(string) agent.Status
	Interrupt(context.Context, string) error
	Remove(string) error
}

type ProcessLifecycle interface {
	SuspendSession(context.Context, string) error
	ResumeSession(string)
	InterruptSession(string) error
	DeleteSession(string) error
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
	if b.AgentSessions != nil {
		for _, item := range b.AgentSessions.Active() {
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
	selection, err := b.requestSelection(request.Agent, request.Mode, request.Model, request.Variant)
	if err != nil {
		return v1.Session{}, err
	}
	item, err := b.Sessions.CreateSelected(ctx, session.CreateParams{ParentSessionID: request.ParentSessionID, ProjectID: request.ProjectID, ProjectRoot: b.ProjectRoot, Title: request.Title}, selection)
	if errors.Is(err, session.ErrNotFound) {
		return v1.Session{}, ErrNotFound
	}
	if errors.Is(err, session.ErrParentProjectMismatch) {
		return v1.Session{}, ErrInvalid
	}
	if err != nil {
		return v1.Session{}, err
	}
	return sessionDTO(item), nil
}

func (b *DomainBackend) requestSelection(agentID, modeID, modelID string, variant *string) (session.Selection, error) {
	selection := b.DefaultSelection
	if modeID != "" {
		agentID = modeID
	}
	if variant != nil {
		selection.Variant = *variant
	}
	if agentID != "" {
		selection.Agent = agentID
	}
	if modelID != "" {
		providerID, selectedModel, qualified := strings.Cut(modelID, "/")
		if qualified {
			selection.Provider, selection.Model = providerID, selectedModel
		} else {
			selection.Model = modelID
		}
	}
	if !completeSelection(selection) {
		return session.Selection{}, ErrModelRequired
	}
	if err := b.validateSelection(selection); err != nil {
		return session.Selection{}, err
	}
	return selection, nil
}

func (b *DomainBackend) ClaimSession(ctx context.Context, request v1.ClaimSessionRequest) (v1.ClaimSessionResponse, error) {
	if request.WorkingDirectory == "" || request.HostKey == "" || request.PID <= 0 {
		return v1.ClaimSessionResponse{}, ErrInvalid
	}
	selection, err := b.requestSelection(request.Agent, request.Mode, request.Model, request.Variant)
	if err != nil {
		return v1.ClaimSessionResponse{}, err
	}
	claim, err := b.Sessions.ClaimInteractive(ctx, session.InteractiveOwner{WorkingDirectory: request.WorkingDirectory, HostKey: request.HostKey, PID: request.PID}, session.CreateParams{ProjectID: request.ProjectID, ProjectRoot: b.ProjectRoot, Title: request.Title}, selection, request.ForceNew, processidentity.Alive)
	if err != nil {
		return v1.ClaimSessionResponse{}, err
	}
	return v1.ClaimSessionResponse{Session: sessionDTO(claim.Session), Disposition: string(claim.Disposition)}, nil
}

func (b *DomainBackend) GetSession(ctx context.Context, id string) (v1.Session, error) {
	item, err := b.Sessions.Get(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return v1.Session{}, ErrNotFound
	}
	return sessionDTO(item), err
}

func (b *DomainBackend) UpdateSessionSelection(ctx context.Context, id string, request v1.UpdateSessionSelectionRequest) (v1.SessionSelection, error) {
	if request.Mode != "" {
		request.Agent = request.Mode
	}
	if request.Agent == "" && request.Model == "" && request.Variant == nil {
		return v1.SessionSelection{}, ErrInvalid
	}
	if b.AgentSessions != nil && b.AgentSessions.Status(id) != agent.StatusIdle {
		return v1.SessionSelection{}, ErrSessionActive
	}
	patch := session.SelectionPatch{
		Agent: request.Agent, FallbackAgent: b.DefaultSelection.Agent,
		FallbackProvider: b.DefaultSelection.Provider, Variant: request.Variant,
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
	if _, err := b.GetSession(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) && b.AgentSessions != nil {
			return b.AgentSessions.Remove(id)
		}
		return err
	}
	var cleanupErr error
	if b.Processes != nil {
		cleanupErr = b.Processes.SuspendSession(ctx, id)
		if cleanupErr == nil {
			defer b.Processes.ResumeSession(id)
		}
	}
	if b.AgentSessions != nil {
		cleanupErr = errors.Join(cleanupErr, b.AgentSessions.Interrupt(ctx, id))
	}
	if b.Processes != nil {
		cleanupErr = errors.Join(cleanupErr, b.Processes.DeleteSession(id))
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	err := b.Sessions.Delete(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if b.AgentSessions != nil {
		return b.AgentSessions.Remove(id)
	}
	return nil
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
	if b.AgentSessions == nil {
		return
	}
	b.AgentSessions.Get(id).Wake()
	if b.Events != nil {
		data, _ := json.Marshal(v1.SessionStatus{Kind: "running"})
		b.Events.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: id, Data: data})
	}
}

func (b *DomainBackend) Interrupt(ctx context.Context, id string) error {
	if _, err := b.GetSession(ctx, id); err != nil {
		return err
	}
	var err error
	if b.Processes != nil {
		err = b.Processes.SuspendSession(ctx, id)
		if err == nil {
			defer b.Processes.ResumeSession(id)
		}
	}
	if b.AgentSessions != nil {
		err = errors.Join(err, b.AgentSessions.Interrupt(ctx, id))
	}
	if b.Processes != nil {
		err = errors.Join(err, b.Processes.InterruptSession(id))
	}
	// If the interrupted turn leaves queued inputs behind, resume the drain so
	// they are processed without the user re-prompting. A fresh context is used
	// because the request context may have elapsed while waiting for the drain
	// to unwind; Wake itself starts the new drain on a detached context.
	if b.AgentSessions != nil && b.Sessions != nil {
		wakeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pending, pendingErr := b.Sessions.HasPendingInputs(wakeCtx, id); pendingErr == nil && pending {
			b.Wake(id)
		}
	}
	return err
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
	stream, err := b.Events.ReplayAndSubscribe(ctx, id, after, capacity)
	if err != nil {
		return nil, err
	}
	return &EventStream{Replay: stream.Replay, Durable: stream.Durable, Live: stream.Transient, Close: stream.Close}, nil
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
		choices := make([]v1.PermissionChoice, len(item.Request.Choices))
		for i, choice := range item.Request.Choices {
			choices[i] = v1.PermissionChoice{
				Value: choice.Value, Decision: choice.Decision,
				Label: choice.Label, Description: choice.Description, RequiresReason: choice.RequiresReason,
			}
		}
		out.Items = append(out.Items, v1.Permission{ID: item.ID, ToolID: item.Request.ToolID, Description: item.Request.Description, CanonicalInput: item.Request.CanonicalInput, Review: item.Request.Review, Choices: choices})
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
	var permissionItem v1.Permission
	for _, item := range items.Items {
		if item.ID == requestID {
			permissionItem = item
			break
		}
	}
	if len(reply.Reason) > 4096 {
		return ErrInvalid
	}
	// The reply must be one of the answers the requesting tool offered. Choices
	// are read from the pending request held by the broker, never from the
	// client.
	if len(permissionItem.Choices) > 0 && !replyMatchesChoices(permissionItem.Choices, reply) {
		return ErrInvalid
	}
	switch reply.Decision {
	case "deny":
		if reply.Reason != "" {
			err = b.Permissions.RejectWithReason(requestID, reply.Reason)
		} else {
			err = b.Permissions.Reject(requestID)
		}
	case "allow":
		if reply.Reason != "" {
			return ErrInvalid
		}
		err = b.Permissions.ReplyOnce(requestID)
	default:
		return ErrInvalid
	}
	if err != nil {
		return ErrPermissionNotFound
	}
	if b.Events != nil {
		data, _ := json.Marshal(v1.PermissionResolved{RequestID: requestID, Decision: reply.Decision})
		b.Events.PublishEvent(v1.Event{Type: v1.EventPermissionReply, SessionID: sessionID, Data: data})
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
		if b.Events != nil {
			data, _ := json.Marshal(v1.QuestionResolved{RequestID: requestID, Rejected: true})
			b.Events.PublishEvent(v1.Event{Type: v1.EventQuestionReply, SessionID: sessionID, Data: data})
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
	if b.Events != nil {
		data, _ := json.Marshal(v1.QuestionResolved{RequestID: requestID})
		b.Events.PublishEvent(v1.Event{Type: v1.EventQuestionReply, SessionID: sessionID, Data: data})
	}
	return nil
}

func (b *DomainBackend) ListModels(context.Context) (v1.ModelList, error) {
	out := v1.ModelList{Items: []v1.Model{}}
	for _, provider := range b.providerList() {
		if provider == nil {
			continue
		}
		for _, model := range provider.Models() {
			variants := make([]v1.ModelVariant, len(model.Capabilities.Variants))
			for i, variant := range model.Capabilities.Variants {
				variants[i] = v1.ModelVariant{Name: variant.Name, ReasoningEffort: variant.ReasoningEffort}
			}
			out.Items = append(out.Items, v1.Model{Provider: provider.ID(), ID: model.ID, Name: model.Name, ContextWindow: model.ContextWindow, MaxOutputTokens: model.MaxOutputTokens, Tools: model.Capabilities.Tools, Reasoning: model.Capabilities.Reasoning, Output: append([]string(nil), model.Capabilities.Output...), Variants: variants})
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

func (b *DomainBackend) GetModelInfo(_ context.Context, providerID string, modelID string) (v1.Model, error) {
	for _, provider := range b.providerList() {
		if provider == nil || provider.ID() != providerID {
			continue
		}
		for _, model := range provider.Models() {
			if model.ID != modelID {
				continue
			}
			variants := make([]v1.ModelVariant, len(model.Capabilities.Variants))
			for i, variant := range model.Capabilities.Variants {
				variants[i] = v1.ModelVariant{Name: variant.Name, ReasoningEffort: variant.ReasoningEffort}
			}
			return v1.Model{Provider: provider.ID(), ID: model.ID, Name: model.Name, ContextWindow: model.ContextWindow, MaxOutputTokens: model.MaxOutputTokens, Tools: model.Capabilities.Tools, Reasoning: model.Capabilities.Reasoning, Output: append([]string(nil), model.Capabilities.Output...), Variants: variants}, nil
		}
	}
	return v1.Model{}, ErrNotFound
}

// providerList reports the live providers. When the resolver is a registry, it
// is the single source of truth and reflects a hot reload; otherwise the
// startup snapshot in Providers is used so backends assembled without a
// resolver keep working.
func (b *DomainBackend) providerList() []provider.Provider {
	if lister, ok := b.ProviderResolver.(agent.ProviderLister); ok {
		return lister.List()
	}
	return b.Providers
}

func (b *DomainBackend) SubscriptionUsage(ctx context.Context) (v1.SubscriptionUsage, error) {
	item, reporter := b.usageReporter()
	if reporter == nil {
		return v1.SubscriptionUsage{}, errors.New("httpapi: subscription usage is unavailable")
	}
	usage, err := reporter.Usage(ctx)
	if err != nil {
		return v1.SubscriptionUsage{}, err
	}
	return v1.SubscriptionUsage{
		Provider: item.ID(), PlanType: usage.PlanType,
		PrimaryWindow: mapSubscriptionWindow(usage.PrimaryWindow), SecondaryWindow: mapSubscriptionWindow(usage.SecondaryWindow),
		Credits: mapSubscriptionCredits(usage.Credits),
	}, nil
}

// usageReporter prefers the provider backing the default selection so a session
// reports its own subscription, falling back to the first provider that can
// report usage at all.
func (b *DomainBackend) usageReporter() (provider.Provider, provider.UsageReporter) {
	var fallbackProvider provider.Provider
	var fallbackReporter provider.UsageReporter
	for _, item := range b.providerList() {
		if item == nil {
			continue
		}
		reporter, ok := item.(provider.UsageReporter)
		if !ok {
			continue
		}
		if item.ID() == b.DefaultSelection.Provider {
			return item, reporter
		}
		if fallbackReporter == nil {
			fallbackProvider, fallbackReporter = item, reporter
		}
	}
	return fallbackProvider, fallbackReporter
}

func mapSubscriptionWindow(window *provider.UsageWindow) *v1.UsageWindow {
	if window == nil {
		return nil
	}
	remaining := 100 - window.UsedPercent
	if remaining < 0 {
		remaining = 0
	} else if remaining > 100 {
		remaining = 100
	}
	return &v1.UsageWindow{UsedPercent: window.UsedPercent, RemainingPercent: remaining, ResetAt: window.ResetAt, LimitWindowSeconds: window.LimitWindowSeconds}
}

func mapSubscriptionCredits(credits *provider.UsageCredits) *v1.UsageCredits {
	if credits == nil {
		return nil
	}
	return &v1.UsageCredits{HasCredits: credits.HasCredits, Balance: credits.Balance}
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

func (b *DomainBackend) CompleteTurn(ctx context.Context, sessionID, messageID string) (v1.TurnCompletion, error) {
	selected, err := b.Sessions.Get(ctx, sessionID)
	if err != nil {
		return v1.TurnCompletion{}, err
	}
	result, err := b.Modes.CompleteTurn(selected.Agent, sessionID, messageID)
	if err != nil {
		return v1.TurnCompletion{}, err
	}
	if result == (mode.TurnCompleteResult{}) {
		return v1.TurnCompletion{}, nil
	}
	raw, err := json.Marshal(result)
	return v1.TurnCompletion{TurnComplete: raw}, err
}

func (b *DomainBackend) ListModes(context.Context) (v1.ModeList, error) {
	out := v1.ModeList{Items: []v1.Mode{}}
	if b.Modes == nil {
		return out, nil
	}
	for _, item := range b.Modes.List() {
		profile := item.Profile()
		entry := v1.Mode{ID: item.ID(), ReadOnly: profile.ReadOnly, MaxTurns: profile.MaxTurns}
		if result := item.OnTurnComplete(); result != (mode.TurnCompleteResult{}) {
			if raw, err := json.Marshal(result); err == nil {
				entry.TurnComplete = raw
			}
		}
		out.Items = append(out.Items, entry)
	}
	return out, nil
}

// ListTools exposes the declared presentation of every registered tool so that
// clients render tool activity without knowing which tools exist. An empty
// snapshot yields an empty list, and such a client falls back to generic
// rendering.
func (b *DomainBackend) ListTools(context.Context) (v1.ToolList, error) {
	out := v1.ToolList{Items: []v1.Tool{}}
	for _, entry := range b.Tools.Presentations() {
		out.Items = append(out.Items, v1.Tool{ID: entry.ID, Presentation: toolPresentationDTO(entry.Presentation)})
	}
	return out, nil
}

func toolPresentationDTO(presentation tool.Presentation) v1.ToolPresentation {
	fields := make([]v1.ToolLabelPart, 0, len(presentation.Label.Fields))
	for _, field := range presentation.Label.Fields {
		fields = append(fields, v1.ToolLabelPart{
			Names: field.Names, Quote: field.Quote, Default: field.Default,
			Array: field.Array, Item: field.Item, Overflow: field.Overflow, TaskName: field.TaskName,
		})
	}
	return v1.ToolPresentation{
		Label: v1.ToolLabel{
			Kind: string(presentation.Label.Kind), Fields: fields, Source: presentation.Label.Source,
			Prefix: presentation.Label.Prefix, Noun: presentation.Label.Noun,
		},
		Redact: presentation.Redact, Muted: presentation.Muted,
		Result: string(presentation.Result), Output: string(presentation.Output), Failure: string(presentation.Failure),
		Subagent: presentation.Subagent, Modeline: presentation.Modeline, LiveOnly: presentation.LiveOnly, LabelInPermission: presentation.LabelInPermission,
		CompletedInput: v1.ToolCompletedInput{
			Fields: presentation.CompletedInput.Fields, TerminalOnly: presentation.CompletedInput.TerminalOnly,
		},
	}
}

// replyMatchesChoices reports whether a reply is one of the answers the
// requesting tool offered. A reason is accepted only for a choice which asks
// for one.
func replyMatchesChoices(choices []v1.PermissionChoice, reply v1.PermissionReply) bool {
	for _, choice := range choices {
		if choice.Decision != reply.Decision {
			continue
		}
		if reply.Reason != "" && !choice.RequiresReason {
			continue
		}
		return true
	}
	return false
}

func sessionDTO(item session.AgentSessionDto) v1.Session {
	return v1.Session{ID: item.ID, ParentSessionID: item.ParentSessionID, Name: item.Name, ProjectID: item.ProjectID, Title: item.Title, Agent: item.Agent, Mode: item.Agent, Provider: item.Provider, Model: item.Model, Variant: item.Variant, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func selectionDTO(item session.AgentSessionDto) v1.SessionSelection {
	return v1.SessionSelection{Agent: item.Agent, Mode: item.Agent, Provider: item.Provider, Model: item.Model, Variant: item.Variant}
}

func completeSelection(selection session.Selection) bool {
	return selection.Agent != "" && selection.Provider != "" && selection.Model != ""
}

func (b *DomainBackend) validateSelection(selection session.Selection) error {
	if !completeSelection(selection) {
		return ErrModelRequired
	}
	if b.Modes != nil {
		if _, err := b.Modes.Get(selection.Agent); err != nil {
			// Agent remains a compatibility input and is used by task sessions.
			if b.Agents == nil {
				return fmt.Errorf("%w: %v", ErrInvalidSelection, err)
			}
			if _, agentErr := b.Agents.Get(selection.Agent); agentErr != nil {
				return fmt.Errorf("%w: %v", ErrInvalidSelection, err)
			}
		}
	} else {
		if b.Agents == nil {
			return errors.New("httpapi: mode registry is unavailable")
		}
		if _, err := b.Agents.Get(selection.Agent); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidSelection, err)
		}
	}
	resolver := b.ProviderResolver
	if resolver == nil {
		registry, err := agent.NewProviderRegistry(b.Providers...)
		if err != nil {
			return err
		}
		resolver = registry
	}
	_, model, err := resolver.Resolve(selection.Provider, selection.Model)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSelection, err)
	}
	if selection.Variant != "" {
		if _, ok := model.Capabilities.Variant(selection.Variant); !ok {
			return fmt.Errorf("%w: unknown variant %q", ErrInvalidSelection, selection.Variant)
		}
	}
	return nil
}

func containsPermission(items []v1.Permission, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
