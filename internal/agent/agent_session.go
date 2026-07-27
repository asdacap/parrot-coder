package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/compaction"
	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/queue"
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/session"
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	"github.com/amirulashraf/parrot-coder/internal/systemcontext"
	"github.com/amirulashraf/parrot-coder/internal/tool"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type agentSession struct {
	dto                    session.AgentSessionDto
	parent                 AgentSession
	user                   *userSession
	agentSessionRepository *agentSessionRepository
	store                  session.AgentSessionStore
	systemPrompt           SystemPrompt
	config                 AgentSessionConfig
	stateDirectories       UserSessionStateDirectories
	profiles               ProfileResolver
	modes                  ModeResolver
	providers              ProviderResolver
	workspace              *workspace.Workspace
	outputs                *tool.OutputStore
	processes              *process.Runner
	live                   LivePublisher
	compactor              Compactor
	goals                  *session.GoalService
	statusObserver         StatusObserver
	toolPanicLogger        func(context.Context, string, string, any, []byte)
	securityProfile        *agentSessionSecurityProfile
	mu                     sync.Mutex
	selectionMu            sync.Mutex
	status                 Status
	turn                   *turnState
	events                 event.EventBroker
	managedTask            bool
	childCreations         int
	removed                bool
	shuttingDown           bool
	childTurns             *childTurnSemaphore
	observers              []LifecycleObserver
	toolSnapshot           tool.Snapshot
	toolExecutor           tool.Executor
	maxChildPromptBytes    int
	maxChildResultBytes    int
	turnFinishListeners    []*turnFinishListener
}

type LifecycleObserver interface {
	LifecycleComplete(sessionID string, err error)
}

type LifecycleStartObserver interface {
	LifecycleStarted(sessionID string)
}

type AgentStatus string

const (
	StatusIdle         AgentStatus = "idle"
	StatusPending      AgentStatus = "pending"
	StatusBlocked      AgentStatus = "blocked"
	StatusRunning      AgentStatus = "running"
	StatusInterrupting AgentStatus = "interrupting"
	StatusSucceeded    AgentStatus = "succeeded"
	StatusFailed       AgentStatus = "failed"
	StatusCanceled     AgentStatus = "canceled"
)

type Active struct {
	SessionID string
	Status    AgentStatus
}

var noSteerSignal <-chan struct{} = make(chan struct{})

// ErrNoFinalAssistantMessage means a completed turn did not persist a terminal assistant response.
var ErrNoFinalAssistantMessage = errors.New("agent: session produced no assistant output")

type turnState struct {
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
	wake              bool
	steer             chan struct{}
	steerSignaled     bool
	steerSequence     int64
	status            AgentStatus
	err               error
	messageID         string
	result            Status
	releaseQuota      func()
	mode              Mode
	profile           Profile
	latestAssistantID string
}

type turnFinishResult struct {
	content string
	err     error
}

type turnFinishListener struct {
	session   *agentSession
	messageID string
	result    chan turnFinishResult
	once      sync.Once
}

func (l *turnFinishListener) notify(ctx context.Context, initialMessageID, terminalAssistantID string, lifecycleErr error) {
	received, content, err := l.session.messageResponseThrough(ctx, l.messageID, terminalAssistantID)
	if !received {
		if err == nil && initialMessageID != l.messageID {
			return
		}
		if err == nil {
			err = lifecycleErr
		}
	} else if errors.Is(err, ErrNoFinalAssistantMessage) && lifecycleErr != nil {
		err = lifecycleErr
	}
	l.once.Do(func() { l.result <- turnFinishResult{content: content, err: err} })
}

type TurnWorkingEvent struct {
	Status Status
	Report func(ChildProgress)
}

// AgentSession is the runtime for one persisted agent session. It owns both
// execution and the synchronization which ensures only one execution lifecycle
// is active for its bound session ID.
type AgentSession interface {
	ID() string
	Name() string
	Parent() AgentSession
	CreateChild(context.Context, ChildRequest) (AgentSession, error)
	Status() Status
	Observe() (ChildTurnObserver, error)
	ResolveChild(string) (AgentSession, error)
	Prompt(context.Context, string) (string, error)
	Send(context.Context, string, string) (session.Admission, error)
	Wake()
	Resume(context.Context) error
	Interrupt(context.Context) error
	Shutdown(context.Context) error
	Details(context.Context) (session.AgentSessionDto, error)
	UpdateSelection(context.Context, session.SelectionPatch, session.SelectionValidator) (session.AgentSessionDto, error)
	ListMessages(context.Context) ([]session.Message, error)
	HasPendingInputs(context.Context) (bool, error)
	LatestSequence(context.Context) (int64, error)
}

func (s *agentSession) ID() string           { return s.dto.ID }
func (s *agentSession) Name() string         { return s.dto.Name }
func (s *agentSession) Parent() AgentSession { return s.parent }

func (s *agentSession) Details(ctx context.Context) (session.AgentSessionDto, error) {
	return s.store.Get(ctx)
}

func (s *agentSession) UpdateSelection(ctx context.Context, patch session.SelectionPatch, validate session.SelectionValidator) (session.AgentSessionDto, error) {
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	updated, err := s.store.UpdateSelection(ctx, patch, validate)
	if err != nil {
		if reconciled, getErr := s.store.Get(ctx); getErr == nil {
			s.applySelection(reconciled)
		}
		return session.AgentSessionDto{}, err
	}
	s.applySelection(updated)
	return updated, nil
}

func (s *agentSession) applySelection(updated session.AgentSessionDto) {
	s.mu.Lock()
	s.dto.Agent = updated.Agent
	s.dto.Model = updated.Model
	s.status.Agent = updated.Agent
	s.status.Model = updated.Model
	s.mu.Unlock()
	if s.agentSessionRepository != nil {
		s.agentSessionRepository.updateSelection(s, updated)
	}
}

func (s *agentSession) ListMessages(ctx context.Context) ([]session.Message, error) {
	return s.store.ListMessages(ctx)
}
func (s *agentSession) HasPendingInputs(ctx context.Context) (bool, error) {
	return s.store.HasPendingInputs(ctx)
}
func (s *agentSession) LatestSequence(ctx context.Context) (int64, error) {
	return s.store.LatestSequence(ctx)
}
func (s *agentSession) Status() Status {
	s.mu.Lock()
	status := cloneStatus(s.status)
	if s.turn != nil && turnActive(s.turn.status) {
		status.State = s.turn.status
	}
	s.mu.Unlock()
	return status
}

func (s *agentSession) executionStatus() AgentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil && turnActive(s.turn.status) {
		return s.turn.status
	}
	return StatusIdle
}

func (s *agentSession) Observe() (ChildTurnObserver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		return nil, ErrChildNotFound
	}
	return turnObserver{turn: s.turn}, nil
}

func (s *agentSession) ResolveChild(identifier string) (AgentSession, error) {
	if identifier == "" || s.agentSessionRepository == nil {
		return nil, ErrChildNotFound
	}
	pending := s.agentSessionRepository.ChildSessions(s.ID())
	for len(pending) > 0 {
		relation := pending[0]
		pending = pending[1:]
		child, ok := s.agentSessionRepository.Lookup(relation.SessionID)
		if !ok {
			continue
		}
		if relation.SessionID == identifier || child.Name() == identifier {
			return child, nil
		}
		pending = append(pending, s.agentSessionRepository.ChildSessions(relation.SessionID)...)
	}
	return nil, ErrChildNotFound
}

// Prompt sends input with a generated message ID and waits for that input's
// terminal assistant response.
func (s *agentSession) Prompt(ctx context.Context, content string) (string, error) {
	return s.sendAndWait(ctx, content)
}

// Send admits steer input with the caller's idempotency key and wakes the
// session without waiting for it to idle.
func (s *agentSession) Send(ctx context.Context, messageID, content string) (session.Admission, error) {
	return s.send(ctx, messageID, content, content)
}

func (s *agentSession) send(ctx context.Context, messageID, content, measuredContent string) (session.Admission, error) {
	if s.parent != nil {
		if err := validateChildPrompt(measuredContent, s.maxChildPromptBytes); err != nil {
			return session.Admission{}, err
		}
	}
	admission, _, err := s.admitAndStart(ctx, messageID, content, false, true)
	return admission, err
}

func (s *agentSession) sendAndWait(ctx context.Context, content string) (string, error) {
	if s.parent != nil {
		if err := validateChildPrompt(content, s.maxChildPromptBytes); err != nil {
			return "", err
		}
	}
	messageID, err := id.New("msg")
	if err != nil {
		return "", err
	}
	listener, remove := s.registerTurnFinishListener(messageID)
	defer remove()
	if _, _, err := s.admitAndStart(ctx, messageID, content, false, true); err != nil {
		return "", err
	}
	return s.awaitMessage(ctx, listener)
}

func (s *agentSession) registerTurnFinishListener(messageID string) (*turnFinishListener, func()) {
	listener := &turnFinishListener{session: s, messageID: messageID, result: make(chan turnFinishResult, 1)}
	s.mu.Lock()
	s.turnFinishListeners = append(s.turnFinishListeners, listener)
	s.mu.Unlock()
	var once sync.Once
	return listener, func() {
		once.Do(func() {
			s.mu.Lock()
			for i, current := range s.turnFinishListeners {
				if current == listener {
					s.turnFinishListeners = append(s.turnFinishListeners[:i], s.turnFinishListeners[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
		})
	}
}

func (s *agentSession) notifyTurnFinishListeners(initialMessageID, terminalAssistantID string, lifecycleErr error) {
	s.mu.Lock()
	listeners := append([]*turnFinishListener(nil), s.turnFinishListeners...)
	s.mu.Unlock()
	for _, listener := range listeners {
		listener.notify(context.Background(), initialMessageID, terminalAssistantID, lifecycleErr)
	}
}

func (s *agentSession) awaitMessage(ctx context.Context, listener *turnFinishListener) (string, error) {
	select {
	case result := <-listener.result:
		return result.content, result.err
	case <-ctx.Done():
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.interruptExecution(cleanup)
		cancel()
		return "", ctx.Err()
	}
}

func (s *agentSession) messageResponse(ctx context.Context, messageID string) (string, error) {
	received, content, err := s.messageResponseThrough(ctx, messageID, "")
	if !received {
		return "", ErrNoFinalAssistantMessage
	}
	return content, err
}

func (s *agentSession) messageResponseThrough(ctx context.Context, messageID, terminalAssistantID string) (bool, string, error) {
	messages, err := s.store.ListMessages(ctx)
	if err != nil {
		return false, "", err
	}
	found := false
	for _, message := range messages {
		if !found {
			found = message.Role == "user" && message.ID == messageID
		} else {
			if message.Role == "assistant" && message.FinishReason != string(protocol.FinishToolCalls) {
				if message.Error != "" {
					return true, message.Content, errors.New(message.Error)
				}
				return true, message.Content, nil
			}
		}
		if terminalAssistantID != "" && message.ID == terminalAssistantID {
			break
		}
	}
	if found {
		return true, "", ErrNoFinalAssistantMessage
	}
	return false, "", nil
}

func (s *agentSession) admitAndStart(ctx context.Context, messageID, content string, waitForPermit, start bool) (session.Admission, *turnState, error) {
retry:
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		return session.Admission{}, nil, ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		s.mu.Unlock()
		return session.Admission{}, nil, ErrUserSessionClosed
	}
	if s.turn != nil && (s.turn.status == StatusBlocked || s.turn.status == StatusInterrupting) {
		turn := s.turn
		s.mu.Unlock()
		select {
		case <-turn.done:
			goto retry
		case <-ctx.Done():
			return session.Admission{}, nil, ctx.Err()
		}
	}
	if s.turn != nil && turnActive(s.turn.status) {
		admission, err := s.admitLocked(ctx, messageID, content)
		if err == nil && admission.Created {
			s.turn.wake = true
			s.signalSteerLocked(s.turn, admission.Input.AdmittedSequence)
		}
		state := s.turn
		s.mu.Unlock()
		return admission, state, err
	}
	s.mu.Unlock()

	release, blocked, err := s.tryAcquireTurnQuota()
	if err != nil || blocked && !waitForPermit {
		if blocked {
			err = ErrChildConcurrency
		}
		return session.Admission{}, nil, err
	}
	s.mu.Lock()
	if s.turn != nil && turnActive(s.turn.status) {
		s.mu.Unlock()
		if release != nil {
			release()
		}
		goto retry
	}
	admission, err := s.admitLocked(ctx, messageID, content)
	if err != nil || !admission.Created {
		state := s.turn
		s.mu.Unlock()
		if release != nil {
			release()
		}
		return admission, state, err
	}
	state := s.newTurnLocked(admission.Input.MessageID, release, blocked)
	s.mu.Unlock()
	if start {
		s.startTurn(state)
	}
	return admission, state, nil
}

func (s *agentSession) startTurn(state *turnState) {
	s.mu.Lock()
	turnStatus := state.status
	status := cloneStatus(s.status)
	s.mu.Unlock()
	if turnStatus == StatusInterrupting {
		go s.finishTurn(state, "", context.Canceled, false)
		return
	}
	s.events.Publish(event.BrokerEvent{Name: event.TurnStarted, Payload: status})
	if turnStatus == StatusBlocked {
		go s.waitForTurnPermit(state)
	} else {
		go s.runTurn(state)
	}
}

func (s *agentSession) abortTurn(state *turnState) {
	state.cancel()
	s.mu.Lock()
	if state.releaseQuota != nil {
		state.releaseQuota()
		state.releaseQuota = nil
	}
	state.err = context.Canceled
	if s.turn == state {
		s.turn = nil
	}
	close(state.done)
	s.mu.Unlock()
}

func (s *agentSession) admitLocked(ctx context.Context, messageID, content string) (session.Admission, error) {
	if s.removed {
		return session.Admission{}, ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		return session.Admission{}, ErrUserSessionClosed
	}
	return s.store.Admit(ctx, session.AdmitParams{MessageID: messageID, Content: content, Delivery: session.DeliverySteer})
}

func (s *agentSession) startEmptyTurn(wake bool) (*turnState, error) {
retry:
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		return nil, ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		s.mu.Unlock()
		return nil, ErrUserSessionClosed
	}
	if s.turn != nil && turnActive(s.turn.status) {
		if wake {
			s.turn.wake = true
		}
		state := s.turn
		s.mu.Unlock()
		return state, nil
	}
	s.mu.Unlock()

	release, blocked, err := s.tryAcquireTurnQuota()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.removed || s.shuttingDown || s.turn != nil && turnActive(s.turn.status) {
		s.mu.Unlock()
		if release != nil {
			release()
		}
		goto retry
	}
	state := s.newTurnLocked("", release, blocked)
	s.mu.Unlock()
	s.startTurn(state)
	return state, nil
}

// Wake coalesces with an active turn and returns immediately.
func (s *agentSession) Wake() {
	_, _ = s.startEmptyTurn(true)
}

// Resume starts an idle turn or joins the complete lifetime of an active one.
func (s *agentSession) Resume(ctx context.Context) error {
	state, err := s.startEmptyTurn(false)
	if err != nil {
		return err
	}
	return s.wait(ctx, state)
}

func (s *agentSession) wait(ctx context.Context, state *turnState) error {
	if state == nil {
		return nil
	}
	select {
	case <-state.done:
		return state.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *agentSession) Interrupt(ctx context.Context) error {
	if err := s.interruptExecution(ctx); ctx.Err() != nil {
		return err
	}
	return nil
}

func (s *agentSession) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	state := s.turn
	if state == nil || !turnActive(state.status) {
		s.mu.Unlock()
		return nil
	}
	state.status = StatusInterrupting
	state.wake = false
	state.cancel()
	s.mu.Unlock()
	if err := s.wait(ctx, state); ctx.Err() != nil {
		return err
	}
	return nil
}

func (s *agentSession) interruptExecution(ctx context.Context) error {
	s.mu.Lock()
	state := s.turn
	if state == nil || !turnActive(state.status) {
		s.mu.Unlock()
		return nil
	}
	state.status = StatusInterrupting
	state.wake = false
	state.cancel()
	s.mu.Unlock()
	return s.wait(ctx, state)
}

func (s *agentSession) signalSteerLocked(state *turnState, sequence int64) {
	if sequence > state.steerSequence {
		state.steerSequence = sequence
	}
	if !state.steerSignaled {
		close(state.steer)
		state.steerSignaled = true
	}
}

func (s *agentSession) acknowledgeSteers(cutoff int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.turn; state != nil && state.steerSignaled && state.steerSequence <= cutoff {
		state.steer = make(chan struct{})
		state.steerSignaled = false
		state.steerSequence = 0
	}
}

func (s *agentSession) steerSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		return noSteerSignal
	}
	return s.turn.steer
}

func (s *agentSession) newTurnLocked(messageID string, release func(), blocked bool) *turnState {
	ctx, cancel := context.WithCancel(context.Background())
	status := StatusRunning
	if blocked {
		status = StatusBlocked
	}
	state := &turnState{ctx: ctx, cancel: cancel, done: make(chan struct{}), steer: make(chan struct{}), status: status, messageID: messageID, releaseQuota: release}
	s.turn = state
	s.status.Turn++
	s.status.State, s.status.FinishedAt = status, time.Time{}
	if !blocked {
		s.status.StartedAt = time.Now().UTC()
	} else {
		s.status.StartedAt = time.Time{}
	}
	s.status.Output, s.status.Error, s.status.NoFinalMessage, s.status.Truncated = "", "", false, false
	s.status.Usage, s.status.ToolUses = ChildUsage{}, 0
	return state
}

func (s *agentSession) reserveChildCreation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		return ErrUserSessionClosed
	}
	s.childCreations++
	return nil
}

func (s *agentSession) releaseChildCreation() {
	s.mu.Lock()
	s.childCreations--
	s.mu.Unlock()
}

func (s *agentSession) removeIfIdle(remove func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return nil
	}
	if s.turn != nil && turnActive(s.turn.status) || s.childCreations != 0 {
		return ErrAgentSessionActive
	}
	if err := remove(); err != nil {
		return err
	}
	s.removed = true
	return nil
}

func (s *agentSession) waitForTurnPermit(state *turnState) {
	release, err := s.acquireTurnQuota(state.ctx)
	if err != nil {
		s.finishTurn(state, "", err, false)
		return
	}
	s.mu.Lock()
	if state.ctx.Err() != nil || s.turn != state {
		s.mu.Unlock()
		if release != nil {
			release()
		}
		s.finishTurn(state, "", context.Canceled, false)
		return
	}
	state.releaseQuota = release
	state.status = StatusRunning
	s.status.State, s.status.StartedAt = StatusRunning, time.Now().UTC()
	s.mu.Unlock()
	go s.runTurn(state)
}

func (s *agentSession) runTurn(state *turnState) {
	s.mu.Lock()
	canceled := state.ctx.Err() != nil || state.status == StatusInterrupting
	s.mu.Unlock()
	if canceled {
		s.finishTurn(state, "", context.Canceled, false)
		return
	}
	if err := s.prepareTurn(state); err != nil {
		s.finishTurn(state, "", err, false)
		return
	}
	s.started()
	stop := s.events.Publish(event.BrokerEvent{Name: event.TurnWorking, Payload: TurnWorkingEvent{Status: s.Status(), Report: func(progress ChildProgress) { s.reportTurnProgress(state, progress) }}})
	var err error
	for {
		err = s.drainOnce(state.ctx)
		if err == nil && state.ctx.Err() == nil {
			var prepared bool
			prepared, err = s.prepareQueueNotification(state.ctx)
			if err == nil && prepared {
				continue
			}
			if err == nil {
				prepared, err = s.prepareContinuation(state.ctx)
				if err == nil && prepared {
					continue
				}
			}
		}

		s.mu.Lock()
		if state.wake && state.ctx.Err() == nil {
			state.wake = false
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		break
	}
	stop()
	output := ""
	noFinalMessage := false
	if s.parent != nil && state.messageID != "" && err == nil {
		output, err = s.messageResponse(context.Background(), state.messageID)
		if errors.Is(err, ErrNoFinalAssistantMessage) {
			noFinalMessage, err = true, nil
		}
	}
	if err == nil && !noFinalMessage && state.ctx.Err() == nil {
		s.mu.Lock()
		messageID := state.latestAssistantID
		s.mu.Unlock()
		if messageID == "" {
			err = ErrNoFinalAssistantMessage
		} else {
			var emitted []event.BrokerEvent
			emitted, err = state.mode.OnTurnFinish(s.ID(), messageID)
			if err == nil {
				for _, item := range emitted {
					s.events.Publish(item)
				}
			}
		}
	}
	s.finishTurn(state, output, err, noFinalMessage)
}

func (s *agentSession) prepareTurn(state *turnState) error {
	selected, err := s.store.Get(state.ctx)
	if err != nil {
		return err
	}
	resolved, err := s.modes.Get(selected.Agent)
	if err != nil {
		return err
	}
	turnProfile, err := resolved.OnTurnStart(s.ID())
	if err != nil {
		return err
	}
	stateDirectory, err := s.stateDirectories.Prepare(s.ID())
	if err != nil {
		return err
	}
	state.mode, state.profile = resolved, turnProfile.Profile()
	s.securityProfile = newAgentSessionSecurityProfile(turnProfile)
	s.securityProfile.AddCapability(security.Rule{Path: stateDirectory.ScratchPath(), Action: security.ActionAllowWrite})
	s.securityProfile.AddCapability(security.Rule{Path: s.user.queues.Directory(), Action: security.ActionAllowRead})
	return nil
}

func (s *agentSession) finishTurn(state *turnState, output string, runErr error, noFinalMessage bool) {
	canceled := state.ctx.Err() != nil
	state.cancel()
	truncated := false
	if s.parent != nil {
		if canceled && errors.Is(runErr, context.Canceled) {
			runErr = ErrChildCanceled
		}
		var outputTruncated, errorTruncated bool
		output, outputTruncated = truncateChild(output, s.maxChildResultBytes)
		if runErr != nil {
			var errText string
			errText, errorTruncated = truncateChild(runErr.Error(), s.maxChildResultBytes)
			if errorTruncated {
				runErr = errors.New(errText)
			}
		}
		truncated = outputTruncated || errorTruncated
	}
	s.mu.Lock()
	s.status.Truncated = truncated
	s.status.NoFinalMessage = noFinalMessage
	state.err = runErr
	status, errText := StatusSucceeded, ""
	if runErr != nil {
		status, errText = StatusFailed, runErr.Error()
	}
	if state.status == StatusInterrupting && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, ErrChildCanceled)) {
		status = StatusCanceled
	}
	s.status.State, s.status.Output, s.status.Error = status, output, errText
	s.status.FinishedAt = time.Now().UTC()
	state.result = cloneStatus(s.status)
	state.status = StatusInterrupting
	if state.releaseQuota != nil {
		state.releaseQuota()
		state.releaseQuota = nil
	}
	s.mu.Unlock()
	s.events.Publish(event.BrokerEvent{Name: event.TurnFinished, Payload: state.result})
	s.mu.Lock()
	state.status = status
	close(state.done)
	s.mu.Unlock()
	s.notifyTurnFinishListeners(state.messageID, state.latestAssistantID, runErr)
	s.events.Publish(event.BrokerEvent{Name: event.TurnCompleted, Payload: state.result})
	s.completed(runErr)
	s.mu.Lock()
	missedWake := state.wake && !s.shuttingDown && !s.removed
	s.mu.Unlock()
	if missedWake {
		_, _ = s.startEmptyTurn(false)
	}
}

func (s *agentSession) started() {
	for _, observer := range s.observers {
		if starter, ok := observer.(LifecycleStartObserver); ok {
			starter.LifecycleStarted(s.dto.ID)
		}
	}
}

func (s *agentSession) completed(err error) {
	for _, observer := range s.observers {
		if observer != nil {
			observer.LifecycleComplete(s.dto.ID, err)
		}
	}
}

func (s *agentSession) CreateChild(ctx context.Context, request ChildRequest) (AgentSession, error) {
	if s.user == nil {
		return nil, ErrChildNotFound
	}
	return s.user.CreateChild(ctx, s, request)
}

func (s *agentSession) tryAcquireTurnQuota() (func(), bool, error) {
	if s.parent == nil {
		return nil, false, nil
	}
	if s.user == nil {
		return nil, false, ErrChildNotFound
	}
	parent, ok := s.parent.(*agentSession)
	if !ok || parent == nil {
		return nil, false, ErrChildNotFound
	}
	parentPermit, ok := parent.childTurns.tryAcquire()
	if !ok {
		return nil, true, nil
	}
	globalRelease, err := s.user.TryAcquireWorkerQuota()
	if errors.Is(err, ErrChildConcurrency) {
		parentPermit.Release()
		return nil, true, nil
	}
	if err != nil {
		parentPermit.Release()
		return nil, false, err
	}
	var once sync.Once
	return func() { once.Do(func() { parentPermit.Release(); globalRelease() }) }, false, nil
}

func (s *agentSession) acquireTurnQuota(ctx context.Context) (func(), error) {
	if s.user == nil || s.parent == nil {
		return nil, ErrChildNotFound
	}
	parent, ok := s.parent.(*agentSession)
	if !ok || parent == nil {
		return nil, ErrChildNotFound
	}
	parentPermit, err := parent.childTurns.acquire(ctx)
	if err != nil {
		return nil, err
	}
	globalPermit, err := s.user.AcquireWorkerQuota(ctx)
	if err != nil {
		parentPermit.Release()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { parentPermit.Release(); globalPermit.Release() }) }, nil
}

func (s *agentSession) statusSnapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneStatus(s.status)
}

func (s *agentSession) reportTurnProgress(turn *turnState, progress ChildProgress) {
	if progress.ToolUses < 0 || progress.Usage.InputTokens < 0 || progress.Usage.OutputTokens < 0 || progress.Usage.TotalTokens < 0 || progress.Usage.ReasoningTokens < 0 || progress.Usage.CachedInputTokens < 0 {
		return
	}
	s.mu.Lock()
	if turn == nil || s.turn != turn || turn.status != StatusRunning {
		s.mu.Unlock()
		return
	}
	s.status.Usage.InputTokens += progress.Usage.InputTokens
	s.status.Usage.OutputTokens += progress.Usage.OutputTokens
	s.status.Usage.TotalTokens += progress.Usage.TotalTokens
	s.status.Usage.ReasoningTokens += progress.Usage.ReasoningTokens
	s.status.Usage.CachedInputTokens += progress.Usage.CachedInputTokens
	s.status.ToolUses += progress.ToolUses
	status := cloneStatus(s.status)
	s.mu.Unlock()
	s.events.Publish(event.BrokerEvent{Name: event.TurnProgress, Payload: status})
}

func newAgentSession(
	dto session.AgentSessionDto,
	parent AgentSession,
	user *userSession,
	repository *agentSessionRepository,
	store session.AgentSessionStore,
	config AgentSessionConfig,
	stateDirectories UserSessionStateDirectories, profiles ProfileResolver, modes ModeResolver, providers ProviderResolver,
	workspace *workspace.Workspace, outputs *tool.OutputStore, processes *process.Runner,
	live LivePublisher, compactor Compactor, goals *session.GoalService, statusObserver StatusObserver,
	toolPanicLogger func(context.Context, string, string, any, []byte),
	maxConcurrentChildTurns int,
	observers []LifecycleObserver,
	maxChildPromptBytes, maxChildResultBytes int,
	events event.EventBroker,
) *agentSession {
	status := Status{SessionID: dto.ID, RootSession: dto.ID, Agent: dto.Agent, Model: dto.Model, Name: dto.Name, State: StatusIdle}
	if parent != nil {
		parentStatus := parent.Status()
		status.ParentSession = parentStatus.SessionID
		status.RootSession = parentStatus.RootSession
		status.Lineage = append(parentStatus.Lineage, parentStatus.Agent)
		status.Depth = parentStatus.Depth + 1
	}
	return &agentSession{
		dto: dto, parent: parent, user: user, agentSessionRepository: repository, store: store, config: config,
		stateDirectories: stateDirectories, profiles: profiles, modes: modes, providers: providers,
		workspace: workspace, outputs: outputs, processes: processes,
		live: live, compactor: compactor, goals: goals, statusObserver: statusObserver, toolPanicLogger: toolPanicLogger,
		status: status, events: events, childTurns: newChildTurnSemaphore(maxConcurrentChildTurns), observers: observers,
		maxChildPromptBytes: maxChildPromptBytes, maxChildResultBytes: maxChildResultBytes,
	}
}

type agentSessionSecurityProfile struct {
	readOnly     bool
	baseRules    []security.Rule
	capabilities []security.Rule
}

func newAgentSessionSecurityProfile(profile security.SecurityProfile) *agentSessionSecurityProfile {
	session := &agentSessionSecurityProfile{readOnly: profile.IsReadOnly(), baseRules: append([]security.Rule(nil), profile.Rules()...)}
	if layered, ok := profile.(security.LayeredSecurityProfile); ok {
		session.baseRules = layered.BaseRules()
		session.capabilities = layered.CapabilityRules()
	}
	return session
}

func (p *agentSessionSecurityProfile) IsReadOnly() bool { return p.readOnly }
func (p *agentSessionSecurityProfile) Rules() []security.Rule {
	return append(p.BaseRules(), p.capabilities...)
}
func (p *agentSessionSecurityProfile) BaseRules() []security.Rule {
	return append([]security.Rule(nil), p.baseRules...)
}
func (p *agentSessionSecurityProfile) CapabilityRules() []security.Rule {
	return append([]security.Rule(nil), p.capabilities...)
}
func (p *agentSessionSecurityProfile) AddCapability(rule security.Rule) {
	p.capabilities = append(p.capabilities, rule)
}
func (r *agentSession) drainOnce(ctx context.Context) (runErr error) {
	stateDirectory, err := r.stateDirectories.Prepare(r.dto.ID)
	if err != nil {
		return err
	}
	scratchPath := stateDirectory.ScratchPath()
	defer func() {
		if runErr == nil || ctx.Err() != nil || r.goals == nil || !provider.IsUsageLimitError(runErr) {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), r.config.CleanupTimeout)
		defer cancel()
		if _, _, err := r.goals.MarkUsageLimited(cleanupCtx, r.dto.ID); err != nil && !errors.Is(err, session.ErrGoalNotFound) {
			runErr = errors.Join(runErr, err)
		}
	}()
	if err := r.store.RepairActive(ctx); err != nil {
		return err
	}
	turn := 0
	var profile Profile
	r.mu.Lock()
	if r.turn != nil {
		profile = r.turn.profile
	}
	r.mu.Unlock()
	if profile == nil {
		selected, err := r.store.Get(ctx)
		if err != nil {
			return err
		}
		resolved, err := r.modes.Get(selected.Agent)
		if err != nil {
			return err
		}
		turnProfile, err := resolved.OnTurnStart(r.ID())
		if err != nil {
			return err
		}
		profile = turnProfile.Profile()
		r.securityProfile = newAgentSessionSecurityProfile(turnProfile)
		r.securityProfile.AddCapability(security.Rule{Path: scratchPath, Action: security.ActionAllowWrite})
		r.securityProfile.AddCapability(security.Rule{Path: r.user.queues.Directory(), Action: security.ActionAllowRead})
	}
	activeTools := r.toolSnapshot.Only(profile.AllowedTools())
	forceFinal := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cutoff, err := r.store.LatestSequence(ctx)
		if err != nil {
			return err
		}
		epoch, err := r.store.CurrentCompactionEpoch(ctx)
		if err != nil {
			return err
		}
		promoted, err := r.store.PromoteSteers(ctx, cutoff)
		if err != nil {
			return err
		}
		r.acknowledgeSteers(cutoff)

		selected, err := r.store.Get(ctx)
		if err != nil {
			return err
		}
		providerClient, model, variant, err := r.providers.Resolve(selected.Model)
		if err != nil {
			return err
		}
		promptModel := systemcontext.ModelSelection{
			RequestedSelector: selected.Model,
			CanonicalBase:     providerClient.ID() + "/" + model.ID,
			CanonicalSelector: providerClient.ID() + "/" + model.ID,
		}
		if variant != nil {
			promptModel.CanonicalSelector += "/" + variant.Name
		}
		history, err := r.store.ListModelHistory(ctx, epoch.HistoryCutoff)
		if err != nil {
			return err
		}
		ready := len(promoted) > 0
		if len(history) > 0 {
			role := history[len(history)-1].Role
			ready = ready || role == protocol.RoleUser || role == protocol.RoleTool
		}
		if !ready {
			queued, err := r.store.PromoteNextQueue(ctx)
			if err != nil || len(queued) == 0 {
				return err
			}
			history, err = r.store.ListModelHistory(ctx, epoch.HistoryCutoff)
			if err != nil {
				return err
			}
		}
		if turn == 0 && r.statusObserver != nil {
			pending, err := r.store.StatusPromptPending(ctx)
			if err != nil {
				return err
			}
			if pending {
				statusPrompt, err := r.statusObserver.Observe(ctx, r.statusQuery(selected, profile), newProfileInstructions(profile))
				if err != nil {
					return err
				}
				if strings.TrimSpace(statusPrompt) != "" {
					if _, err := r.store.AppendStatusPrompt(ctx, statusPrompt); err != nil {
						return err
					}
					r.publishStatusPromptInjected()
					history, err = r.store.ListModelHistory(ctx, epoch.HistoryCutoff)
					if err != nil {
						return err
					}
				}
			}
		}
		if r.systemPrompt == nil {
			return errors.New("agent: system prompt is required")
		}
		systemPrompt, err := r.systemPrompt.GetSystemPrompt(ctx, promptModel)
		if err != nil {
			return err
		}

		definitions := toolDefinitions(activeTools)
		turn++
		maxTurnsReached := turn >= profile.MaxTurns()
		finalTurn := forceFinal || maxTurnsReached
		instructions := runnerInstructions(systemPrompt, epoch.SummaryPrompt, scratchPath, r.user.queues.Directory(), finalTurn)
		if finalTurn {
			definitions = nil
		}
		if maxTurnsReached {
			r.publishMaxTurnsReached(profile.MaxTurns())
		}
		if r.compactor != nil {
			result, compactErr := r.compactor.Compact(ctx, compaction.Request{
				SessionID: r.dto.ID, ProviderID: providerClient.ID(), Model: model,
				Instructions: instructions, Tools: definitions,
			})
			if compactErr != nil {
				return compactErr
			}
			if result.Status == "complete" {
				epoch, err = r.store.CurrentCompactionEpoch(ctx)
				if err != nil {
					return err
				}
				history, err = r.store.ListModelHistory(ctx, epoch.HistoryCutoff)
				if err != nil {
					return err
				}
				instructions = runnerInstructions(systemPrompt, epoch.SummaryPrompt, scratchPath, r.user.queues.Directory(), finalTurn)
			}
		}
		request := protocol.Request{Model: model.ID, Instructions: instructions, Messages: history, Tools: definitions}
		if variant != nil {
			request.Reasoning = &protocol.ReasoningOptions{Effort: variant.ReasoningEffort, Summary: "auto"}
		}
		calls, finish, err := r.loggedProviderTurn(ctx, providerClient.ID(), turn, providerClient, model, request)
		if err != nil {
			var failure *providerTurnFailure
			if !errors.As(err, &failure) || !failure.overflow || !failure.retrySafe || r.compactor == nil {
				return err
			}
			result, compactErr := r.compactor.Compact(ctx, compaction.Request{
				SessionID: r.dto.ID, ProviderID: providerClient.ID(), Model: model,
				Instructions: instructions, Tools: definitions, Force: true,
			})
			if compactErr != nil || result.Status != "complete" {
				if compactErr != nil {
					return errors.Join(err, compactErr)
				}
				return err
			}
			if recordErr := r.store.RecordCompactionRetry(ctx, failure.code, result.RecordID); recordErr != nil {
				return recordErr
			}
			epoch, err = r.store.CurrentCompactionEpoch(ctx)
			if err != nil {
				return err
			}
			history, err = r.store.ListModelHistory(ctx, epoch.HistoryCutoff)
			if err != nil {
				return err
			}
			request.Instructions = runnerInstructions(systemPrompt, epoch.SummaryPrompt, scratchPath, r.user.queues.Directory(), finalTurn)
			request.Messages = history
			calls, finish, err = r.loggedProviderTurn(ctx, providerClient.ID(), turn, providerClient, model, request)
			if err != nil {
				return err
			}
		}
		if len(calls) > 0 {
			if finalTurn {
				return errors.New("agent: provider returned tools after final-turn tool omission")
			}
			activeGoalID := ""
			if r.goals != nil {
				goal, err := r.goals.Get(ctx, r.dto.ID)
				if err != nil && !errors.Is(err, session.ErrGoalNotFound) {
					return err
				}
				if err == nil && goal.Status == session.GoalActive {
					activeGoalID = goal.ID
				}
			}
			if err := r.executeTools(ctx, selected, profile, activeTools, calls); err != nil {
				return err
			}
			if activeGoalID != "" {
				goal, err := r.goals.Get(ctx, r.dto.ID)
				if err != nil && !errors.Is(err, session.ErrGoalNotFound) {
					return err
				}
				forceFinal = errors.Is(err, session.ErrGoalNotFound) || goal.ID != activeGoalID || goal.Status != session.GoalActive
			}
			continue
		}
		if finish == protocol.FinishStop || finish == protocol.FinishLength || finish == protocol.FinishContentFilter || finish == protocol.FinishIncomplete {
			queued, err := r.store.PromoteNextQueue(ctx)
			if err != nil {
				return err
			}
			if len(queued) == 0 {
				return nil
			}
			continue
		}
		return nil
	}
}

func (r *agentSession) statusQuery(selected session.AgentSessionDto, profile Profile) statusinfo.Query {
	query := statusinfo.Query{
		SessionID:       r.dto.ID,
		ParentSessionID: selected.ParentSessionID,
		Agent:           profile.ID(),
		Model:           selected.Model,
	}
	if selected.ParentSessionID != "" {
		if parent, err := r.user.Get(selected.ParentSessionID); err == nil {
			query.ParentSessionName = parent.Name()
		}
	}
	return query
}

func (r *agentSession) publishStatusPromptInjected() {
	if r.live != nil {
		r.live.PublishProtocol(r.dto.ID, protocol.Event{Type: protocol.EventStatusPromptInjected, Text: "Status prompt injected"})
	}
}

func (r *agentSession) publishMaxTurnsReached(maxTurns int) {
	if r.live != nil {
		r.live.PublishProtocol(r.dto.ID, protocol.Event{Type: protocol.EventMaxTurnsReached, Text: fmt.Sprintf("Maximum turn limit reached (%d); producing final response", maxTurns)})
	}
}

// prepareQueueNotification delivers one monitored queue item only after a
// successful drain. The queue keeps the item locked and durable until the
// synthetic turn is accepted, so regular input wins without requiring rollback.
func (r *agentSession) prepareQueueNotification(ctx context.Context) (bool, error) {
	if r.parent != nil || r.user == nil {
		return false, nil
	}
	return r.user.deliverQueueNotification(ctx, r)
}

func (s *userSession) deliverQueueNotification(ctx context.Context, target *agentSession) (bool, error) {
	if s == nil || s.queues == nil || target == nil {
		return false, nil
	}
	s.queueDeliveryMu.Lock()
	defer s.queueDeliveryMu.Unlock()
	var process bool
	delivered, err := s.queues.DeliverMonitored(func(notification queue.Notification) (bool, error) {
		message := fmt.Sprintf("Queue notification from %q:\n\n%s", notification.Name, notification.Item)
		appendedMessage, appended, err := target.store.AppendMessageIfNoPendingInputs(ctx, notification.ID, protocol.Message{
			Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: message}},
		})
		if err != nil {
			return false, err
		}
		process = appended
		return appendedMessage.ID != "", nil
	})
	return delivered && process, err
}

// PrepareContinuation persists a new synthetic user turn when an active goal
// remains after a successful drain. The coordinator invokes this only while
// the session is otherwise idle, so each continuation is a normal new turn.
func (r *agentSession) prepareContinuation(ctx context.Context) (bool, error) {
	if r.goals == nil {
		return false, nil
	}
	goal, active, err := r.goals.Active(ctx, r.dto.ID)
	if err != nil || !active {
		return false, err
	}
	selected, err := r.store.Get(ctx)
	if err != nil {
		return false, err
	}
	profile, err := r.profiles.GetProfile(selected.Agent)
	if err != nil {
		return false, err
	}
	// A read-only profile cannot update goals, so it also
	// cannot participate in goal continuation.
	if profile.IsReadOnly() {
		return false, nil
	}
	remaining := "unlimited"
	if value := goal.RemainingTokens(); value != nil {
		remaining = fmt.Sprintf("%d", *value)
	}
	message := fmt.Sprintf("Continue working autonomously toward the active goal: %s\n\nThis is an automatic goal continuation turn. Remaining token budget: %s. Use get_goal to inspect current state. Mark the goal complete only when achieved; mark it blocked only for a genuine recurring blocker.", goal.Objective, remaining)
	_, err = r.store.AppendMessage(ctx, protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: message}}})
	return err == nil, err
}
func (r *agentSession) loggedProviderTurn(ctx context.Context, providerID string, turn int, client provider.Provider, model provider.Model, request protocol.Request) ([]completedCall, protocol.FinishReason, error) {
	started := time.Now()
	diagnostics.Event("provider_turn_started",
		"session_id", r.dto.ID, "provider", providerID, "model", request.Model, "turn", turn,
		"message_count", len(request.Messages), "tool_count", len(request.Tools),
	)
	calls, finish, err := r.providerTurn(ctx, client, model, request)
	attributes := []any{
		"session_id", r.dto.ID, "provider", providerID, "model", request.Model, "turn", turn,
		"finish_reason", finish, "tool_call_count", len(calls), "duration_ms", time.Since(started).Milliseconds(),
	}
	if err != nil {
		diagnostics.Error("provider_turn_finished", append(attributes, "status", "error", "error_type", diagnostics.ErrorType(err))...)
	} else {
		diagnostics.Event("provider_turn_finished", append(attributes, "status", "success")...)
	}
	return calls, finish, err
}

func (r *agentSession) providerTurn(ctx context.Context, client provider.Provider, model provider.Model, request protocol.Request) ([]completedCall, protocol.FinishReason, error) {
	assistant, err := r.store.StartAssistant(ctx)
	if err != nil {
		return nil, "", err
	}
	stream, err := provider.StreamWithRetry(ctx, client, request, func(notice provider.RetryNotice) {
		if r.live != nil {
			r.live.PublishProtocol(r.dto.ID, protocol.Event{Type: protocol.EventProviderRetry, Text: notice.String()})
		}
	})
	if err != nil {
		finishErr := r.finishOnCleanup(assistant.ID, nil, protocol.FinishError, err.Error(), "error")
		code, overflow := contextOverflowError(err)
		return nil, "", &providerTurnFailure{err: errors.Join(err, finishErr), code: code, overflow: overflow, retrySafe: finishErr == nil}
	}
	defer stream.Close()
	var text, reasoning strings.Builder
	var reasoningSummary reasoningSummaryAccumulator
	var usage protocol.Usage
	var calls []completedCall
	finish := protocol.FinishIncomplete
	for {
		item, nextErr := stream.Next(ctx)
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				break
			}
			status := "error"
			if ctx.Err() != nil {
				status = "interrupted"
			}
			// A call is only part of valid model history once the provider turn
			// completes and AddToolCall has made it executable. Keeping calls from
			// an interrupted or failed stream would replay a function_call without
			// a matching function_call_output on the next request.
			parts := finalParts(text.String(), preferredReasoning(reasoning.String(), reasoningSummary.String()), nil)
			_ = r.finishOnCleanup(assistant.ID, parts, protocol.FinishError, nextErr.Error(), status)
			return nil, "", nextErr
		}
		// Usage must be priced before it is published: the live event is the only
		// carrier of cost to clients, so pricing a copy after the fact leaves every
		// subscriber with a zero cost.
		if item.Type == protocol.EventUsage && item.Usage != nil {
			item.Usage.InputCost = float64(item.Usage.InputTokens) * model.InputPrice
			item.Usage.OutputCost = float64(item.Usage.OutputTokens) * model.OutputPrice
		}
		if r.live != nil {
			item.MessageID = assistant.ID
			r.live.PublishProtocol(r.dto.ID, item)
		}
		switch item.Type {
		case protocol.EventTextDelta:
			text.WriteString(item.Text)
		case protocol.EventReasoningDelta:
			reasoning.WriteString(item.Text)
		case protocol.EventReasoningSummaryDelta:
			reasoningSummary.Write(item.PartID, item.Text)
		case protocol.EventReasoningSummaryDone:
			if item.Text != "" {
				reasoningSummary.Set(item.PartID, item.Text)
			}
		case protocol.EventToolCallComplete:
			if item.ToolCall == nil {
				continue
			}
			calls = append(calls, completedCall{assistant.ID, *item.ToolCall})
		case protocol.EventUsage:
			if item.Usage != nil {
				usage = *item.Usage
			}
		case protocol.EventFinish:
			finish = item.FinishReason
		case protocol.EventProviderError:
			message := "provider error"
			kind := ""
			code := ""
			if item.ProviderError != nil {
				message = item.ProviderError.Message
				kind = item.ProviderError.Type
				code = item.ProviderError.Code
			}
			// Tool calls from a failed provider turn are not executed, so they must
			// not be retained without corresponding tool results.
			parts := finalParts(text.String(), preferredReasoning(reasoning.String(), reasoningSummary.String()), nil)
			finishErr := r.finishAssistantOnCleanup(assistant.ID, session.AssistantFinal{Parts: parts, Usage: usage, FinishReason: protocol.FinishError, Error: message, Status: "error"})
			overflow := item.ProviderError != nil && (canonicalOverflow(item.ProviderError.Type, item.ProviderError.Code) || overflowMessage(item.ProviderError.Message))
			responseErr := &provider.ResponseError{Type: kind, Code: code, Message: message}
			return nil, protocol.FinishError, &providerTurnFailure{err: errors.Join(responseErr, finishErr), code: code, overflow: overflow, retrySafe: finishErr == nil && text.Len() == 0 && reasoning.Len() == 0 && reasoningSummary.Len() == 0 && len(calls) == 0}
		}
	}
	parts := finalParts(text.String(), preferredReasoning(reasoning.String(), reasoningSummary.String()), calls)
	final := session.AssistantFinal{Parts: parts, Usage: usage, FinishReason: finish, Status: "complete"}
	if err := r.store.FinishAssistant(ctx, assistant.ID, final); err != nil {
		if ctx.Err() != nil {
			final.Status = "interrupted"
			final.FinishReason = protocol.FinishError
			final.Error = ctx.Err().Error()
			if cleanupErr := r.finishAssistantOnCleanup(assistant.ID, final); cleanupErr != nil {
				return nil, finish, errors.Join(err, cleanupErr)
			}
		}
		return nil, finish, err
	}
	r.mu.Lock()
	if r.turn != nil {
		r.turn.latestAssistantID = assistant.ID
	}
	r.mu.Unlock()
	if r.goals != nil {
		if _, err := r.goals.AccountUsage(ctx, r.dto.ID, usage); err != nil && !errors.Is(err, session.ErrGoalNotFound) {
			return nil, finish, err
		}
	}
	for _, call := range calls {
		if _, err := r.store.AddToolCall(ctx, assistant.ID, call.call); err != nil {
			return nil, finish, err
		}
	}
	return calls, finish, nil
}
func (r *agentSession) settleTool(ctx context.Context, callID, status string, result tool.Result, errorText string) error {
	tail, _ := result.Metadata["output_tail"].(string)
	return r.store.SettleToolWithOutput(ctx, callID, status, result.Text, errorText, tail)
}

func (r *agentSession) executeTools(ctx context.Context, selected session.AgentSessionDto, profile Profile, activeTools tool.Snapshot, calls []completedCall) error {
	executor := r.toolExecutor
	executor.Snapshot = activeTools
	statusQuery := r.statusQuery(selected, profile)
	sem := make(chan struct{}, r.config.MaxConcurrentTools)
	outcomes := make([]toolOutcome, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call completedCall) {
			defer wg.Done()
			// Record the call before any cancellable operation. This lets cleanup
			// settle and answer calls that were still waiting for the semaphore.
			outcomes[i].call = call
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				outcomes[i] = toolOutcome{call: call, err: ctx.Err(), interrupted: true}
				return
			}
			defer func() { <-sem }()
			if err := r.store.StartTool(ctx, call.call.ID); err != nil {
				outcomes[i] = toolOutcome{call: call, persistErr: err}
				return
			}
			var onPanic func(recovered any, stack []byte)
			if logger := r.toolPanicLogger; logger != nil {
				onPanic = func(recovered any, stack []byte) {
					logger(ctx, r.dto.ID, call.call.Name, recovered, stack)
				}
			}
			result, err := executeToolCall(ctx, executor, call, tool.CallContext{Workspace: r.workspace, Outputs: r.outputs, SessionID: r.dto.ID, Processes: r.processes, Agent: profile.ID(), ToolCallID: call.call.ID, Output: &toolOutputWriter{live: r.live, sessionID: r.dto.ID, callID: call.call.ID}, Displays: toolDisplayPublisher{live: r.live, sessionID: r.dto.ID, callID: call.call.ID}, SecurityProfile: r.securityProfile, StatusQuery: statusQuery, StatusProvider: newProfileInstructions(profile), Steer: r.steerSignal()}, onPanic)
			outcome := toolOutcome{call: call, text: result.Text, modelText: result.ModelText, err: err, interrupted: ctx.Err() != nil}
			status, errorText := "success", ""
			if outcome.interrupted {
				status, errorText = "interrupted", ctx.Err().Error()
			} else if err != nil {
				status, errorText = "failure", err.Error()
			}
			settleCtx := ctx
			var cancel context.CancelFunc
			if ctx.Err() != nil {
				settleCtx, cancel = context.WithTimeout(context.Background(), r.config.CleanupTimeout)
				defer cancel()
			}
			settleErr := r.settleTool(settleCtx, call.call.ID, status, result, errorText)
			outcome.settled = settleErr == nil
			outcome.persistErr = settleErr
			outcomes[i] = outcome
		}(i, call)
	}
	wg.Wait()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), r.config.CleanupTimeout)
		defer cancel()
		for i := range outcomes {
			if !outcomes[i].settled {
				outcomes[i].interrupted = true
				if outcomes[i].err == nil {
					outcomes[i].err = ctx.Err()
				}
				if err := r.store.SettleTool(cleanup, outcomes[i].call.call.ID, "interrupted", "", ctx.Err().Error()); err != nil {
					outcomes[i].persistErr = err
				} else {
					outcomes[i].settled = true
					outcomes[i].persistErr = nil
				}
			}
		}
		resultErr := error(ctx.Err())
		for _, outcome := range outcomes {
			if outcome.persistErr != nil {
				resultErr = errors.Join(resultErr, outcome.persistErr)
			}
			// Provider protocols require one result for every completed call. Use
			// the cleanup context so Ctrl-C cannot leave an orphaned function_call
			// that makes the provider reject the user's next prompt.
			_, err := r.store.AppendMessage(cleanup, toolResultMessage(outcome))
			if err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}
		return resultErr
	}
	for _, outcome := range outcomes {
		if outcome.persistErr != nil {
			return outcome.persistErr
		}
		if !outcome.settled {
			return errors.New("agent: tool call did not settle")
		}
	}
	for _, outcome := range outcomes {
		_, err := r.store.AppendMessage(ctx, toolResultMessage(outcome))
		if err != nil {
			return err
		}
	}
	return nil
}
func (r *agentSession) finishOnCleanup(messageID string, parts []protocol.ContentPart, finish protocol.FinishReason, errorText, status string) error {
	return r.finishAssistantOnCleanup(messageID, session.AssistantFinal{Parts: parts, FinishReason: finish, Error: errorText, Status: status})
}

func (r *agentSession) finishAssistantOnCleanup(messageID string, final session.AssistantFinal) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.config.CleanupTimeout)
	defer cancel()
	return r.store.FinishAssistant(ctx, messageID, final)
}
