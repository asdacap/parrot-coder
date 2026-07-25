package enhancedchat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	"github.com/amirulashraf/parrot-coder/internal/client"
	customcommand "github.com/amirulashraf/parrot-coder/internal/command"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

const (
	exitOK        = 0
	exitError     = 1
	exitInterrupt = 130
)

var errSecondInterrupt = errors.New("second interrupt")

type API interface {
	Runtime(context.Context) (v1.Runtime, error)
	Messages(context.Context, string) (v1.MessageList, error)
	Prompt(context.Context, string, v1.PromptRequest) (v1.PromptAccepted, error)
	Interrupt(context.Context, string) error
	Events(context.Context, string, *int64) (*client.EventStream, error)
	Permissions(context.Context, string) (v1.PermissionList, error)
	ReplyPermission(context.Context, string, string, v1.PermissionReply) error
	Questions(context.Context, string) (v1.QuestionList, error)
	ReplyQuestion(context.Context, string, string, v1.QuestionReply) error
}

type Expansion struct {
	Prompt, Agent, Model string
	Subtask              bool
}

type TurnComplete struct {
	Session   v1.Session
	Mode      string
	MessageID string
}

type TurnCompleteDialog struct {
	Markdown     string
	Prompt       string
	Context      []string
	Choices      []terminal.Candidate
	CustomChoice string
	CustomPrompt string
	Handle       func(string) (TurnCompleteResult, error)
}

type TurnCompleteResult struct {
	Prompt          string
	ValidationError string
}

type Config struct {
	Context    context.Context
	Interrupts <-chan os.Signal
	API        API
	CurrentAPI func() API
	// Presentation reports what the connected server's tools declared about
	// themselves. It is a function so a mid-session /connect is picked up.
	Presentation       func() chatview.Presentations
	Editor             *terminal.Editor
	Decoder            *terminal.KeyDecoder
	Renderer           *terminal.LiveRenderer
	Stderr             io.Writer
	Thinking           bool
	ThinkingEnabled    func() bool
	Current            func() v1.Session
	SetCurrent         func(v1.Session)
	Agent              func() string
	ModelName          func() string
	ModelineModelLabel func(int) string
	NextAgent          func(string) (string, error)
	ApplyAgent         func(string, bool) error
	SelectAgent        func(string) error
	PickModel          func() (string, error)
	ApplyModel         func(string) error
	SelectModel        func(string) error
	ChooseSession      func(string) (v1.Session, error)
	CreateSession      func(string, bool) (v1.Session, error)
	ResumeSession      func(string) error
	CommitUser         func(string) error
	CommitError        func(string)
	Expand             func(string, string) (Expansion, error)
	Slash              func(string, string) (bool, int)
	OnTurnComplete     func(TurnComplete) *TurnCompleteDialog
}

type Result struct {
	Code   int
	Reason string
	Err    error
}

type codingFlags struct{ thinking bool }
type chatSelection struct{ agent, model string }

func (s chatSelection) modelName() string { return s.model }
func (s chatSelection) modelLabel() string {
	if s.model == "" {
		return "no model"
	}
	return s.model
}

type chatShell struct {
	ctx       context.Context
	api       API
	current   v1.Session
	selection chatSelection
	options   codingFlags
	editor    *terminal.Editor
	decoder   *terminal.KeyDecoder
	renderer  *terminal.LiveRenderer
	stderr    io.Writer
	config    *Config
	result    Result
	commands  commandExpander
}

type commandExpander interface {
	Expand(string, string) (customcommand.Expansion, error)
}
type expansionAdapter struct {
	expand func(string, string) (Expansion, error)
}

func (a expansionAdapter) Expand(name, arguments string) (customcommand.Expansion, error) {
	x, err := a.expand(name, arguments)
	return customcommand.Expansion{Prompt: x.Prompt, Agent: x.Agent, Model: x.Model, Subtask: x.Subtask}, err
}

type interruptKey struct{}

func Run(config Config, first string) Result {
	s := &chatShell{ctx: config.Context, api: config.API, options: codingFlags{thinking: config.Thinking}, editor: config.Editor, decoder: config.Decoder, renderer: config.Renderer, stderr: config.Stderr, config: &config}
	if config.Current != nil {
		s.current = config.Current()
	}
	if config.Agent != nil {
		s.selection.agent = config.Agent()
	}
	if config.ModelName != nil {
		s.selection.model = config.ModelName()
	}
	if config.Expand != nil {
		s.commands = expansionAdapter{expand: config.Expand}
	}
	s.ctx = context.WithValue(s.ctx, interruptKey{}, config.Interrupts)
	s.ctx = context.WithValue(s.ctx, shellKey{}, s)
	s.runEnhanced(first)
	if config.SetCurrent != nil {
		config.SetCurrent(s.current)
	}
	return s.result
}

func exitWithReason(ctx context.Context, code int, reason string, err error) int {
	if shell, ok := ctx.Value(shellKey{}).(*chatShell); ok {
		shell.result = Result{Code: code, Reason: reason, Err: err}
	}
	return code
}

type shellKey struct{}

func (s *chatShell) modelineModelLabel(tokens int) string {
	if s.config != nil && s.config.ModelineModelLabel != nil {
		return s.config.ModelineModelLabel(tokens)
	}
	return s.selection.modelLabel()
}
func (s *chatShell) nextAgent(current string) (string, error) {
	if s.config == nil || s.config.NextAgent == nil {
		return current, nil
	}
	return s.config.NextAgent(current)
}
func (s *chatShell) applyAgent(agent string, announce bool) error {
	if s.config != nil && s.config.ApplyAgent != nil {
		if err := s.config.ApplyAgent(agent, announce); err != nil {
			return err
		}
	}
	s.selection.agent = agent
	return nil
}
func (s *chatShell) selectAgent(agent string) error {
	if s.config != nil && s.config.SelectAgent != nil {
		if err := s.config.SelectAgent(agent); err != nil {
			return err
		}
	}
	s.selection.agent = agent
	if s.config != nil && s.config.Agent != nil {
		s.selection.agent = s.config.Agent()
	}
	return nil
}
func (s *chatShell) pickModel() (string, error) {
	if s.config == nil || s.config.PickModel == nil {
		return "", terminal.ErrCanceled
	}
	return s.config.PickModel()
}
func (s *chatShell) applyModel(model string) error {
	if s.config != nil && s.config.ApplyModel != nil {
		if err := s.config.ApplyModel(model); err != nil {
			return err
		}
	}
	s.selection.model = model
	return nil
}
func (s *chatShell) selectModel(model string) error {
	if s.config != nil && s.config.SelectModel != nil {
		if err := s.config.SelectModel(model); err != nil {
			return err
		}
	}
	s.selection.model = model
	if s.config != nil && s.config.ModelName != nil {
		s.selection.model = s.config.ModelName()
	}
	return nil
}
func (s *chatShell) chooseSession(value string) (v1.Session, error) {
	if s.config == nil || s.config.ChooseSession == nil {
		return v1.Session{}, terminal.ErrCanceled
	}
	return s.config.ChooseSession(value)
}
func (s *chatShell) setCurrent(item v1.Session) {
	s.current = item
	s.selection = selectionFromSession(item, s.selection.agent)
	if s.config != nil && s.config.SetCurrent != nil {
		s.config.SetCurrent(item)
	}
}
func (s *chatShell) refreshState() {
	if s.config == nil {
		return
	}
	if s.config.CurrentAPI != nil {
		s.api = s.config.CurrentAPI()
	}
	if s.config.Current != nil {
		item := s.config.Current()
		s.current = item
		s.selection = selectionFromSession(item, s.selection.agent)
	}
	if s.config.Agent != nil {
		s.selection.agent = s.config.Agent()
	}
	if s.config.ModelName != nil {
		s.selection.model = s.config.ModelName()
	}
	if s.config.ThinkingEnabled != nil {
		s.options.thinking = s.config.ThinkingEnabled()
	}
}
func (s *chatShell) createSession(title string, forceNew bool) (v1.Session, error) {
	if s.config == nil || s.config.CreateSession == nil {
		return v1.Session{}, errors.New("session creation is unavailable")
	}
	return s.config.CreateSession(title, forceNew)
}
func (s *chatShell) commitUser(value string) error {
	if s.config != nil && s.config.CommitUser != nil {
		return s.config.CommitUser(value)
	}
	if s.renderer != nil {
		return s.renderer.CommitUserMessage("$ ", strings.TrimRight(value, "\r\n"))
	}
	return nil
}
func (s *chatShell) commitError(value string) {
	if s.config != nil && s.config.CommitError != nil {
		s.config.CommitError(value)
		return
	}
	if s.renderer != nil {
		_ = s.renderer.Commit("✗ Error: " + terminal.Sanitize(value))
	}
}
func (s *chatShell) commitNotice(value string) {
	if s.renderer != nil {
		_ = s.renderer.Commit(chatview.EventLine(0, "", "↻ "+terminal.Sanitize(value)))
	}
}
func (s *chatShell) slash(name, arguments string) (bool, int) {
	if s.config == nil || s.config.Slash == nil {
		return false, exitOK
	}
	return s.config.Slash(name, arguments)
}

type taskReport struct {
	id, taskID, sessionID, parentSessionID, line, block, blockKind string
	terminal, emitPlain, skip, mainStatus                          bool
	style                                                          terminal.TextStyle
}

// taskStreamTracker adapts the shared task tree tracker to the enhanced chat
// activity list. The tracker owns every parent-child task relationship; the
// enhanced runtime only renders the reports it returns.
type taskStreamTracker struct {
	tracker      *chatview.TaskTracker
	presentation chatview.Presentations
}

func (t *taskStreamTracker) Tracker() *chatview.TaskTracker {
	return t.tracker
}

// ensure builds the tracker on first use. The main task's usage is recorded
// before any task event may have arrived, so creation cannot wait for describe.
func (t *taskStreamTracker) ensure() *chatview.TaskTracker {
	if t.tracker == nil {
		t.tracker = chatview.NewTaskTracker()
		t.tracker.Presentation = t.presentation
	}
	return t.tracker
}

func (t *taskStreamTracker) addUsage(taskID string, usage v1.Usage) {
	t.ensure().AddUsage(taskID, usage)
}

func (t *taskStreamTracker) describe(item v1.Event, thinking bool) ([]taskReport, error) {
	reports, err := t.ensure().Apply(item, thinking)
	if err != nil {
		return nil, err
	}
	result := make([]taskReport, len(reports))
	for i, report := range reports {
		result[i] = taskReport{id: report.ID, taskID: report.TaskID, sessionID: report.SessionID, parentSessionID: report.ParentSessionID, line: report.Line, block: report.Block, blockKind: report.BlockKind, terminal: report.Terminal, emitPlain: report.EmitPlain, skip: report.Skip, mainStatus: report.MainStatus, style: report.Style}
	}
	return result, nil
}

func isTaskEvent(item v1.Event) bool { return chatview.IsTaskEvent(item) }

func chatExitReason(code int) string {
	if code == exitInterrupt {
		return "turn_interrupted"
	}
	return "chat_exited"
}
func permissionContextLines(item v1.Permission) []string {
	return chatview.PermissionContextLines(item)
}
func agentsLoadedActivities(item v1.Event) []string    { return chatview.AgentsLoadedActivities(item) }
func toolActivityStyle(name string) terminal.TextStyle { return chatview.ToolActivityStyle(name) }

// presentation reports what the connected server's tools declared. The zero
// value describes nothing, so callers fall back to the legacy rendering.
func (r *enhancedChatRuntime) presentation() chatview.Presentations {
	if r == nil || r.shell == nil || r.shell.config == nil || r.shell.config.Presentation == nil {
		return chatview.Presentations{}
	}
	return r.shell.config.Presentation()
}
func formatJSONAsYAML(input json.RawMessage) string { return chatview.FormatJSONAsYAML(input) }
func slashParts(line string) (string, string) {
	name, argument, _ := strings.Cut(line, " ")
	return name, strings.TrimSpace(argument)
}
func isBuiltinSlash(name string) bool {
	switch name {
	case "/help", "/version", "/run", "/chat", "/models", "/usage", "/model", "/effort", "/modes", "/mode", "/agents", "/agent", "/sessions", "/session", "/auth", "/serve", "/resume", "/new", "/clear", "/continue", "/compact", "/connect", "/thinking", "/goal", "/status", "/exit":
		return true
	default:
		return false
	}
}
func subtaskPrompt(expansion customcommand.Expansion) string {
	var request strings.Builder
	request.WriteString("Delegate the following work using agent_spawn")
	if expansion.Agent != "" {
		fmt.Fprintf(&request, " with agent %q", expansion.Agent)
	}
	if expansion.Model != "" {
		fmt.Fprintf(&request, " and model %q", expansion.Model)
	}
	request.WriteString(". Completion is reported automatically. If you need to block for the result, call wait_agent with the returned session_id, then relay its output.\n\n")
	request.WriteString(expansion.Prompt)
	return request.String()
}
func selectionFromSession(item v1.Session, fallback string) chatSelection {
	agent := item.Agent
	if agent == "" {
		agent = fallback
	}
	model := item.Model
	if item.Provider != "" && model != "" {
		model = item.Provider + "/" + model
	}
	return chatSelection{agent: agent, model: model}
}
func opaqueID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(value), nil
}
