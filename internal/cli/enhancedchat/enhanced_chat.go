package enhancedchat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	"github.com/amirulashraf/parrot-coder/internal/client"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
var errInvalidModalAnswer = errors.New("invalid modal answer")

const (
	maxToolBlockLines   = 10
	maxShellOutputLines = 10
	maxShellOutputBytes = 16 << 10
	permissionTimeout   = 2 * time.Minute
)

func isExecutionHaltKey(key terminal.Key) bool {
	return key.Kind == terminal.KeyEscape || key.Kind == terminal.KeyInterrupt
}

type enhancedKeyResult struct {
	key   terminal.Key
	err   error
	ack   chan struct{}
	epoch int64
}

type enhancedKeyPump struct {
	cancel context.CancelFunc
	done   chan struct{}
	events <-chan enhancedKeyResult
}

type enhancedInputMode struct {
	mu    sync.RWMutex
	epoch int64
}

func (m *enhancedInputMode) current() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.epoch
}

func (m *enhancedInputMode) advance() {
	m.mu.Lock()
	m.epoch++
	m.mu.Unlock()
}

func startEnhancedKeyPump(parent context.Context, decoder *terminal.KeyDecoder, modes ...*enhancedInputMode) *enhancedKeyPump {
	ctx, cancel := context.WithCancel(parent)
	events := make(chan enhancedKeyResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		for {
			key, err := decoder.ReadKey(ctx)
			epoch := int64(0)
			var mode *enhancedInputMode
			if len(modes) > 0 {
				mode = modes[0]
			}
			if mode != nil {
				mode.mu.RLock()
				epoch = mode.epoch
			}
			ack := make(chan struct{})
			select {
			case events <- enhancedKeyResult{key: key, err: err, ack: ack, epoch: epoch}:
			case <-ctx.Done():
				if mode != nil {
					mode.mu.RUnlock()
				}
				return
			}
			if mode != nil {
				mode.mu.RUnlock()
			}
			select {
			case <-ack:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return &enhancedKeyPump{cancel: cancel, done: done, events: events}
}

func (p *enhancedKeyPump) stop() {
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
}

type enhancedSessionEvent struct {
	generation int
	event      v1.Event
	err        error
}

type queuedChatInput struct {
	inputID   string
	messageID string
	content   string
}

type enhancedActivityItem struct {
	id               string
	taskID           string
	sessionID        string
	parentSessionID  string
	mainStatus       bool
	messageID        string
	label            string
	toolName         string
	style            terminal.TextStyle
	input            map[string]any
	error            string
	status           string
	tokens           int
	hasUsage         bool
	outputTokens     int
	reasoningTokens  int
	toolUses         int
	started          time.Time
	ended            time.Time
	terminal         bool
	hidden           bool
	reasoning        bool
	reasoningSummary bool
	block            string
	rendered         string
	output           shellOutputTail
}

type shellOutputTail struct {
	lines    []string
	pending  []rune
	carriage bool
}

func (t *shellOutputTail) Write(delta string) {
	for _, char := range delta {
		if t.carriage {
			t.carriage = false
			if char != '\n' {
				t.pending = t.pending[:0]
			}
		}
		switch char {
		case '\n':
			t.lines = append(t.lines, string(t.pending))
			t.pending = t.pending[:0]
		case '\r':
			t.carriage = true
		default:
			t.pending = append(t.pending, char)
			if len(t.pending) > maxShellOutputBytes {
				t.pending = t.pending[len(t.pending)-maxShellOutputBytes:]
			}
		}
	}
	if len(t.lines) > maxShellOutputLines {
		t.lines = t.lines[len(t.lines)-maxShellOutputLines:]
	}
}

func (t shellOutputTail) String() string {
	lines := append([]string(nil), t.lines...)
	if len(t.pending) != 0 {
		lines = append(lines, string(t.pending))
	}
	if len(lines) > maxShellOutputLines {
		lines = lines[len(lines)-maxShellOutputLines:]
	}
	return strings.Join(lines, "\n")
}

type enhancedModal struct {
	kind            string
	prompt          string
	context         []string
	state           *terminal.EditorState
	permission      *v1.Permission
	question        *v1.QuestionRequest
	turnComplete    *TurnCompleteDialog
	index           int
	selected        int
	choices         []terminal.Candidate
	answers         []v1.Answer
	customInput     bool
	selectedOptions map[string]bool
	createdAt       time.Time
}

type enhancedInputOutcome struct {
	exit   bool
	code   int
	retain bool
}

type enhancedChatRuntime struct {
	shell *chatShell
	state *terminal.EditorState

	busy              bool
	idleSeen          bool
	status            string
	spinner           int
	interruptCount    int
	toolInput         bool
	streamed          strings.Builder
	reasoningText     strings.Builder
	reasoningSummary  bool
	reasoningParts    map[string]string
	streamMessageID   string
	pending           []queuedChatInput
	modal             *enhancedModal
	inputMode         enhancedInputMode
	knownMessages     map[string]bool
	unsyncedMessages  map[string]bool
	activity          []enhancedActivityItem
	completedTools    []enhancedActivityItem
	turnCompleteID    string
	lastCompleteID    string
	borderCommitted   bool
	contextTokens     int
	mainTaskUsage     chatview.TaskUsage
	subagents         taskStreamTracker
	pendingToolOutput map[string]shellOutputTail
	completedToolIDs  map[string]bool

	stream           *client.EventStream
	streamSessionID  string
	eventSessionID   string
	eventAfter       int64
	streamGeneration int
	streamDone       chan struct{}
	streamExited     chan struct{}
	events           chan enhancedSessionEvent
}

func (s *chatShell) runEnhanced(first string) int {
	state, err := s.editor.Start("")
	if err != nil {
		s.commitError(err.Error())
		return exitWithReason(s.ctx, exitError, "enhanced_editor_start_failed", err)
	}
	runtime := &enhancedChatRuntime{
		shell: s, state: state, knownMessages: make(map[string]bool), unsyncedMessages: make(map[string]bool),
		events: make(chan enhancedSessionEvent, 128),
	}
	defer runtime.stopStream()

	if first != "" {
		outcome := runtime.handleInput(first)
		if outcome.retain {
			_ = state.Reset(first)
		}
		if outcome.exit {
			return exitWithReason(s.ctx, outcome.code, chatExitReason(outcome.code), nil)
		}
	}
	if err := runtime.render(); err != nil {
		return s.enhancedRenderError(err)
	}

	pump := startEnhancedKeyPump(s.ctx, s.decoder, &runtime.inputMode)
	defer func() { pump.stop() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	interrupts, _ := s.ctx.Value(interruptKey{}).(<-chan os.Signal)
	ticks := 0

	for {
		select {
		case <-s.ctx.Done():
			return exitWithReason(s.ctx, exitInterrupt, "chat_context_canceled", s.ctx.Err())
		case <-interrupts:
			if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
				return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
			}
			_ = runtime.render()
		case result, ok := <-pump.events:
			pumpStopped := false
			if !ok {
				return exitWithReason(s.ctx, exitInterrupt, "chat_input_stopped", nil)
			}
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) {
					continue
				}
				runtime.commitError(result.err.Error())
				return exitWithReason(s.ctx, exitError, "chat_input_failed", result.err)
			}
			if result.key.Kind == terminal.KeyModeSwitch {
				close(result.ack)
				if err := runtime.cycleMode(); err != nil {
					runtime.status = err.Error()
				}
				if err := runtime.render(); err != nil {
					return s.enhancedRenderError(err)
				}
				continue
			}
			if runtime.busy && isExecutionHaltKey(result.key) {
				close(result.ack)
				runtime.cancelModal()
				if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
					return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
				}
				if err := runtime.render(); err != nil {
					return s.enhancedRenderError(err)
				}
				continue
			}

			targetState := state
			modalAction := false
			if runtime.modal != nil && result.epoch == runtime.inputMode.current() {
				targetState = runtime.modal.state
				modalAction = true
			}
			if modalAction && runtime.modal.kind == "permission" && !runtime.modal.customInput {
				done, err := runtime.handlePermissionModalKey(result.key)
				close(result.ack)
				if done {
					pump.stop()
					pumpStopped = true
				}
				if err != nil {
					if errors.Is(err, errSecondInterrupt) {
						return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
					}
					runtime.commitError(err.Error())
					runtime.cancelModal()
				}
				if !done {
					// Ignore text-editing keys; the permission prompt is a selection.
				}
				if err := runtime.render(); err != nil {
					return s.enhancedRenderError(err)
				}
				if pumpStopped {
					pump = startEnhancedKeyPump(s.ctx, s.decoder, &runtime.inputMode)
				}
				continue
			}
			if modalAction && runtime.modal.kind == "question" && len(runtime.modal.choices) > 0 {
				handled, err := runtime.handleQuestionModalKey(result.key)
				if handled {
					close(result.ack)
					if err != nil {
						if errors.Is(err, errInvalidModalAnswer) {
							runtime.status = err.Error()
						} else {
							runtime.commitError(err.Error())
							runtime.cancelModal()
						}
					}
					if err := runtime.render(); err != nil {
						return s.enhancedRenderError(err)
					}
					continue
				}
			}
			if modalAction && runtime.modal.kind == "turn_complete" && len(runtime.modal.choices) > 0 {
				handled, err := runtime.handleTurnCompleteModalKey(result.key)
				if handled {
					close(result.ack)
					if err != nil {
						if errors.Is(err, errInvalidModalAnswer) {
							runtime.status = err.Error()
						} else {
							runtime.commitError(err.Error())
							runtime.cancelModal()
						}
					}
					if err := runtime.render(); err != nil {
						return s.enhancedRenderError(err)
					}
					continue
				}
			}
			action := targetState.Handle(result.key)
			if !action.Done {
				close(result.ack)
				if err := runtime.render(); err != nil {
					return s.enhancedRenderError(err)
				}
				continue
			}

			pump.stop()
			outcome := enhancedInputOutcome{}
			if modalAction {
				switch {
				case errors.Is(action.Err, terminal.ErrInterrupted):
					runtime.cancelModal()
					if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
						return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
					}
				case errors.Is(action.Err, terminal.ErrCanceled), errors.Is(action.Err, io.EOF):
					runtime.cancelModal()
				case action.Err != nil:
					runtime.commitError(action.Err.Error())
					runtime.cancelModal()
				default:
					if err := runtime.answerModal(action.Value); err != nil {
						if errors.Is(err, errInvalidModalAnswer) {
							runtime.status = err.Error()
							_ = runtime.modal.state.Reset("")
						} else {
							runtime.commitError(err.Error())
							runtime.cancelModal()
						}
					}
				}
			} else {
				switch {
				case errors.Is(action.Err, terminal.ErrInterrupted):
					if state.Value() != "" {
						_ = state.Reset("")
					} else if runtime.busy {
						if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
							return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
						}
					}
				case errors.Is(action.Err, terminal.ErrCanceled):
					_ = state.Reset("")
				case errors.Is(action.Err, io.EOF):
					if !runtime.busy {
						return exitWithReason(s.ctx, exitOK, "chat_input_closed", nil)
					}
					runtime.status = "agent is still working"
				case action.Err != nil:
					runtime.commitError(action.Err.Error())
					_ = runtime.ensureInputBorder()
					_ = state.Reset("")
				case strings.TrimSpace(action.Value) != "":
					outcome = runtime.handleInput(action.Value)
					if !outcome.retain {
						_ = state.Reset("")
					}
				default:
					_ = state.Reset("")
				}
			}
			if outcome.exit {
				return exitWithReason(s.ctx, outcome.code, chatExitReason(outcome.code), nil)
			}
			pump = startEnhancedKeyPump(s.ctx, s.decoder, &runtime.inputMode)
			if err := runtime.render(); err != nil {
				return s.enhancedRenderError(err)
			}
		case result := <-runtime.events:
			if result.generation != runtime.streamGeneration {
				continue
			}
			if result.err != nil {
				runtime.status = "event stream closed"
				runtime.stopStream()
			} else if err := runtime.handleEvent(result.event); err != nil {
				runtime.commitError(err.Error())
				_ = runtime.ensureInputBorder()
				runtime.stopStream()
			} else if result.event.Sequence != nil {
				runtime.eventAfter = *result.event.Sequence
			}
			if err := runtime.render(); err != nil {
				return s.enhancedRenderError(err)
			}
		case <-ticker.C:
			ticks++
			if runtime.busy {
				runtime.spinner = (runtime.spinner + 1) % len(spinnerFrames)
			}
			if runtime.busy && ticks%5 == 0 {
				runtime.detectModal()
			}
			if runtime.busy && ticks%10 == 0 {
				var err error
				if runtime.stream == nil {
					err = runtime.ensureStream(s.current.ID)
				} else {
					err = runtime.reconcileRuntime()
				}
				if err != nil {
					runtime.status = "runtime reconciliation failed"
				}
			}
			if runtime.modal != nil && runtime.modal.kind == "permission" && !runtime.modal.createdAt.IsZero() && time.Since(runtime.modal.createdAt) > permissionTimeout {
				runtime.timeoutModal()
			}
			if err := runtime.render(); err != nil {
				return s.enhancedRenderError(err)
			}
		}
	}
}

func (s *chatShell) enhancedRenderError(err error) int {
	// The renderer is the normal enhanced-chat error surface. If it failed,
	// preserve its last live frame and write the failure directly to stderr.
	// chatCommand deliberately skips live-buffer cleanup for this error exit.
	fmt.Fprintln(s.stderr, "parrot: enhanced chat render failed:", terminal.Sanitize(err.Error()))
	reason := "enhanced_render_failed"
	if class := terminal.RenderErrorClass(err); class != "" {
		reason += "_" + class
	}
	return exitWithReason(s.ctx, exitError, reason, err)
}

func (r *enhancedChatRuntime) render() error {
	now := time.Now()
	pending := make([]string, len(r.pending))
	for i, item := range r.pending {
		pending[i] = item.content
	}
	message := r.streamed.String()
	var stream *terminal.StreamMessage
	if message != "" && r.streamMessageID != "" {
		stream = &terminal.StreamMessage{ID: r.streamMessageID, Prefix: "● ", Text: message}
	}
	prompt := r.state.PromptState()
	if r.modal != nil {
		prompt = r.modal.state.PromptState()
		prompt.Prefix = r.modal.prompt
		if len(r.modal.choices) > 0 {
			prompt.Completions = r.modal.choices
			prompt.Selected = r.modal.selected
		}
		if len(r.modal.choices) > 0 {
			prompt.Text = ""
			prompt.Cursor = 0
		}
	}
	busy := r.busy
	if r.modal != nil && r.modal.kind == "permission" {
		busy = false
	}
	right := r.shell.modelineModelLabel(r.contextTokens)
	if tokens := formatTaskTokenUsage(r.mainTaskUsage); tokens != "-" {
		right += " · " + tokens
	}
	if r.mainTaskUsage.Cost > 0 {
		right += " · " + formatCost(r.mainTaskUsage.Cost)
	}
	frames := r.activityFrames(now, r.shell.renderer.Columns())
	frames = append(frames, terminal.LiveFrame{
		TaskID: managedtask.MainTaskID, MainStatus: true,
		Stream: stream, PromptContext: r.modalContext(), Pending: pending,
		InputLeft: r.inputModeLabel(), InputCenter: r.modelineActivity(now), InputRight: right,
		Prompt: prompt, Busy: busy, Spinner: spinnerFrames[r.spinner],
		ShowDivider: r.modal != nil || message != "" || len(r.activity) > 0 || !r.borderCommitted,
	})
	return r.shell.renderer.Frames(frames)
}
