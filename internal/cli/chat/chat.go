package chat

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/app"
	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	"github.com/amirulashraf/parrot-coder/internal/cli/enhancedchat"
	"github.com/amirulashraf/parrot-coder/internal/client"
	customcommand "github.com/amirulashraf/parrot-coder/internal/command"
	configpkg "github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/mode"
	"github.com/amirulashraf/parrot-coder/internal/processidentity"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

const (
	exitOK        = 0
	exitError     = 1
	exitUsage     = 2
	exitInterrupt = 130
)

type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

type Config struct {
	Context           context.Context
	Interrupts        <-chan os.Signal
	Args              []string
	Stdin             io.Reader
	Stdout, Stderr    io.Writer
	NoColor           bool
	Build             BuildInfo
	Open              func(context.Context, app.Options) (*app.App, error)
	EnableRawMode     func(*os.File) (*terminal.RawState, error)
	SetBracketedPaste func(io.Writer, bool) error
}
type Result struct {
	Code   int
	Reason string
	Err    error
}
type resultState struct{ result Result }
type resultKey struct{}
type interruptKey struct{}

var errSecondInterrupt = errors.New("second interrupt")

func interruptChannel(ctx context.Context) <-chan os.Signal {
	value, _ := ctx.Value(interruptKey{}).(<-chan os.Signal)
	return value
}
func isExecutionHaltKey(key terminal.Key) bool {
	return key.Kind == terminal.KeyEscape || key.Kind == terminal.KeyInterrupt
}
func normalizeLeadingPrompt(args []string) []string {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") {
		return args
	}
	return append(append([]string(nil), args[1:]...), args[0])
}
func opaqueID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(value), nil
}
func writeJSONLine(output io.Writer, item v1.Event) error {
	return json.NewEncoder(output).Encode(item)
}

// jsonlRedactor blanks the transcript export for tools which declared they
// have no displayable output, so a redacted field never reaches a file.
type jsonlRedactor struct {
	presentation chatview.Presentations
	tools        map[string]string
}

// suppressed reports whether a tool declared that its output must not be shown.
func (r *jsonlRedactor) suppressed(name string) bool {
	return name != "" && r.presentation.Output(name) == chatview.ToolOutputNone
}

func (r *jsonlRedactor) redact(item v1.Event) v1.Event {
	if item.Type == v1.EventPermission {
		var request v1.Permission
		if json.Unmarshal(item.Data, &request) == nil && r.suppressed(request.ToolID) {
			request.Description = request.ToolID
			var input map[string]any
			if json.Unmarshal(request.CanonicalInput, &input) == nil {
				request.CanonicalInput, _ = json.Marshal(r.presentation.Redact(request.ToolID, input))
			}
			request.Review = json.RawMessage(`{"redacted":true}`)
			item.Data, _ = json.Marshal(request)
		}
		return item
	}
	if strings.HasPrefix(item.Type, "session.tool.") {
		callID, name, input, _ := r.presentation.Payload(item.Data)
		if r.tools == nil {
			r.tools = make(map[string]string)
		}
		if r.suppressed(name) && callID != "" {
			r.tools[callID] = name
		}
		effectiveName := name
		if effectiveName == "" {
			effectiveName = r.tools[callID]
		}
		if r.suppressed(effectiveName) {
			var raw map[string]any
			if json.Unmarshal(item.Data, &raw) == nil {
				if input != nil {
					redactToolEventInput(raw, input)
				}
				for _, key := range []string{"result", "Result", "error", "error_message", "message", "output_tail"} {
					if _, exists := raw[key]; exists {
						raw[key] = "<redacted>"
					}
				}
				item.Data, _ = json.Marshal(raw)
			}
		}
		return item
	}
	if item.Type == v1.EventMessagePartDelta {
		var delta v1.MessagePartDelta
		if json.Unmarshal(item.Data, &delta) == nil && delta.Kind == "tool_input" {
			if r.suppressed(delta.ToolName) {
				if r.tools == nil {
					r.tools = make(map[string]string)
				}
				r.tools[delta.ToolCallID] = delta.ToolName
			}
			if r.suppressed(r.tools[delta.ToolCallID]) {
				delta.Delta = "<redacted>"
				item.Data, _ = json.Marshal(delta)
			}
		}
	}
	if item.Type == v1.EventToolOutputDelta {
		var delta v1.ToolOutputDelta
		if json.Unmarshal(item.Data, &delta) == nil && r.suppressed(r.tools[delta.ToolCallID]) {
			delta.Delta = "<redacted>"
			item.Data, _ = json.Marshal(delta)
		}
	}
	return item
}

func redactToolEventInput(raw map[string]any, input map[string]any) {
	for _, key := range []string{"input", "Input", "arguments", "Arguments"} {
		if _, exists := raw[key]; exists {
			raw[key] = input
		}
	}
	if call, ok := raw["call"].(map[string]any); ok {
		for _, key := range []string{"input", "Input", "arguments", "Arguments"} {
			if _, exists := call[key]; exists {
				call[key] = input
			}
		}
	}
}
func formatTokenCount(tokens int) string { return chatview.FormatTokenCount(tokens) }
func cleanupEnhancedRenderer(renderer *terminal.LiveRenderer, code int) {
	if renderer != nil && (code == exitOK || code == exitInterrupt) {
		_ = renderer.Close()
	}
}

func finish(ctx context.Context, code int, reason string, err error) int {
	if state, ok := ctx.Value(resultKey{}).(*resultState); ok {
		state.result = Result{code, reason, err}
	}
	return code
}
func Run(config Config) Result {
	state := &resultState{}
	ctx := context.WithValue(config.Context, resultKey{}, state)
	ctx = context.WithValue(ctx, interruptKey{}, config.Interrupts)
	command(ctx, config)
	if state.result.Reason == "" {
		state.result = Result{Code: exitOK, Reason: "chat_exited"}
	}
	return state.result
}

type codingFlags struct {
	continued                      bool
	session, model, variant, agent string
	thinking                       bool
}

func addCodingFlags(fs *flag.FlagSet, options *codingFlags) {
	fs.BoolVar(&options.continued, "continue", false, "continue the most recent session")
	fs.StringVar(&options.session, "session", "", "continue a session ID")
	fs.StringVar(&options.model, "model", "", "select provider/model")
	fs.StringVar(&options.agent, "mode", "", "select a foreground mode")
	fs.StringVar(&options.agent, "agent", "", "deprecated alias for --mode")
	fs.StringVar(&options.variant, "variant", "", "select a model reasoning variant")
	fs.BoolVar(&options.thinking, "thinking", false, "show reasoning status")
}

type PromptConfig struct {
	Context        context.Context
	Interrupts     <-chan os.Signal
	Args           []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Build          BuildInfo
	Open           func(context.Context, app.Options) (*app.App, error)
}

func RunPrompt(config PromptConfig) Result {
	state := &resultState{}
	ctx := context.WithValue(config.Context, resultKey{}, state)
	ctx = context.WithValue(ctx, interruptKey{}, config.Interrupts)
	promptCommand(ctx, config)
	if state.result.Reason == "" {
		state.result = Result{Code: exitOK, Reason: "turn_completed"}
	}
	return state.result
}

func promptCommand(ctx context.Context, config PromptConfig) int {
	fs := newFlagSet("run", config.Stderr)
	args := normalizeLeadingPrompt(config.Args)
	var options codingFlags
	var format string
	var interactive bool
	addCodingFlags(fs, &options)
	fs.StringVar(&format, "format", "text", "output format: text or jsonl")
	fs.BoolVar(&interactive, "interactive-prompts", false, "answer prompts from the controlling terminal")
	if err := fs.Parse(args); err != nil {
		return finish(ctx, flagCode(err), flagReason(err), nil)
	}
	if options.continued && options.session != "" || fs.NArg() > 1 || format != "text" && format != "jsonl" {
		fmt.Fprintln(config.Stderr, "invalid run flags; see parrot run --help")
		return finish(ctx, exitUsage, "invalid_run_arguments", nil)
	}
	prompt, err := promptInput(config.Stdin, fs.Args())
	if err != nil {
		fmt.Fprintln(config.Stderr, err)
		return finish(ctx, exitError, "prompt_read_failed", err)
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(config.Stderr, "run requires a prompt argument or stdin data")
		return finish(ctx, exitUsage, "prompt_required", nil)
	}
	var tty io.ReadCloser
	if interactive {
		file, openErr := terminal.OpenInput()
		if openErr != nil {
			fmt.Fprintln(config.Stderr, "interactive prompts require /dev/tty:", openErr)
			return finish(ctx, exitError, "interactive_terminal_open_failed", openErr)
		}
		tty = file
		defer tty.Close()
	}
	runtime, err := config.Open(ctx, app.Options{Version: config.Build.Version, Model: options.model, Variant: options.variant, Agent: options.agent, NonInteractive: !interactive})
	if err != nil {
		fmt.Fprintln(config.Stderr, err)
		return finish(ctx, exitError, appOpenReason(err), err)
	}
	defer runtime.Close()
	writeAgentsStartupActivity(config.Stderr, runtime.AgentsFiles)
	sessionItem, err := chooseSession(ctx, runtime.Client, runtime.Project.ID, options.continued, options.session, prompt)
	if err != nil {
		fmt.Fprintln(config.Stderr, err)
		return finish(ctx, exitError, "session_selection_failed", err)
	}
	if err := applySelection(ctx, runtime.Client, sessionItem.ID, options.agent, options.model, optionalVariant(options.variant)); err != nil {
		fmt.Fprintln(config.Stderr, err)
		return finish(ctx, exitError, "selection_update_failed", err)
	}
	result := streamTurn(ctx, runtime.Client, sessionItem.ID, prompt, streamOptions{format: format, stdout: config.Stdout, stderr: config.Stderr, promptInput: tty, thinking: options.thinking})
	if result.err != nil {
		if errors.Is(result.err, context.Canceled) {
			return finish(ctx, exitInterrupt, "turn_interrupted", result.err)
		}
		fmt.Fprintln(config.Stderr, result.err)
		return finish(ctx, exitError, "session_turn_failed", result.err)
	}
	return finish(ctx, exitOK, "turn_completed", nil)
}

func promptInput(stdin io.Reader, arguments []string) (string, error) {
	argument := ""
	if len(arguments) == 1 {
		argument = arguments[0]
	}
	var piped string
	if !terminal.IsTTY(stdin) {
		data, err := io.ReadAll(io.LimitReader(stdin, 16<<20+1))
		if err != nil {
			return "", fmt.Errorf("read prompt: %w", err)
		}
		if len(data) > 16<<20 {
			return "", errors.New("prompt exceeds 16 MiB")
		}
		piped = string(data)
	}
	if argument != "" && piped != "" {
		return argument + "\n" + piped, nil
	}
	return argument + piped, nil
}

func command(ctx context.Context, config Config) int {
	args, stdin, stdout, stderr, noColor := config.Args, config.Stdin, config.Stdout, config.Stderr, config.NoColor
	fs := newFlagSet("chat", stderr)
	args = normalizeLeadingPrompt(args)
	var options codingFlags
	addCodingFlags(fs, &options)
	if err := fs.Parse(args); err != nil {
		return finish(ctx, flagCode(err), flagReason(err), nil)
	}
	if options.continued && options.session != "" || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "invalid chat flags; see parrot chat --help")
		return finish(ctx, exitUsage, "invalid_chat_arguments", nil)
	}
	runtime, err := config.Open(ctx, app.Options{Version: config.Build.Version, Model: options.model, Variant: options.variant, Agent: options.agent, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return finish(ctx, exitError, appOpenReason(err), err)
	}
	defer runtime.Close()
	api := apiClient(runtime.Client)
	models, err := api.Models(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return finish(ctx, exitError, "model_list_failed", err)
	}
	// A server which does not serve tool presentation yields an empty table,
	// which renders through the generic fallback rather than failing startup.
	toolList, _ := api.Tools(ctx)
	var current v1.Session
	if options.continued || options.session != "" {
		current, err = chooseSession(ctx, api, runtime.Project.ID, options.continued, options.session, "")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return finish(ctx, exitError, "session_selection_failed", err)
		}
		if err := applySelection(ctx, api, current.ID, options.agent, options.model, optionalVariant(options.variant)); err != nil {
			fmt.Fprintln(stderr, err)
			return finish(ctx, exitError, "selection_update_failed", err)
		}
		if options.agent != "" || options.model != "" || options.variant != "" {
			current, err = api.Session(ctx, current.ID)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return finish(ctx, exitError, "session_refresh_failed", err)
			}
		}
	}
	selection := defaultChatSelection(runtime.DefaultSelection, options.variant)
	identity, identityErr := processidentity.Load(runtime.Paths.State)
	if identityErr != nil {
		fmt.Fprintln(stderr, identityErr)
		return finish(ctx, exitError, "process_identity_failed", identityErr)
	}
	claimRequest := v1.ClaimSessionRequest{WorkingDirectory: runtime.WorkingDirectory, HostKey: identity.HostKey, PID: identity.PID, ProjectID: runtime.Project.ID}
	if current.ID == "" && selection.modelName() != "" {
		claimRequest.Agent, claimRequest.Model, claimRequest.Variant = selection.agent, selection.modelName(), &selection.variant
		claimed, claimErr := runtime.Client.ClaimSession(ctx, claimRequest)
		if claimErr != nil {
			fmt.Fprintln(stderr, claimErr)
			return finish(ctx, exitError, "session_claim_failed", claimErr)
		}
		current = claimed.Session
		if err := applySelection(ctx, api, current.ID, options.agent, options.model, optionalVariant(options.variant)); err != nil {
			fmt.Fprintln(stderr, err)
			return finish(ctx, exitError, "selection_update_failed", err)
		}
		if options.agent != "" || options.model != "" || options.variant != "" {
			current, err = api.Session(ctx, current.ID)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return finish(ctx, exitError, "session_refresh_failed", err)
			}
		}
	}
	if current.ID != "" {
		selection = selectionFromSession(current, selection.agent)
	}
	plainOut := terminal.Writer{W: stdout}
	shell := &chatShell{
		ctx: ctx, api: api, current: current, selection: selection, options: options,
		projectID: runtime.Project.ID, projectRoot: runtime.Project.Root, configDir: runtime.Paths.Config, claimRequest: claimRequest, commands: runtime.Commands,
		build: config.Build, credentials: runtime.Credentials, handler: runtime.Handler,
		reloadProviders: func(ctx context.Context) error { return runtime.ReloadProviders(ctx) },
		models:          models.Items, presentation: chatview.NewPresentations(toolList),
		stdout: plainOut, stderr: stderr, inputTTY: terminal.IsTTY(stdin), outputTTY: terminal.IsTTY(stdout),
		inputEcho: terminal.InputEchoed(stdin, stdout), columns: terminal.Columns(stdout),
	}
	defer shell.close()
	shell.refreshModelInfo()
	if inputFile, ok := stdin.(*os.File); ok && terminal.IsTTY(inputFile) && terminal.IsTTY(stdout) && os.Getenv("TERM") != "dumb" {
		raw, rawErr := config.EnableRawMode(inputFile)
		if rawErr != nil {
			fmt.Fprintln(stderr, "enhanced terminal unavailable; using plain input:", rawErr)
		} else {
			if err := config.SetBracketedPaste(stdout, true); err != nil {
				fmt.Fprintln(stderr, "enhanced terminal unavailable; using plain input:", err)
				_ = raw.Close()
			} else {
				defer config.SetBracketedPaste(stdout, false)
				defer raw.Close()
				shell.enhanced = true
				shell.stdout = stdout
				shell.renderer = terminal.NewLiveRenderer(stdout, terminal.RendererConfig{
					TTY: true, Color: terminal.ColorEnabled(stdout, noColor), Columns: terminal.Columns(stdout), MaxRows: terminal.DefaultLiveRows, MaxInputRows: 12,
					InlineDiff:  runtime.Config.Config.InlineDiff,
					ColumnsFunc: func() int { return terminal.Columns(stdout) },
				})
				shell.decoder = terminal.NewKeyDecoder(inputFile)
				shell.decoder.SetOversizedPasteHandler(func(ctx context.Context, reader io.Reader) (string, error) {
					if shell.api != runtime.Client {
						return "", errors.New("oversized paste storage is unavailable for remote connections")
					}
					stored, err := runtime.StoreOutput(ctx, reader)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("[Pasted content stored as output %s; use read_output to read it.]", stored.ID), nil
				})
				shell.editor = terminal.NewEditorDecoder(shell.decoder, stdout,
					terminal.WithCompletions(chatCompletionCandidates(runtime.Commands)),
					terminal.WithEditorRenderer(shell.renderer))
			}
		}
	}
	if !shell.enhanced {
		shell.reader = bufio.NewReader(stdin)
	}
	first := ""
	if fs.NArg() == 1 {
		first = fs.Arg(0)
	}
	if shell.enhanced {
		code := shell.runEnhanced(first)
		cleanupEnhancedRenderer(shell.renderer, code)
		return code
	}
	return shell.run(first)
}

type apiClient interface {
	Runtime(context.Context) (v1.Runtime, error)
	Sessions(context.Context) (v1.SessionList, error)
	CreateSession(context.Context, v1.CreateSessionRequest) (v1.Session, error)
	UpdateSessionSelection(context.Context, string, v1.UpdateSessionSelectionRequest) (v1.SessionSelection, error)
	Session(context.Context, string) (v1.Session, error)
	DeleteSession(context.Context, string) error
	Messages(context.Context, string) (v1.MessageList, error)
	Prompt(context.Context, string, v1.PromptRequest) (v1.PromptAccepted, error)
	Interrupt(context.Context, string) error
	Events(context.Context, string, *int64) (*client.EventStream, error)
	Permissions(context.Context, string) (v1.PermissionList, error)
	ReplyPermission(context.Context, string, string, v1.PermissionReply) error
	Questions(context.Context, string) (v1.QuestionList, error)
	ReplyQuestion(context.Context, string, string, v1.QuestionReply) error
	Models(context.Context) (v1.ModelList, error)
	ModelInfo(context.Context, string, string) (v1.Model, error)
	SubscriptionUsage(context.Context) (v1.SubscriptionUsage, error)
	Agents(context.Context) (v1.AgentList, error)
	Tools(context.Context) (v1.ToolList, error)
}

type goalClient interface {
	Goal(context.Context, string) (v1.Goal, error)
	PutGoal(context.Context, string, v1.PutGoalRequest) (v1.Goal, error)
	DeleteGoal(context.Context, string) error
}

type sessionClaimer interface {
	ClaimSession(context.Context, v1.ClaimSessionRequest) (v1.ClaimSessionResponse, error)
}

type resumableClient interface {
	apiClient
	Resume(context.Context, string) error
}

// applySelection patches a session's selection. A nil variant leaves the
// current one alone; a non-nil empty variant clears it, which is what switching
// to a model without reasoning variants requires.
func applySelection(ctx context.Context, api apiClient, sessionID, agentID, model string, variant *string) error {
	if agentID == "" && model == "" && variant == nil {
		return nil
	}
	request := v1.UpdateSessionSelectionRequest{Agent: agentID, Model: model, Variant: variant}
	_, err := api.UpdateSessionSelection(ctx, sessionID, request)
	return err
}

// optionalVariant treats an unset command-line variant as "leave unchanged".
func optionalVariant(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func chooseSession(ctx context.Context, api apiClient, projectID string, continued bool, sessionID, title string) (v1.Session, error) {
	if sessionID != "" {
		return api.Session(ctx, sessionID)
	}
	if continued {
		items, err := api.Sessions(ctx)
		if err != nil {
			return v1.Session{}, err
		}
		for _, item := range items.Items {
			if item.ProjectID == projectID {
				return item, nil
			}
		}
		return v1.Session{}, errors.New("no previous session exists for this project")
	}
	line, _, _ := strings.Cut(strings.TrimSpace(title), "\n")
	if len(line) > 80 {
		line = line[:80]
	}
	return api.CreateSession(ctx, v1.CreateSessionRequest{ProjectID: projectID, Title: line})
}

type streamOptions struct {
	format      string
	stdout      io.Writer
	stderr      io.Writer
	promptInput io.Reader
	thinking    bool
	chat        bool
	resume      bool
	renderer    *terminal.LiveRenderer
	keyInput    *terminal.KeyDecoder
	tasks       *taskStreamTracker
	// presentation is what the connected server's tools declared about
	// themselves. Its zero value renders through the legacy fallbacks.
	presentation chatview.Presentations
}

type streamResult struct {
	text string
	err  error
}

type streamToolReport struct {
	line      string
	label     string
	block     string
	blockKind string
	terminal  bool
	hidden    bool
	liveOnly  bool
	style     terminal.TextStyle
}

type streamToolTracker struct {
	tracker chatview.StreamToolTracker
	calls   map[string]streamToolCall
}

type streamToolCall struct{}

func (t *streamToolTracker) describe(item v1.Event) (string, string, bool) {
	return t.tracker.Describe(item)
}

func (t *streamToolTracker) describeReport(item v1.Event) streamToolReport {
	return streamToolReportFromView(t.tracker.DescribeReport(item))
}

func (t *streamToolTracker) output(item *v1.ToolOutputDelta) streamToolReport {
	return streamToolReportFromView(t.tracker.Output(item))
}

func streamToolReportFromView(report chatview.StreamToolReport) streamToolReport {
	return streamToolReport{line: report.Line, label: report.Label, block: report.Block, blockKind: report.BlockKind, terminal: report.Terminal, hidden: report.Hidden, liveOnly: report.LiveOnly, style: report.Style}
}

type taskReport struct {
	id            string
	line          string
	block         string
	blockKind     string
	blockLanguage string
	terminal      bool
	emitPlain     bool
	skip          bool
	style         terminal.TextStyle
}

// taskStreamTracker adapts the shared task tree tracker to the plain chat
// renderer. The tracker owns the parent-child relationships of every task on
// the stream; this wrapper only owns which report is currently live.
type taskStreamTracker struct {
	tracker      *chatview.TaskTracker
	presentation chatview.Presentations
	liveID       string
}

func newTaskStreamTracker(presentation chatview.Presentations) taskStreamTracker {
	tracker := chatview.NewTaskTracker()
	tracker.Presentation = presentation
	return taskStreamTracker{tracker: tracker, presentation: presentation}
}

func isTaskEvent(item v1.Event) bool { return chatview.IsTaskEvent(item) }

func (t *taskStreamTracker) Tracker() *chatview.TaskTracker {
	return t.tracker
}

func (t *taskStreamTracker) describe(item v1.Event, thinking bool) ([]taskReport, error) {
	if t.tracker == nil {
		t.tracker = chatview.NewTaskTracker()
		t.tracker.Presentation = t.presentation
	}
	reports, err := t.tracker.Apply(item, thinking)
	if err != nil {
		return nil, err
	}
	result := make([]taskReport, len(reports))
	for i, report := range reports {
		result[i] = taskReport{id: report.ID, line: report.Line, block: report.Block, blockKind: report.BlockKind, blockLanguage: report.BlockLanguage, terminal: report.Terminal, emitPlain: report.EmitPlain, skip: report.Skip, style: report.Style}
	}
	return result, nil
}

func writeStreamTaskEvent(options streamOptions, tracker *taskStreamTracker, item v1.Event) error {
	reports, err := tracker.describe(item, options.thinking)
	if err != nil {
		return err
	}
	for _, report := range reports {
		if report.skip {
			if report.terminal && options.renderer != nil && tracker.liveID == report.id {
				err = options.renderer.UpdateStyled(nil)
				tracker.liveID = ""
			}
			if err != nil {
				return err
			}
			continue
		}
		text := report.line
		if report.block != "" {
			text += "\n" + report.block
		}
		if options.renderer != nil {
			styled := terminal.StyledText{Text: text, Style: report.style}
			if report.terminal {
				tracker.liveID = ""
				if report.blockKind == chatview.ToolResultDiff {
					styled.Text = report.line
					err = options.renderer.CommitDiffBlock(styled, report.block)
				} else if report.blockKind == chatview.ToolResultCode {
					styled.Text = report.line
					err = options.renderer.CommitCodeBlock(styled, report.block, report.blockLanguage)
				} else if report.block != "" {
					err = options.renderer.CommitStyledBlock(styled)
				} else {
					err = options.renderer.CommitStyled(styled)
				}
			} else {
				err = options.renderer.UpdateStyled([]terminal.StyledText{styled})
				tracker.liveID = report.id
			}
		} else if report.emitPlain {
			_, err = fmt.Fprintln(options.stderr, terminal.Sanitize(text))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func toolActivityStyle(presentation chatview.Presentations, name string) terminal.TextStyle {
	return presentation.Style(name)
}
func toolActivityLabel(presentation chatview.Presentations, name string, input map[string]any) string {
	return presentation.Label(name, input)
}

func writeStreamCodeDisplay(options streamOptions, display *v1.CodeDisplay) error {
	status := chatview.CodeDisplayStatus(*display)
	if options.renderer != nil {
		return options.renderer.CommitCodeBlock(terminal.MutedText(status), display.Source, display.Language)
	}
	_, err := fmt.Fprintln(options.stderr, terminal.Sanitize(status+"\n"+display.Source))
	return err
}

func writeStreamToolOutput(options streamOptions, tracker *streamToolTracker, item *v1.ToolOutputDelta) error {
	report := tracker.output(item)
	if report.line == "" || options.renderer == nil {
		return nil
	}
	text := report.line
	if report.block != "" {
		text += "\n" + report.block
	}
	return options.renderer.UpdateStyled([]terminal.StyledText{{Text: text, Style: report.style}})
}

func writeStreamToolEvent(options streamOptions, tracker *streamToolTracker, item v1.Event) error {
	report := tracker.describeReport(item)
	if report.hidden {
		return nil
	}
	if options.renderer != nil {
		styled := terminal.StyledText{Text: report.line, Style: report.style}
		if !report.terminal {
			return options.renderer.UpdateStyled([]terminal.StyledText{styled})
		}
		if report.liveOnly {
			return options.renderer.UpdateStyled(nil)
		}
		if report.blockKind == chatview.ToolResultDiff {
			return options.renderer.CommitDiffBlock(styled, report.block)
		}
		if report.block != "" {
			styled.Text += "\n" + report.block
			return options.renderer.CommitStyledBlock(styled)
		}
		return options.renderer.CommitStyled(styled)
	}
	if report.liveOnly {
		return nil
	}
	text := report.line
	if report.block != "" {
		text += "\n" + report.block
	}
	_, err := fmt.Fprintln(options.stderr, terminal.Sanitize(text))
	return err
}

func streamTurn(ctx context.Context, api apiClient, sessionID, prompt string, options streamOptions) streamResult {
	after := int64(^uint64(0) >> 1)
	stream, err := api.Events(ctx, sessionID, &after)
	if err != nil {
		return streamResult{err: err}
	}
	defer stream.Close()
	connected, err := stream.Next()
	if err != nil || connected.Type != v1.EventServerConnected {
		if err == nil {
			err = errors.New("event stream did not send server.connected")
		}
		return streamResult{err: err}
	}
	if options.format == "jsonl" {
		if err := writeJSONLine(options.stdout, connected); err != nil {
			return streamResult{err: err}
		}
	}
	before, _ := api.Messages(ctx, sessionID)
	if options.resume {
		resumer, ok := api.(interface {
			Resume(context.Context, string) error
		})
		if !ok {
			return streamResult{err: errors.New("connected server does not support explicit resume")}
		}
		if err := resumer.Resume(ctx, sessionID); err != nil {
			return streamResult{err: err}
		}
	} else {
		messageID, err := opaqueID("msg")
		if err != nil {
			return streamResult{err: err}
		}
		if _, err := api.Prompt(ctx, sessionID, v1.PromptRequest{MessageID: messageID, Content: prompt, Delivery: "steer"}); err != nil {
			return streamResult{err: err}
		}
	}
	events := make(chan eventResult)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			item, nextErr := stream.Next()
			select {
			case events <- eventResult{item, nextErr}:
			case <-done:
				return
			}
			if nextErr != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var streamed strings.Builder
	statusError := false
	interrupted := false
	interruptCount := 0
	toolInput := false
	toolTracker := streamToolTracker{tracker: chatview.StreamToolTracker{Presentation: options.presentation}}
	subagentTracker := options.tasks
	if subagentTracker == nil {
		fresh := newTaskStreamTracker(options.presentation)
		subagentTracker = &fresh
	}
	interrupts, _ := ctx.Value(interruptKey{}).(<-chan os.Signal)
	requestInterrupt := func() error {
		interruptCount++
		if interruptCount > 1 {
			return errSecondInterrupt
		}
		interrupted = true
		if options.renderer != nil {
			subagentTracker.liveID = ""
			_ = options.renderer.Commit("■ Interrupt requested")
		} else {
			fmt.Fprintln(options.stderr, "■ Interrupt requested")
		}
		go func() {
			interruptCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = api.Interrupt(interruptCtx, sessionID)
		}()
		return nil
	}
	jsonl := jsonlRedactor{presentation: options.presentation}
	for {
		select {
		case <-interrupts:
			if err := requestInterrupt(); err != nil {
				return streamResult{err: err}
			}
		case <-ctx.Done():
			interruptCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = api.Interrupt(interruptCtx, sessionID)
			cancel()
			return streamResult{err: ctx.Err()}
		case <-ticker.C:
			if err := settleStreamPrompts(ctx, api, sessionID, options); err != nil {
				if errors.Is(err, terminal.ErrInterrupted) {
					if interruptErr := requestInterrupt(); interruptErr != nil {
						return streamResult{err: interruptErr}
					}
					continue
				}
				return streamResult{err: err}
			}
			if options.keyInput != nil {
				pollCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
				key, keyErr := options.keyInput.ReadKey(pollCtx)
				cancel()
				if keyErr == nil && isExecutionHaltKey(key) {
					if err := requestInterrupt(); err != nil {
						return streamResult{err: err}
					}
				} else if keyErr != nil && !errors.Is(keyErr, context.DeadlineExceeded) && !errors.Is(keyErr, context.Canceled) {
					return streamResult{err: keyErr}
				}
			}
		case result := <-events:
			if result.err != nil {
				return streamResult{err: result.err}
			}
			item := result.event
			if options.format == "jsonl" {
				if err := writeJSONLine(options.stdout, jsonl.redact(item)); err != nil {
					return streamResult{err: err}
				}
			}
			if options.format != "jsonl" && isTaskEvent(item) {
				if err := writeStreamTaskEvent(options, subagentTracker, item); err != nil {
					return streamResult{err: err}
				}
				continue
			}
			if options.format != "jsonl" && strings.HasPrefix(item.Type, "session.tool.") {
				subagentTracker.liveID = ""
				if err := writeStreamToolEvent(options, &toolTracker, item); err != nil {
					return streamResult{err: err}
				}
			}
			if options.format != "jsonl" && (item.Type == "session.context.initialized" || item.Type == "session.context.changed" || item.Type == "session.context.replaced") {
				subagentTracker.liveID = ""
				if err := writeAgentsLoadedActivity(options, item); err != nil {
					return streamResult{err: err}
				}
			}
			payload, decodeErr := v1.DecodeEventData(item)
			if decodeErr != nil {
				return streamResult{err: decodeErr}
			}
			switch value := payload.(type) {
			case *v1.MessagePartDelta:
				if value.Kind == "text" {
					streamed.WriteString(value.Delta)
					if options.renderer != nil {
						subagentTracker.liveID = ""
						if err := options.renderer.UpdateMessage("● ", streamed.String()); err != nil {
							return streamResult{err: err}
						}
					}
				} else if value.Kind == "tool_input" {
					toolInput = true
				} else if options.format != "jsonl" && (value.Kind != "reasoning" && value.Kind != "reasoning_summary" || options.thinking) {
					if options.renderer != nil {
						subagentTracker.liveID = ""
						_ = options.renderer.Update([]string{"status: " + value.Kind})
					} else {
						fmt.Fprintf(options.stderr, "status: %s\n", value.Kind)
					}
				}
			case *v1.SessionStatus:
				// Retry notices carry a human-readable message; flush it as-is
				// instead of buffering it into the assistant stream.
				label := "status: " + value.Kind
				if (value.Kind == "provider_retry" || value.Kind == "status_prompt") && value.Message != "" {
					label = chatview.EventLine(0, "", "↻ "+value.Message)
				}
				if value.Kind == "router_metadata" && value.Message != "" {
					label = value.Message
				}
				if options.format != "jsonl" && value.Kind != "idle" && value.Kind != "finish" && value.Kind != "usage" {
					if options.renderer != nil {
						subagentTracker.liveID = ""
						_ = options.renderer.Update([]string{label})
					} else {
						fmt.Fprintln(options.stderr, label)
					}
				}
				if value.Kind == "error" || value.Kind == "provider_error" {
					statusError = true
				}
				if value.Kind == "idle" || value.Kind == "error" {
					finished := finishStream(api, sessionID, before, streamed.String(), statusError, toolInput, options)
					if interrupted {
						finished.err = nil
					}
					return finished
				}
			case *v1.CodeDisplay:
				if options.format != "jsonl" {
					subagentTracker.liveID = ""
					if err := writeStreamCodeDisplay(options, value); err != nil {
						return streamResult{err: err}
					}
				}
			case *v1.ToolOutputDelta:
				if options.format != "jsonl" {
					subagentTracker.liveID = ""
					if err := writeStreamToolOutput(options, &toolTracker, value); err != nil {
						return streamResult{err: err}
					}
				}
			}
		}
	}
}

func agentsLoadedPaths(item v1.Event) []string { return chatview.AgentsLoadedPaths(item) }
func agentsLoadedActivity(path string) string  { return chatview.AgentsLoadedActivity(path) }

func agentsLoadedLines(paths []string) []string { return chatview.AgentsLoadedLines(paths) }

func agentsLoadedActivities(item v1.Event) []string { return chatview.AgentsLoadedActivities(item) }

func writeAgentsStartupActivity(output io.Writer, paths []string) {
	for _, line := range agentsLoadedLines(paths) {
		_, _ = fmt.Fprintln(output, terminal.Sanitize(line))
	}
}

func writeAgentsLoadedActivity(options streamOptions, item v1.Event) error {
	for _, line := range agentsLoadedActivities(item) {
		if options.renderer != nil {
			if err := options.renderer.Commit(line); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintln(options.stderr, terminal.Sanitize(line)); err != nil {
			return err
		}
	}
	return nil
}

type eventResult struct {
	event v1.Event
	err   error
}

type messageClient interface {
	Messages(context.Context, string) (v1.MessageList, error)
}

func finishStream(api messageClient, sessionID string, before v1.MessageList, streamed string, statusError bool, toolInput bool, options streamOptions) streamResult {
	after, err := api.Messages(context.Background(), sessionID)
	if err != nil {
		return streamResult{err: err}
	}
	old := make(map[string]bool, len(before.Items))
	for _, item := range before.Items {
		old[item.ID] = true
	}
	final := ""
	finalError := ""
	for _, item := range after.Items {
		if !old[item.ID] && item.Role == "assistant" {
			final += item.Content
			if item.Error != "" {
				finalError = item.Error
			}
		}
	}
	if final == "" {
		final = streamed
	}
	if final == "" && !toolInput {
		final = chatview.AgentEmptyResponseText
	}
	if options.format == "text" && !options.chat {
		if _, err := io.WriteString(options.stdout, terminal.Sanitize(strings.TrimRight(final, "\n")+"\n")); err != nil {
			return streamResult{err: err}
		}
	}
	if options.chat {
		if options.renderer != nil {
			if err := options.renderer.CommitMessage("● ", final, false); err != nil {
				return streamResult{err: err}
			}
		} else if final = strings.TrimRight(final, "\r\n"); final != "" {
			plain := terminal.NewLiveRenderer(options.stdout, terminal.RendererConfig{Columns: terminal.Columns(options.stdout)})
			if err := plain.CommitMessage("● ", final, false); err != nil {
				return streamResult{err: err}
			}
		}
	}
	if finalError != "" {
		line := "✗ Error: " + finalError
		if options.renderer != nil {
			_ = options.renderer.Commit(line)
		} else {
			fmt.Fprintln(options.stderr, line)
		}
	}
	if statusError || finalError != "" {
		return streamResult{text: final, err: errors.New("session turn failed")}
	}
	return streamResult{text: final}
}

func streamToolStatus(status, errorText string) string {
	return chatview.StreamToolStatus(status, errorText)
}

func permissionContextLines(item v1.Permission) []string {
	return chatview.PermissionContextLines(item)
}

func formatJSONAsYAML(input json.RawMessage) string { return chatview.FormatJSONAsYAML(input) }

func writePermissionContext(w io.Writer, item v1.Permission) {
	for _, line := range permissionContextLines(item) {
		fmt.Fprintln(w, line)
	}
}

func settlePrompts(ctx context.Context, api apiClient, sessionID string, input io.Reader, output io.Writer) error {
	permissions, err := api.Permissions(ctx, sessionID)
	if err != nil {
		return err
	}
	questions, err := api.Questions(ctx, sessionID)
	if err != nil {
		return err
	}
	if input == nil {
		return nil
	}
	reader, ok := input.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(input)
	}
	for _, item := range permissions.Items {
		writePermissionContext(output, item)
		fmt.Fprint(output, permissionPromptFor(item))
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		var reply v1.PermissionReply
		answer := strings.ToLower(strings.TrimSpace(line))
		if declared, ok := chatview.PermissionReplyForChoice(item, answer, ""); ok {
			reply = declared
			if requiresPermissionReason(item, answer) {
				fmt.Fprint(output, "rejection reason: ")
				reason, reasonErr := reader.ReadString('\n')
				if reasonErr != nil && !errors.Is(reasonErr, io.EOF) {
					return reasonErr
				}
				reply.Reason = strings.TrimSpace(reason)
			}
		} else {
			reply = permissionDefaultReply(item)
		}
		if err := api.ReplyPermission(ctx, sessionID, item.ID, reply); err != nil {
			return err
		}
	}
	for _, request := range questions.Items {
		answers := make([]v1.Answer, 0, len(request.Questions))
		reject := false
		for _, item := range request.Questions {
			fmt.Fprintf(output, "question: %s\nanswer with option ID or text [reject]: ", item.Prompt)
			line, readErr := reader.ReadString('\n')
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			value := strings.TrimSpace(line)
			if value == "" {
				reject = true
				break
			}
			answer := v1.Answer{QuestionID: item.ID}
			for _, option := range item.Options {
				if value == option.ID {
					answer.OptionIDs = []string{value}
				}
			}
			if len(answer.OptionIDs) == 0 {
				answer.Custom = value
			}
			answers = append(answers, answer)
		}
		if err := api.ReplyQuestion(ctx, sessionID, request.ID, v1.QuestionReply{Answers: answers, Reject: reject}); err != nil {
			return err
		}
	}
	return nil
}

func settleStreamPrompts(ctx context.Context, api apiClient, sessionID string, options streamOptions) error {
	if options.renderer == nil || options.keyInput == nil {
		return settlePrompts(ctx, api, sessionID, options.promptInput, options.stderr)
	}
	permissions, err := api.Permissions(ctx, sessionID)
	if err != nil {
		return err
	}
	questions, err := api.Questions(ctx, sessionID)
	if err != nil {
		return err
	}
	read := func(prefix string) (string, error) {
		editor := terminal.NewEditorDecoder(options.keyInput, options.stdout,
			terminal.WithEditorPrompt(prefix), terminal.WithEditorRenderer(options.renderer))
		return editor.Read(ctx)
	}
	for _, item := range permissions.Items {
		for _, line := range permissionContextLines(item) {
			if err := options.renderer.Commit(line); err != nil {
				return err
			}
		}
		picker := terminal.NewPickerDecoder(options.keyInput, options.stdout, permissionChoicesFor(item),
			terminal.WithPickerPrompt("permission decision: "), terminal.WithPickerRenderer(options.renderer))
		choice, readErr := picker.Pick(ctx)
		if errors.Is(readErr, terminal.ErrCanceled) || errors.Is(readErr, io.EOF) {
			choice.Value, readErr = "reject", nil
		}
		if readErr != nil {
			return readErr
		}
		var reply v1.PermissionReply
		if declared, ok := chatview.PermissionReplyForChoice(item, choice.Value, ""); ok {
			reply = declared
			if requiresPermissionReason(item, choice.Value) {
				reason, reasonErr := read("rejection reason: ")
				if errors.Is(reasonErr, terminal.ErrCanceled) || errors.Is(reasonErr, io.EOF) {
					reason, reasonErr = "", nil
				}
				if reasonErr != nil {
					return reasonErr
				}
				reply.Reason = strings.TrimSpace(reason)
			}
		} else {
			reply = permissionDefaultReply(item)
		}
		if err := api.ReplyPermission(ctx, sessionID, item.ID, reply); err != nil {
			return err
		}
	}
	for _, request := range questions.Items {
		answers := make([]v1.Answer, 0, len(request.Questions))
		reject := false
		for _, item := range request.Questions {
			if err := options.renderer.Commit("question: " + item.Prompt); err != nil {
				return err
			}
			line, readErr := read("answer with option ID or text [reject]: ")
			if errors.Is(readErr, terminal.ErrCanceled) || errors.Is(readErr, io.EOF) {
				line, readErr = "", nil
			}
			if readErr != nil {
				return readErr
			}
			value := strings.TrimSpace(line)
			if value == "" {
				reject = true
				break
			}
			answer := v1.Answer{QuestionID: item.ID}
			for _, option := range item.Options {
				if value == option.ID {
					answer.OptionIDs = []string{value}
				}
			}
			if len(answer.OptionIDs) == 0 {
				answer.Custom = value
			}
			answers = append(answers, answer)
		}
		if err := api.ReplyQuestion(ctx, sessionID, request.ID, v1.QuestionReply{Answers: answers, Reject: reject}); err != nil {
			return err
		}
	}
	return nil
}

type chatSelection struct {
	agent    string
	provider string
	model    string
	variant  string
}

func (s chatSelection) modelName() string {
	if s.provider == "" || s.model == "" {
		return ""
	}
	return s.provider + "/" + s.model
}

func (s chatSelection) modelLabel() string {
	if value := s.modelName(); value != "" {
		if s.variant != "" {
			return value + " · " + s.variant
		}
		return value
	}
	return "no model"
}

func (s *chatShell) modelineModelLabel(currentTokens int) string {
	label := s.selection.modelLabel()
	window := s.modelInfo.ContextWindow
	if window > 0 {
		return fmt.Sprintf("%s (%s/%s)", label, compactTokenCount(currentTokens), compactTokenCount(window))
	}
	if currentTokens > 0 {
		return fmt.Sprintf("%s (%s/?)", label, compactTokenCount(currentTokens))
	}
	return label
}

func compactTokenCount(value int) string {
	switch {
	case value >= 1_000_000:
		if value%1_000_000 == 0 {
			return fmt.Sprintf("%dm", value/1_000_000)
		}
		return fmt.Sprintf("%.1fm", float64(value)/1_000_000)
	case value >= 1_000:
		if value%1_000 == 0 {
			return fmt.Sprintf("%dk", value/1_000)
		}
		return fmt.Sprintf("%.1fk", float64(value)/1_000)
	default:
		return fmt.Sprintf("%d", value)
	}
}

// refreshModelInfo reloads the cached catalog entry the modeline reads its
// context window from. Every path that changes the selection must call it,
// otherwise the modeline reports the window of a previously applied model or,
// at startup, none at all. Re-fetching is skipped while the cache already
// describes the selection, so callers may invoke it freely.
func (s *chatShell) refreshModelInfo() {
	if s.selection.provider == "" || s.selection.model == "" {
		s.modelInfo = v1.Model{}
		return
	}
	if s.modelInfo.Provider == s.selection.provider && s.modelInfo.ID == s.selection.model {
		return
	}
	info, err := s.api.ModelInfo(s.ctx, s.selection.provider, s.selection.model)
	if err != nil {
		// A server that cannot describe the model leaves the window unknown
		// rather than keeping the previous model's, and the next call retries.
		s.modelInfo = v1.Model{}
		return
	}
	s.modelInfo = info
}

type chatShell struct {
	ctx          context.Context
	api          apiClient
	current      v1.Session
	selection    chatSelection
	options      codingFlags
	projectID    string
	projectRoot  string
	configDir    string
	claimRequest v1.ClaimSessionRequest
	commands     *customcommand.Registry
	build        BuildInfo
	credentials  auth.Store
	// reloadProviders rebuilds the local backend's providers from the credential
	// store after /auth changes a credential, so new keys take effect without a
	// restart. It is nil when no local backend is reloadable.
	reloadProviders func(context.Context) error
	handler         http.Handler
	server          *http.Server
	listener        net.Listener
	models          []v1.Model
	modelInfo       v1.Model
	presentation    chatview.Presentations
	stdout          io.Writer
	stderr          io.Writer
	reader          *bufio.Reader
	decoder         *terminal.KeyDecoder
	editor          *terminal.Editor
	renderer        *terminal.LiveRenderer
	enhanced        bool
	inputTTY        bool
	outputTTY       bool
	inputEcho       bool
	inputEchoed     bool
	columns         int

	// tasks tracks the session's task tree across turns. A task started in one
	// turn can still emit events in a later one, so the tracker is rebuilt only
	// when the session changes, never between turns of one session.
	tasks        *taskStreamTracker
	tasksSession string

	// lastPrompt stores the last submitted prompt so /continue can resend it
	// after an error or when the user wants to retry.
	lastPrompt string
}

// taskTracker returns the task tree tracker for the current session, starting
// a fresh tree when the session changes.
func (s *chatShell) taskTracker() *taskStreamTracker {
	if s.tasks == nil || s.tasksSession != s.current.ID {
		tracker := newTaskStreamTracker(s.presentation)
		s.tasks = &tracker
		s.tasksSession = s.current.ID
	}
	return s.tasks
}

func (s *chatShell) runEnhanced(first string) int {
	result := enhancedchat.Run(s.enhancedConfig(), first)
	return finish(s.ctx, result.Code, result.Reason, result.Err)
}

func (s *chatShell) enhancedConfig() enhancedchat.Config {
	return enhancedchat.Config{
		Context: s.ctx, Interrupts: interruptChannel(s.ctx), API: s.api, CurrentAPI: func() enhancedchat.API { return s.api },
		Presentation: func() chatview.Presentations { return s.presentation },
		Editor:       s.editor, Decoder: s.decoder, Renderer: s.renderer, Stderr: s.stderr,
		Thinking: s.options.thinking, ThinkingEnabled: func() bool { return s.options.thinking },
		Current: func() v1.Session { return s.current },
		SetCurrent: func(item v1.Session) {
			s.current = item
			s.selection = selectionFromSession(item, s.selection.agent)
			s.refreshModelInfo()
		},
		Agent: func() string { return s.selection.agent },
		// selection is a struct value, so a bound method value would capture a
		// copy taken at startup and report that model forever. refreshState
		// assigns this back onto the enhanced selection, so a stale reading
		// silently undoes every model switch.
		ModelName: func() string { return s.selection.modelName() }, ModelineModelLabel: s.modelineModelLabel,
		NextAgent: s.nextAgent, ApplyAgent: s.applyAgent, SelectAgent: s.selectAgent,
		PickModel: s.pickModel, ApplyModel: s.applyModel, SelectModel: s.selectModel,
		ChooseSession: s.chooseSession, CreateSession: s.createSession,
		ResumeSession: func(id string) error {
			resumer, ok := s.api.(resumableClient)
			if !ok {
				return errors.New("connected server does not support explicit resume")
			}
			return resumer.Resume(s.ctx, id)
		},
		CommitUser: s.commitUser, CommitError: s.commitError,
		Expand: func(name, arguments string) (enhancedchat.Expansion, error) {
			expansion, err := s.commands.Expand(name, arguments)
			return enhancedchat.Expansion{Prompt: expansion.Prompt, Agent: expansion.Agent, Model: expansion.Model, Subtask: expansion.Subtask}, err
		},
		Slash:          s.slash,
		OnTurnComplete: s.onTurnComplete,
	}
}

func (s *chatShell) onTurnComplete(completed enhancedchat.TurnComplete) *enhancedchat.TurnCompleteDialog {
	spec, ok, err := s.turnCompleteSpec(completed)
	if err != nil {
		s.commitError("turn completion: " + err.Error())
		return nil
	}
	if !ok {
		return nil
	}
	if spec.Dialog == nil {
		// A mode may transition directly without a dialog.
		if spec.Agent != "" {
			_ = s.applyAgent(spec.Agent, false)
		}
		return nil
	}
	dialog := spec.Dialog
	choices := make([]terminal.Candidate, 0, len(dialog.Choices)+1)
	for _, choice := range dialog.Choices {
		choices = append(choices, terminal.Candidate{Value: choice.Value, Description: choice.Description})
	}
	if dialog.CustomChoice != "" {
		description := dialog.CustomDescription
		if description == "" {
			description = "Provide feedback and revise"
		}
		choices = append(choices, terminal.Candidate{Value: dialog.CustomChoice, Description: description})
	}
	return &enhancedchat.TurnCompleteDialog{
		Markdown: dialog.Markdown, Prompt: dialog.Prompt, Context: dialog.Context,
		Choices: choices, CustomChoice: dialog.CustomChoice, CustomPrompt: dialog.CustomPrompt,
		Handle: func(value string) (enhancedchat.TurnCompleteResult, error) {
			answer := strings.ToLower(strings.TrimSpace(value))
			for _, choice := range dialog.Choices {
				if answer != choice.Value && !containsString(choice.Aliases, answer) {
					continue
				}
				if choice.Action.Agent != "" {
					if err := s.applyAgent(choice.Action.Agent, false); err != nil {
						return enhancedchat.TurnCompleteResult{}, err
					}
				}
				return enhancedchat.TurnCompleteResult{Prompt: choice.Action.Prompt}, nil
			}
			if strings.TrimSpace(value) == "" {
				return enhancedchat.TurnCompleteResult{ValidationError: dialog.EmptyMessage}, nil
			}
			return enhancedchat.TurnCompleteResult{Prompt: strings.TrimSpace(value)}, nil
		},
	}
}

// turnCompleteSpec resolves the completed turn's behavior. Newer servers can
// enrich the mode declaration with turn-specific data such as a written plan;
// the static declaration remains a compatibility fallback.
func (s *chatShell) turnCompleteSpec(completed enhancedchat.TurnComplete) (mode.TurnCompleteResult, bool, error) {
	if completed.Mode == "" {
		return mode.TurnCompleteResult{}, false, nil
	}
	if provider, ok := s.api.(interface {
		TurnCompletion(context.Context, string, string) (v1.TurnCompletion, error)
	}); ok && completed.Session.ID != "" && completed.MessageID != "" {
		completion, err := provider.TurnCompletion(s.ctx, completed.Session.ID, completed.MessageID)
		if err == nil {
			if len(completion.TurnComplete) == 0 {
				return mode.TurnCompleteResult{}, false, nil
			}
			var spec mode.TurnCompleteResult
			if err := json.Unmarshal(completion.TurnComplete, &spec); err != nil {
				return mode.TurnCompleteResult{}, false, fmt.Errorf("decode result: %w", err)
			}
			return spec, true, nil
		}
		return mode.TurnCompleteResult{}, false, err
	}
	lister, ok := s.api.(interface {
		Modes(context.Context) (v1.ModeList, error)
	})
	if !ok {
		return mode.TurnCompleteResult{}, false, nil
	}
	items, err := lister.Modes(s.ctx)
	if err != nil {
		return mode.TurnCompleteResult{}, false, nil
	}
	for _, item := range items.Items {
		if item.ID != completed.Mode || len(item.TurnComplete) == 0 {
			continue
		}
		var spec mode.TurnCompleteResult
		if err := json.Unmarshal(item.TurnComplete, &spec); err != nil {
			return mode.TurnCompleteResult{}, false, nil
		}
		return spec, true, nil
	}
	return mode.TurnCompleteResult{}, false, nil
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func (s *chatShell) enhancedRenderError(err error) int {
	fmt.Fprintln(s.stderr, "parrot: enhanced chat render failed:", terminal.Sanitize(err.Error()))
	reason := "enhanced_render_failed"
	if class := terminal.RenderErrorClass(err); class != "" {
		reason += "_" + class
	}
	return finish(s.ctx, exitError, reason, err)
}

func (s *chatShell) run(first string) int {
	draft := first
	readDraft := draft == ""
	for {
		if readDraft {
			line, err := s.readPrompt(draft)
			if errors.Is(err, io.EOF) {
				return finish(s.ctx, exitOK, "chat_input_closed", nil)
			}
			if errors.Is(err, terminal.ErrInterrupted) || errors.Is(err, terminal.ErrCanceled) {
				draft = ""
				readDraft = true
				continue
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return finish(s.ctx, exitInterrupt, "chat_input_interrupted", err)
				}
				s.commitError(err.Error())
				return finish(s.ctx, exitError, "chat_input_failed", err)
			}
			draft = line
		}
		readDraft = true
		if strings.TrimSpace(draft) == "" {
			draft = ""
			continue
		}

		line := draft
		if strings.HasPrefix(strings.TrimSpace(line), "/") {
			trimmed := strings.TrimSpace(line)
			name, arguments := slashParts(trimmed)
			if name == "/run" {
				if arguments == "" {
					s.commitError("run requires a prompt")
					draft = ""
					continue
				}
				line = arguments
			} else if isBuiltinSlash(name) {
				exit, code := s.slash(name, arguments)
				draft = ""
				if exit {
					return finish(s.ctx, code, chatExitReason(code), nil)
				}
				continue
			}
			expansion, err := s.commands.Expand(strings.TrimPrefix(name, "/"), arguments)
			if err != nil {
				s.commitError(fmt.Sprintf("unknown slash command %q: %v", name, err))
				draft = ""
				continue
			}
			if expansion.Subtask {
				line = subtaskPrompt(expansion)
			} else {
				if expansion.Agent != "" {
					if err := s.selectAgent(expansion.Agent); err != nil {
						s.commitError(err.Error())
						draft = ""
						continue
					}
				}
				if expansion.Model != "" {
					if err := s.selectModel(expansion.Model); err != nil {
						s.commitError(err.Error())
						draft = ""
						continue
					}
				}
				line = expansion.Prompt
			}
		}
		if line != draft {
			s.inputEchoed = false
		}

		if s.selection.modelName() == "" {
			selected, err := s.pickModel()
			if err != nil {
				if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
					s.commitError(err.Error())
				}
				// Re-open the editor with the exact draft after cancellation or an
				// empty catalog. No session has been created at this point.
				readDraft = true
				continue
			}
			if err := s.applyModel(selected); err != nil {
				s.commitError(err.Error())
				readDraft = true
				continue
			}
		}

		if s.current.ID == "" {
			item, err := s.createSession(line, false)
			if err != nil {
				s.commitError(err.Error())
				readDraft = true
				continue
			}
			s.current = item
		}
		if err := s.commitUser(line); err != nil {
			s.commitError(err.Error())
			return finish(s.ctx, exitError, "chat_output_failed", err)
		}
		s.lastPrompt = line
		result := streamTurn(s.ctx, s.api, s.current.ID, line, s.streamOptions(false))
		draft = ""
		if result.err != nil {
			if errors.Is(result.err, errSecondInterrupt) || errors.Is(result.err, context.Canceled) {
				return finish(s.ctx, exitInterrupt, "turn_interrupted", result.err)
			}
			s.commitError(result.err.Error())
		}
	}
}

func (s *chatShell) createSession(title string, forceNew bool) (v1.Session, error) {
	if claimer, ok := s.api.(sessionClaimer); ok && s.claimRequest.WorkingDirectory != "" {
		request := s.claimRequest
		line, _, _ := strings.Cut(strings.TrimSpace(title), "\n")
		if len(line) > 80 {
			line = line[:80]
		}
		request.Title, request.Agent, request.Model, request.ForceNew = line, s.selection.agent, s.selection.modelName(), forceNew
		request.Variant = &s.selection.variant
		claimed, err := claimer.ClaimSession(s.ctx, request)
		return claimed.Session, err
	}
	return createChatSession(s.ctx, s.api, s.projectID, title, s.selection)
}

func (s *chatShell) commitUser(text string) error {
	text = strings.TrimRight(text, "\r\n")
	if s.renderer != nil {
		return s.renderer.CommitUserMessage("$ ", text)
	}
	if !s.inputEchoed || !s.outputTTY {
		fmt.Fprintln(s.stdout, "$ "+strings.ReplaceAll(text, "\n", "\n  "))
	}
	return nil
}

func (s *chatShell) readPrompt(initial string) (string, error) {
	if s.enhanced {
		s.inputEchoed = false
		s.editor.SetPrompt(s.promptLabel())
		return s.editor.ReadInitial(s.ctx, initial)
	}
	if s.inputTTY {
		fmt.Fprint(s.stdout, "$ ")
	}
	s.inputEchoed = s.inputEcho
	line, err := s.reader.ReadString('\n')
	if errors.Is(err, io.EOF) && line != "" {
		err = nil
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), err
}

func (s *chatShell) promptLabel() string {
	return "$ "
}

func (s *chatShell) streamOptions(resume bool) streamOptions {
	return streamOptions{format: "text", stdout: s.stdout, stderr: s.stderr, promptInput: s.reader,
		thinking: s.options.thinking, chat: true, resume: resume, renderer: s.renderer, keyInput: s.decoder, tasks: s.taskTracker(),
		presentation: s.presentation}
}

func (s *chatShell) commit(text string) {
	if s.renderer != nil {
		_ = s.renderer.Commit(text)
		return
	}
	fmt.Fprintln(s.stdout, terminal.Sanitize(text))
}

func (s *chatShell) commitError(text string) {
	line := "✗ Error: " + terminal.Sanitize(text)
	if s.renderer != nil {
		_ = s.renderer.Commit(line)
		return
	}
	fmt.Fprintln(s.stderr, line)
}

func (s *chatShell) commitStatus(text string) {
	if s.renderer != nil {
		_ = s.renderer.Commit(text)
		return
	}
	fmt.Fprintln(s.stderr, terminal.Sanitize(text))
}

func selectionFromSession(item v1.Session, fallbackAgent string) chatSelection {
	agent := item.Agent
	if agent == "" {
		agent = fallbackAgent
	}
	return chatSelection{agent: agent, provider: item.Provider, model: item.Model, variant: item.Variant}
}

func defaultChatSelection(item v1.SessionSelection, variantOverride string) chatSelection {
	variant := item.Variant
	if variantOverride != "" {
		variant = variantOverride
	}
	return chatSelection{agent: item.Agent, provider: item.Provider, model: item.Model, variant: variant}
}

type sessionCreator interface {
	CreateSession(context.Context, v1.CreateSessionRequest) (v1.Session, error)
}

func createChatSession(ctx context.Context, api sessionCreator, projectID, title string, selection chatSelection) (v1.Session, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(title), "\n")
	if len(line) > 80 {
		line = line[:80]
	}
	request := v1.CreateSessionRequest{ProjectID: projectID, Title: line, Agent: selection.agent, Model: selection.modelName(), Variant: &selection.variant}
	return api.CreateSession(ctx, request)
}

var builtinChatCommands = []terminal.Candidate{
	{Value: "/help", Description: "show commands and keybindings"},
	{Value: "/version", Description: "print build information"},
	{Value: "/run", Description: "execute a prompt in this chat"},
	{Value: "/chat", Description: "show interactive chat status"},
	{Value: "/models", Description: "list available models"},
	{Value: "/usage", Description: "show ChatGPT subscription usage"},
	{Value: "/model", Description: "select a model"},
	{Value: "/effort", Description: "select model reasoning effort"},
	{Value: "/modes", Description: "list available modes"},
	{Value: "/mode", Description: "select a mode"},
	{Value: "/agents", Description: "list reusable child agents"},
	{Value: "/agent", Description: "deprecated alias for /mode"},
	{Value: "/sessions", Description: "list sessions"},
	{Value: "/session", Description: "list, show, switch, compact, or delete sessions"},
	{Value: "/auth", Description: "list, login, or logout provider credentials"},
	{Value: "/serve", Description: "start, stop, or inspect the local API server"},
	{Value: "/resume", Description: "resume an interrupted session"},
	{Value: "/new", Description: "start a new session"},
	{Value: "/clear", Description: "start a fresh session"},
	{Value: "/continue", Description: "retry the last prompt after an error"},
	{Value: "/compact", Description: "compact the current conversation"},
	{Value: "/connect", Description: "connect to an API server"},
	{Value: "/thinking", Description: "toggle reasoning status"},
	{Value: "/goal", Description: "show or control the current goal"},
	{Value: "/status", Description: "show chat state"},
	{Value: "/exit", Description: "exit chat"},
}

func chatCompletionCandidates(commands *customcommand.Registry) []terminal.Candidate {
	items := append([]terminal.Candidate(nil), builtinChatCommands...)
	if commands == nil {
		return items
	}
	for _, item := range commands.List() {
		items = append(items, terminal.Candidate{Value: "/" + item.Name, Description: item.Description})
	}
	return items
}

func (s *chatShell) slash(command, argument string) (bool, int) {
	switch command {
	case "/help":
		var text strings.Builder
		text.WriteString("Keys: Enter submit/queue; Ctrl-J newline; Ctrl-A/Ctrl-E line start/end; Ctrl-K clear to end of line; Ctrl-C clear draft/interrupt turn; Ctrl-D exit when idle; Tab complete; Escape cancel\nCommands:\n")
		for _, item := range chatCompletionCandidates(s.commands) {
			fmt.Fprintf(&text, "%s\t%s\n", item.Value, item.Description)
		}
		s.commit(strings.TrimSuffix(text.String(), "\n"))
	case "/version":
		s.commit(fmt.Sprintf("parrot %s\ncommit: %s\nbuilt: %s", s.build.Version, s.build.Commit, s.build.Date))
	case "/chat":
		s.commitStatus("✓ Interactive chat is already active")
	case "/models":
		items, err := s.api.Models(s.ctx)
		if err != nil {
			s.commitError(err.Error())
			break
		}
		s.models = items.Items
		if len(items.Items) == 0 {
			s.commit("no models available")
		}
		for _, item := range items.Items {
			s.commit(fmt.Sprintf("%s/%s\t%s", item.Provider, item.ID, item.Name))
		}
	case "/usage":
		usage, err := s.api.SubscriptionUsage(s.ctx)
		if err != nil {
			s.commitError(err.Error())
			break
		}
		s.commit(formatSubscriptionUsage(usage, time.Now()))
	case "/model":
		if argument == "" {
			value, err := s.pickModel()
			if err != nil {
				if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
					s.commitError(err.Error())
				}
				break
			}
			if err := s.applyModel(value); err != nil {
				s.commitError(err.Error())
			}
		} else if err := s.selectModel(argument); err != nil {
			s.commitError(err.Error())
		}
	case "/effort":
		if argument == "" {
			value, err := s.pickEffort()
			if err != nil {
				if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
					s.commitError(err.Error())
				}
				break
			}
			argument = value
		}
		if err := s.selectEffort(argument); err != nil {
			s.commitError(err.Error())
		}
	case "/agents":
		items, err := s.api.Agents(s.ctx)
		if err != nil {
			s.commitError(err.Error())
			break
		}
		if len(items.Items) == 0 {
			s.commit("no agents available")
		}
		for _, item := range items.Items {
			s.commit(fmt.Sprintf("%s\tread_only=%t\tmax_turns=%d", item.ID, item.ReadOnly, item.MaxTurns))
		}
	case "/modes":
		items, err := s.modes()
		if err != nil {
			s.commitError(err.Error())
			break
		}
		if len(items.Items) == 0 {
			s.commit("no modes available")
		}
		for _, item := range items.Items {
			s.commit(fmt.Sprintf("%s\tread_only=%t\tmax_turns=%d", item.ID, item.ReadOnly, item.MaxTurns))
		}
	case "/agent", "/mode":
		if argument == "" {
			items, err := s.modes()
			if err != nil {
				s.commitError(err.Error())
				break
			}
			candidates := make([]terminal.Candidate, 0, len(items.Items))
			for _, item := range items.Items {
				candidates = append(candidates, terminal.Candidate{Value: item.ID, Description: fmt.Sprintf("read_only=%t", item.ReadOnly)})
			}
			if len(candidates) == 0 {
				s.commit("no agents available")
				break
			}
			picked, pickErr := s.pick("mode> ", candidates)
			if pickErr != nil {
				break
			}
			argument = picked.Value
		}
		if err := s.selectAgent(argument); err != nil {
			s.commitError(err.Error())
		}
	case "/sessions":
		items, err := s.api.Sessions(s.ctx)
		if err != nil {
			s.commitError(err.Error())
			break
		}
		if len(items.Items) == 0 {
			s.commit("no sessions available")
		}
		for _, item := range items.Items {
			model := item.Provider + "/" + item.Model
			if item.Provider == "" || item.Model == "" {
				model = "no model"
			}
			s.commit(fmt.Sprintf("%s\t%s\t%s\t%s", item.ID, item.Agent, model, item.Title))
		}
	case "/session":
		if s.sessionAction(argument) {
			break
		}
		fallthrough
	case "/resume":
		item, err := s.chooseSession(argument)
		if err != nil {
			if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
				s.commitError(err.Error())
			}
			break
		}
		s.current = item
		s.selection = selectionFromSession(item, s.selection.agent)
		s.refreshModelInfo()
		s.commitStatus("✓ Session selected: " + item.ID)
		if command == "/resume" {
			result := streamTurn(s.ctx, s.api, item.ID, "", s.streamOptions(true))
			if errors.Is(result.err, errSecondInterrupt) || errors.Is(result.err, context.Canceled) {
				return true, finish(s.ctx, exitInterrupt, "turn_interrupted", result.err)
			}
			if result.err != nil {
				s.commitError(result.err.Error())
			}
		}
	case "/auth":
		s.authAction(argument)
	case "/serve":
		s.serveAction(argument)
	case "/continue":
		if s.lastPrompt == "" {
			s.commitError("no previous prompt to continue")
			break
		}
		if s.current.ID == "" {
			s.commitError("no active session")
			break
		}
		if err := s.commitUser(s.lastPrompt); err != nil {
			s.commitError(err.Error())
			break
		}
		result := streamTurn(s.ctx, s.api, s.current.ID, s.lastPrompt, s.streamOptions(false))
		if result.err != nil {
			if errors.Is(result.err, errSecondInterrupt) || errors.Is(result.err, context.Canceled) {
				return true, finish(s.ctx, exitInterrupt, "turn_interrupted", result.err)
			}
			s.commitError(result.err.Error())
		}
	case "/new", "/clear":
		if s.selection.modelName() == "" {
			s.commitError("select a model before starting a new session")
			break
		}
		item, err := s.createSession("", true)
		if err != nil {
			s.commitError(err.Error())
			break
		}
		s.current = item
		s.commitStatus("✓ New session: " + item.ID)
	case "/connect":
		if argument == "" {
			s.commitError("connect requires an http:// or https:// API URL")
			break
		}
		remote, err := client.New(argument, nil)
		if err != nil {
			s.commitError(err.Error())
			break
		}
		models, err := remote.Models(s.ctx)
		if err != nil {
			s.commitError(err.Error())
			break
		}
		agents, err := remote.Agents(s.ctx)
		if err != nil {
			s.commitError(err.Error())
			break
		}
		agent := s.selection.agent
		found := false
		for _, item := range agents.Items {
			found = found || item.ID == agent
		}
		if !found {
			agent = ""
			if len(agents.Items) > 0 {
				agent = agents.Items[0].ID
			}
		}
		// Tool presentation is optional: a server without the endpoint returns an
		// error here, and an empty table renders through the generic fallback.
		// Failing the connect over display metadata would be disproportionate.
		remoteTools, _ := remote.Tools(s.ctx)
		s.api = remote
		s.current = v1.Session{}
		s.claimRequest = v1.ClaimSessionRequest{}
		s.selection = chatSelection{agent: agent}
		s.refreshModelInfo()
		s.models = models.Items
		s.presentation = chatview.NewPresentations(remoteTools)
		s.commitStatus("✓ Connected: " + argument)
	case "/thinking":
		s.options.thinking = !s.options.thinking
		state := "disabled"
		if s.options.thinking {
			state = "enabled"
		}
		s.commitStatus("✓ Thinking: " + state)
	case "/compact":
		id := s.current.ID
		if argument != "" {
			if strings.ContainsAny(argument, " \t\r\n") {
				s.commitError("usage: /compact [ID]")
				break
			}
			id = argument
		}
		if id == "" {
			s.commitError("no active session")
			break
		}
		compactor, ok := s.api.(interface {
			Compact(context.Context, string) (v1.Compaction, error)
		})
		if !ok {
			s.commitError("connected server does not support compaction")
			break
		}
		result, err := compactor.Compact(s.ctx, id)
		if err != nil {
			s.commitError(err.Error())
		} else if result.Status != "complete" {
			reason := result.Reason
			if reason == "" {
				reason = "compaction did not complete"
			}
			s.commitError(reason)
		} else {
			s.commitStatus("✓ Compaction: " + result.Status)
		}
	case "/goal":
		s.goalAction(argument)
	case "/status":
		sessionID := s.current.ID
		if sessionID == "" {
			sessionID = "none"
		}
		model := s.selection.modelName()
		if model == "" {
			model = "no model"
		}
		effort := s.selection.variant
		if effort == "" {
			effort = "default"
		}
		status := fmt.Sprintf("project: %s\nsession: %s\nagent: %s\nmodel: %s\neffort: %s\nthinking: %t",
			s.projectRoot, sessionID, s.selection.agent, model, effort, s.options.thinking)
		var goalErr error
		if s.current.ID == "" {
			status += "\ngoal: none"
		} else if goals, ok := s.api.(goalClient); !ok {
			status += "\ngoal: unavailable"
			goalErr = errors.New("connected server does not support goals")
		} else if goal, err := goals.Goal(s.ctx, s.current.ID); goalNotFound(err) {
			status += "\ngoal: none"
		} else if err != nil {
			status += "\ngoal: unavailable"
			goalErr = err
		} else {
			status += fmt.Sprintf("\ngoal: %s — %s\ngoal usage: %s, elapsed %s", goal.Status, goal.Objective, formatGoalTokens(goal), formatGoalElapsed(goal.ElapsedSeconds))
		}
		s.commit(status)
		if goalErr != nil {
			s.commitError(goalErr.Error())
		}
	case "/exit":
		return true, finish(s.ctx, exitOK, "chat_exited", nil)
	default:
		s.commitError(fmt.Sprintf("unknown slash command %q", command))
	}
	return false, exitOK
}

const goalUsage = "usage: /goal [show|set [--tokens N] OBJECTIVE|budget N|none|pause|resume|clear]"

func (s *chatShell) goalAction(argument string) {
	if s.current.ID == "" {
		s.commitError("no active session")
		return
	}
	goals, ok := s.api.(goalClient)
	if !ok {
		s.commitError("connected server does not support goals")
		return
	}
	fields := strings.Fields(argument)
	if len(fields) == 0 || len(fields) == 1 && fields[0] == "show" {
		goal, err := goals.Goal(s.ctx, s.current.ID)
		if goalNotFound(err) {
			s.commit("no goal configured")
		} else if err != nil {
			s.commitError(err.Error())
		} else {
			s.commit(formatGoal(goal))
		}
		return
	}
	var request v1.PutGoalRequest
	verb := fields[0]
	switch verb {
	case "set":
		rest := strings.TrimSpace(strings.TrimPrefix(argument, "set"))
		if strings.HasPrefix(rest, "--tokens") {
			parts := strings.Fields(rest)
			if len(parts) < 3 || parts[0] != "--tokens" {
				s.commitError(goalUsage)
				return
			}
			budget, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || budget <= 0 {
				s.commitError(goalUsage)
				return
			}
			request.TokenBudget = &budget
			rest = strings.Join(parts[2:], " ")
		}
		if rest == "" || strings.HasPrefix(rest, "--") {
			s.commitError(goalUsage)
			return
		}
		request.Objective = &rest
	case "budget":
		if len(fields) != 2 {
			s.commitError(goalUsage)
			return
		}
		if fields[1] == "none" {
			request.ClearTokenBudget = true
		} else {
			budget, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || budget <= 0 {
				s.commitError(goalUsage)
				return
			}
			request.TokenBudget = &budget
		}
	case "pause", "resume":
		if len(fields) != 1 {
			s.commitError(goalUsage)
			return
		}
		status := map[string]string{"pause": "paused", "resume": "active"}[verb]
		request.Status = &status
	case "clear":
		if len(fields) != 1 {
			s.commitError(goalUsage)
			return
		}
		if err := goals.DeleteGoal(s.ctx, s.current.ID); err != nil {
			s.commitError(err.Error())
		} else {
			s.commitStatus("✓ Goal cleared")
		}
		return
	default:
		s.commitError(goalUsage)
		return
	}
	goal, err := goals.PutGoal(s.ctx, s.current.ID, request)
	if err != nil {
		s.commitError(err.Error())
		return
	}
	s.commitStatus("✓ Goal updated")
	s.commit(formatGoal(goal))
}

func goalNotFound(err error) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.Problem.Status == http.StatusNotFound
}

func formatGoal(goal v1.Goal) string {
	return fmt.Sprintf("objective: %s\nstatus: %s\ntokens: %s\nelapsed: %s", goal.Objective, goal.Status, formatGoalTokens(goal), formatGoalElapsed(goal.ElapsedSeconds))
}

func formatGoalTokens(goal v1.Goal) string {
	if goal.TokenBudget == nil {
		return fmt.Sprintf("%d tokens (unlimited)", goal.TokensUsed)
	}
	remaining := max(*goal.TokenBudget-goal.TokensUsed, 0)
	if goal.RemainingTokens != nil {
		remaining = *goal.RemainingTokens
	}
	return fmt.Sprintf("%d/%d tokens (%d remaining)", goal.TokensUsed, *goal.TokenBudget, remaining)
}

func formatGoalElapsed(seconds int64) string {
	return (time.Duration(max(seconds, 0)) * time.Second).String()
}

// sessionAction implements the management forms of the CLI session command.
// It returns false for the legacy `/session [ID]` switch form.
func (s *chatShell) sessionAction(argument string) bool {
	fields := strings.Fields(argument)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "list":
		if len(fields) != 1 {
			s.commitError("usage: /session list")
			return true
		}
		s.slash("/sessions", "")
	case "show":
		if len(fields) != 2 {
			s.commitError("usage: /session show ID")
			return true
		}
		item, err := s.api.Session(s.ctx, fields[1])
		if err != nil {
			s.commitError(err.Error())
			return true
		}
		messages, err := s.api.Messages(s.ctx, fields[1])
		if err != nil {
			s.commitError(err.Error())
			return true
		}
		data, err := json.MarshalIndent(struct {
			Session  v1.Session   `json:"session"`
			Messages []v1.Message `json:"messages"`
		}{item, messages.Items}, "", "  ")
		if err != nil {
			s.commitError(err.Error())
		} else {
			s.commit(string(data))
		}
	case "compact":
		if len(fields) > 2 {
			s.commitError("usage: /session compact [ID]")
			return true
		}
		id := s.current.ID
		if len(fields) == 2 {
			id = fields[1]
		}
		if id == "" {
			s.commitError("no active session")
			return true
		}
		compactor, ok := s.api.(interface {
			Compact(context.Context, string) (v1.Compaction, error)
		})
		if !ok {
			s.commitError("connected server does not support compaction")
			return true
		}
		result, err := compactor.Compact(s.ctx, id)
		if err != nil {
			s.commitError(err.Error())
		} else if result.Status != "complete" {
			reason := result.Reason
			if reason == "" {
				reason = "compaction did not complete"
			}
			s.commitError(reason)
		} else {
			s.commitStatus("✓ Compaction: " + result.Status)
		}
	case "delete":
		if len(fields) != 2 {
			s.commitError("usage: /session delete ID")
			return true
		}
		if err := s.api.DeleteSession(s.ctx, fields[1]); err != nil {
			s.commitError(err.Error())
			return true
		}
		if s.current.ID == fields[1] {
			s.current = v1.Session{}
		}
		s.commitStatus("✓ Session deleted: " + fields[1])
	default:
		return false
	}
	return true
}

func (s *chatShell) authAction(argument string) {
	fields := strings.Fields(argument)
	if s.credentials == nil {
		s.commitError("local credential store is unavailable")
		return
	}
	if len(fields) == 0 {
		s.commitError("usage: /auth list|login [PROVIDER [KEY|--no-browser]]|logout PROVIDER")
		return
	}
	switch fields[0] {
	case "list":
		if len(fields) != 1 {
			s.commitError("usage: /auth list")
			return
		}
		names, err := s.credentials.List(s.ctx)
		if err != nil {
			s.commitError(err.Error())
			return
		}
		if len(names) == 0 {
			s.commit("no credentials stored")
			return
		}
		for _, name := range names {
			value, err := s.credentials.Get(s.ctx, name)
			if err != nil {
				s.commitError(err.Error())
				return
			}
			s.commit(fmt.Sprintf("%s\t%s", name, value.Type))
		}
	case "logout":
		if len(fields) != 2 {
			s.commitError("usage: /auth logout PROVIDER")
			return
		}
		if err := s.credentials.Delete(s.ctx, credentialName(fields[1])); err != nil {
			s.commitError(err.Error())
			return
		}
		s.reloadAfterAuth("✓ Credential removed")
	case "login":
		if len(fields) > 3 {
			s.commitError("usage: /auth login [PROVIDER [KEY|--no-browser]]")
			return
		}
		name := ""
		if len(fields) > 1 {
			name = fields[1]
		} else {
			// No provider named: choose one, then read its key below.
			item, err := s.pick("provider> ", s.authProviderCandidates())
			if err != nil {
				if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
					s.commitError(err.Error())
				}
				return
			}
			name = item.Value
		}
		if name != "openai" && name != "chatgpt" {
			if len(fields) == 3 && fields[2] == "--no-browser" {
				s.commitError("--no-browser is only valid for OpenAI")
				return
			}
			// A key typed here stays local: builtin slash commands never reach
			// the model or the session transcript, and input history is only
			// held in memory. It is still visible on screen and in terminal
			// scrollback, so PARROT_API_KEY remains the quieter option.
			key := os.Getenv("PARROT_API_KEY")
			if len(fields) == 3 {
				key = fields[2]
			}
			if key == "" {
				value, err := s.promptLine(name + " API key> ")
				if err != nil {
					if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
						s.commitError(err.Error())
					}
					return
				}
				key = strings.TrimSpace(value)
			}
			if key == "" {
				s.commitError("no API key entered")
				return
			}
			if err := s.credentials.Put(s.ctx, name, auth.NewAPIKeyCredential(key)); err != nil {
				s.commitError(err.Error())
				return
			}
			s.reloadAfterAuth("✓ API key stored")
			return
		}
		noBrowser := len(fields) == 3 && fields[2] == "--no-browser"
		if len(fields) == 3 && !noBrowser {
			s.commitError("usage: /auth login openai [--no-browser]")
			return
		}
		openAI := &auth.OpenAI{OpenBrowser: terminal.OpenBrowser}
		var credential auth.OAuthCredential
		var err error
		if noBrowser {
			device, startErr := openAI.StartDeviceAuthorization(s.ctx)
			if startErr != nil {
				err = startErr
			} else {
				s.commit(fmt.Sprintf("Open %s and enter code %s", device.VerificationURL, device.UserCode.Value()))
				credential, err = openAI.AwaitDeviceAuthorization(s.ctx, device)
			}
		} else {
			credential, err = openAI.BrowserLogin(s.ctx)
		}
		if err != nil {
			s.commitError(err.Error())
			return
		}
		if err := s.credentials.Put(s.ctx, "chatgpt", auth.NewOAuthCredential(credential)); err != nil {
			s.commitError(err.Error())
			return
		}
		s.reloadAfterAuth("✓ OpenAI OAuth credential stored")
	default:
		s.commitError("usage: /auth list|login [PROVIDER [KEY|--no-browser]]|logout PROVIDER")
	}
}

// reloadAfterAuth announces a credential change, then rebuilds the local
// providers so the new key takes effect without a restart. The model list is
// refreshed from the connected server so /models and model picking reflect
// any provider added or removed. When no reloadable local backend is available
// (for example after /connect points at a remote server), the credential is
// still stored and the caller is told to restart.
func (s *chatShell) reloadAfterAuth(stored string) {
	if s.reloadProviders == nil {
		s.commitStatus(stored + "; restart chat to reload providers")
		return
	}
	if err := s.reloadProviders(s.ctx); err != nil {
		s.commitStatus(stored + "; reload failed: " + err.Error())
		return
	}
	// The connected server (local or remote) re-reads its provider registry on
	// each request, so a fresh model list reflects the rebuild. A failure here
	// is non-fatal: the providers themselves are already reloaded.
	if items, err := s.api.Models(s.ctx); err == nil {
		s.models = items.Items
	}
	s.commitStatus(stored + "; providers reloaded")
}

func (s *chatShell) serveAction(argument string) {
	fields := strings.Fields(argument)
	if len(fields) == 1 && fields[0] == "status" {
		if s.listener == nil {
			s.commit("API server: stopped")
		} else {
			s.commit("API server: http://" + s.listener.Addr().String())
		}
		return
	}
	if len(fields) == 1 && fields[0] == "stop" {
		if s.server == nil {
			s.commit("API server is not running")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(ctx); err != nil {
			s.commitError(err.Error())
			return
		}
		s.server, s.listener = nil, nil
		s.commitStatus("✓ API server stopped")
		return
	}
	if s.server != nil {
		s.commitError("API server is already running")
		return
	}
	if s.handler == nil {
		s.commitError("local API handler is unavailable")
		return
	}
	fs := newFlagSet("serve", io.Discard)
	host := fs.String("host", "127.0.0.1", "listen host")
	port := fs.Int("port", 4096, "listen port")
	if err := fs.Parse(fields); err != nil || fs.NArg() != 0 || *port < 1 || *port > 65535 {
		s.commitError("usage: /serve [--host 127.0.0.1] [--port 4096]|status|stop")
		return
	}
	if !loopbackHost(*host) {
		s.commitError("refusing unauthenticated non-loopback binding")
		return
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		s.commitError(err.Error())
		return
	}
	server := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	s.listener, s.server = listener, server
	go func() { _ = server.Serve(listener) }()
	s.commitStatus("✓ Listening on http://" + listener.Addr().String())
}

func (s *chatShell) close() {
	if s.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.server.Shutdown(ctx)
	s.server, s.listener = nil, nil
}

func (s *chatShell) selectModel(argument string) error {
	items, err := s.api.Models(s.ctx)
	if err != nil {
		return err
	}
	s.models = items.Items
	value, err := matchModel(argument, items.Items)
	if err != nil {
		return err
	}
	return s.applyModel(value)
}

func (s *chatShell) pickModel() (string, error) {
	items, err := s.api.Models(s.ctx)
	if err != nil {
		return "", err
	}
	s.models = items.Items
	candidates := make([]terminal.Candidate, 0, len(items.Items))
	for _, item := range items.Items {
		candidates = append(candidates, terminal.Candidate{Value: item.Provider + "/" + item.ID, Description: item.Name})
	}
	if len(candidates) == 0 {
		s.commit("no models available; draft retained")
		return "", terminal.ErrCanceled
	}
	item, err := s.pick("model> ", candidates)
	return item.Value, err
}

func (s *chatShell) applyModel(value string) error {
	provider, model, ok := strings.Cut(value, "/")
	if !ok || provider == "" || model == "" {
		return fmt.Errorf("invalid model %q", value)
	}
	variant := s.selection.variant
	for _, item := range s.models {
		if item.Provider != provider || item.ID != model {
			continue
		}
		variant = resolveVariant(variant, item)
		break
	}
	if s.current.ID != "" {
		if err := applySelection(s.ctx, s.api, s.current.ID, "", value, &variant); err != nil {
			return err
		}
	}
	s.selection.provider, s.selection.model, s.selection.variant = provider, model, variant
	s.current.Provider, s.current.Model, s.current.Variant = provider, model, variant
	if err := s.persistSelection(); err != nil {
		return err
	}
	s.commitStatus("✓ Model selected: " + value)
	s.refreshModelInfo()
	return nil
}

// resolveVariant keeps a carried-over reasoning variant only when the target
// model offers it. A model with no variants clears it, rather than sending a
// selection the server rejects as invalid.
func resolveVariant(current string, model v1.Model) string {
	if len(model.Variants) == 0 {
		return ""
	}
	for _, item := range model.Variants {
		if item.Name == current {
			return current
		}
	}
	return model.Variants[0].Name
}

func modelVariantOrder(model v1.Model) []string {
	efforts := make([]string, 0, len(model.Variants))
	for _, variant := range model.Variants {
		efforts = append(efforts, variant.Name)
	}
	return efforts
}

func (s *chatShell) modelEfforts() ([]string, error) {
	if s.selection.provider == "" || s.selection.model == "" {
		return nil, errors.New("select a model before setting effort")
	}
	items, err := s.api.Models(s.ctx)
	if err != nil {
		return nil, err
	}
	s.models = items.Items
	for _, item := range items.Items {
		if item.Provider != s.selection.provider || item.ID != s.selection.model {
			continue
		}
		efforts := modelVariantOrder(item)
		if len(efforts) == 0 {
			return nil, fmt.Errorf("model %s does not expose reasoning efforts", s.selection.modelName())
		}
		return efforts, nil
	}
	return nil, fmt.Errorf("unknown model %q", s.selection.modelName())
}

func (s *chatShell) pickEffort() (string, error) {
	efforts, err := s.modelEfforts()
	if err != nil {
		return "", err
	}
	candidates := make([]terminal.Candidate, 0, len(efforts))
	for _, effort := range efforts {
		candidates = append(candidates, terminal.Candidate{Value: effort})
	}
	item, err := s.pick("effort> ", candidates)
	return item.Value, err
}

func (s *chatShell) selectEffort(value string) error {
	efforts, err := s.modelEfforts()
	if err != nil {
		return err
	}
	found := false
	for _, effort := range efforts {
		found = found || effort == value
	}
	if !found {
		return fmt.Errorf("unknown effort %q for model %s (available: %s)", value, s.selection.modelName(), strings.Join(efforts, ", "))
	}
	if s.current.ID != "" {
		if err := applySelection(s.ctx, s.api, s.current.ID, "", "", &value); err != nil {
			return err
		}
	}
	s.selection.variant = value
	s.current.Variant = value
	if err := s.persistSelection(); err != nil {
		return err
	}
	s.commitStatus("✓ Model effort selected: " + value)
	return nil
}

func (s *chatShell) persistSelection() error {
	if s.configDir == "" {
		return nil
	}
	return configpkg.UpdateDefaultSelection(filepath.Join(s.configDir, configpkg.FileName), s.selection.modelName(), s.selection.variant)
}

func (s *chatShell) selectAgent(argument string) error {
	return s.applyAgent(argument, true)
}

func (s *chatShell) modes() (v1.AgentList, error) {
	if api, ok := s.api.(interface {
		Modes(context.Context) (v1.ModeList, error)
	}); ok {
		items, err := api.Modes(s.ctx)
		out := v1.AgentList{Items: make([]v1.Agent, len(items.Items))}
		for i, item := range items.Items {
			out.Items[i] = v1.Agent{ID: item.ID, ReadOnly: item.ReadOnly, MaxTurns: item.MaxTurns}
		}
		return out, err
	}
	return s.api.Agents(s.ctx)
}

func (s *chatShell) applyAgent(argument string, announce bool) error {
	items, err := s.modes()
	if err != nil {
		return err
	}
	found := false
	for _, item := range items.Items {
		found = found || item.ID == argument
	}
	if !found {
		return fmt.Errorf("unknown mode %q", argument)
	}
	if s.current.ID != "" {
		if err := applySelection(s.ctx, s.api, s.current.ID, argument, "", nil); err != nil {
			return err
		}
	}
	s.selection.agent = argument
	s.current.Agent = argument
	if announce {
		s.commitStatus("✓ Mode selected: " + argument)
	}
	return nil
}

func (s *chatShell) nextAgent(current string) (string, error) {
	items, err := s.modes()
	if err != nil {
		return "", err
	}
	if len(items.Items) == 0 {
		return "", errors.New("no agents available")
	}
	for i, item := range items.Items {
		if item.ID == current {
			return items.Items[(i+1)%len(items.Items)].ID, nil
		}
	}
	return items.Items[0].ID, nil
}

func matchModel(argument string, items []v1.Model) (string, error) {
	if strings.Contains(argument, "/") {
		for _, item := range items {
			value := item.Provider + "/" + item.ID
			if value == argument {
				return value, nil
			}
		}
		return "", fmt.Errorf("unknown model %q", argument)
	}
	match := ""
	for _, item := range items {
		if item.ID != argument {
			continue
		}
		if match != "" {
			return "", fmt.Errorf("model ID %q is ambiguous; use provider/model", argument)
		}
		match = item.Provider + "/" + item.ID
	}
	if match == "" {
		return "", fmt.Errorf("unknown model %q", argument)
	}
	return match, nil
}

func (s *chatShell) chooseSession(argument string) (v1.Session, error) {
	if argument != "" {
		return s.api.Session(s.ctx, argument)
	}
	items, err := s.api.Sessions(s.ctx)
	if err != nil {
		return v1.Session{}, err
	}
	candidates := make([]terminal.Candidate, 0, len(items.Items))
	for _, item := range items.Items {
		candidates = append(candidates, terminal.Candidate{Value: item.ID, Description: item.Title})
	}
	if len(candidates) == 0 {
		s.commit("no sessions available")
		return v1.Session{}, terminal.ErrCanceled
	}
	picked, err := s.pick("session> ", candidates)
	if err != nil {
		return v1.Session{}, err
	}
	return s.api.Session(s.ctx, picked.Value)
}

func (s *chatShell) pick(prompt string, candidates []terminal.Candidate) (terminal.Candidate, error) {
	if s.enhanced {
		picker := terminal.NewPickerDecoder(s.decoder, s.stdout, candidates,
			terminal.WithPickerPrompt(prompt), terminal.WithPickerRenderer(s.renderer))
		return picker.Pick(s.ctx)
	}
	for index, item := range candidates {
		fmt.Fprintf(s.stdout, "%d\t%s\t%s\n", index+1, item.Value, item.Description)
	}
	fmt.Fprint(s.stdout, prompt)
	line, err := s.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return terminal.Candidate{}, err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return terminal.Candidate{}, terminal.ErrCanceled
	}
	if index, convertErr := strconv.Atoi(value); convertErr == nil && index > 0 && index <= len(candidates) {
		return candidates[index-1], nil
	}
	for _, item := range candidates {
		if item.Value == value {
			return item, nil
		}
	}
	return terminal.Candidate{}, fmt.Errorf("unknown selection %q", value)
}

// promptLine reads one line from the user mid-session. It mirrors pick's split
// between the shared key decoder and the plain reader. Input is echoed; there
// is no masked variant.
func (s *chatShell) promptLine(prompt string) (string, error) {
	if s.enhanced {
		editor := terminal.NewEditorDecoder(s.decoder, s.stdout,
			terminal.WithEditorPrompt(prompt), terminal.WithEditorRenderer(s.renderer))
		return editor.Read(s.ctx)
	}
	fmt.Fprint(s.stdout, prompt)
	line, err := s.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// authProviderCandidates offers the built-in providers plus every provider that
// already has a credential or a usable model, so login works before anything is
// configured. Configured providers always hold a credential already, because
// building them without one is a startup error.
func (s *chatShell) authProviderCandidates() []terminal.Candidate {
	stored := map[string]string{}
	if names, err := s.credentials.List(s.ctx); err == nil {
		for _, name := range names {
			stored[name] = "credential stored"
		}
	}
	ids := map[string]struct{}{"chatgpt": {}}
	for _, id := range app.PresetProviderIDs() {
		ids[id] = struct{}{}
	}
	for name := range stored {
		ids[name] = struct{}{}
	}
	if models, err := s.api.Models(s.ctx); err == nil {
		for _, item := range models.Items {
			ids[item.Provider] = struct{}{}
		}
	}
	names := make([]string, 0, len(ids))
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)
	candidates := make([]terminal.Candidate, 0, len(names))
	for _, id := range names {
		description := stored[id]
		if description == "" {
			description = "no credential"
		}
		if id == "chatgpt" {
			description += "; OAuth"
		}
		candidates = append(candidates, terminal.Candidate{Value: id, Description: description})
	}
	return candidates
}

func slashParts(line string) (string, string) {
	name, arguments, found := strings.Cut(line, " ")
	if !found {
		return line, ""
	}
	return name, strings.TrimSpace(arguments)
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

func formatSubscriptionUsage(usage v1.SubscriptionUsage, now time.Time) string {
	lines := []string{subscriptionProviderName(usage.Provider) + " subscription"}
	if usage.PlanType != "" {
		lines[0] += " (" + usage.PlanType + ")"
	}
	appendWindow := func(name string, window *v1.UsageWindow) {
		if window == nil {
			return
		}
		reset := window.ResetAt.Local().Format("2006-01-02 15:04 MST")
		if window.ResetAt.After(now) {
			reset += " (in " + formatResetDuration(window.ResetAt.Sub(now)) + ")"
		}
		lines = append(lines, fmt.Sprintf("%s: %.1f%% remaining (%.1f%% used), resets %s", name, window.RemainingPercent, window.UsedPercent, reset))
	}
	appendWindow("primary", usage.PrimaryWindow)
	appendWindow("secondary", usage.SecondaryWindow)
	if usage.Credits != nil && usage.Credits.HasCredits {
		lines = append(lines, "credits: "+usage.Credits.Balance)
	}
	if len(lines) == 1 {
		lines = append(lines, "usage windows unavailable")
	}
	return strings.Join(lines, "\n")
}

// subscriptionProviderName renders a provider ID for display, falling back to
// the ID itself for providers without a preferred capitalization.
func subscriptionProviderName(id string) string {
	switch id {
	case "chatgpt":
		return "ChatGPT"
	case "openrouter":
		return "OpenRouter"
	case "opencode-go":
		return "OpenCode Go"
	case "kimi-code":
		return "Kimi For Coding"
	case "kimi-api":
		return "Kimi API"
	case "":
		return "provider"
	default:
		return id
	}
}

func formatResetDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	duration = duration.Round(time.Minute)
	if duration >= 24*time.Hour {
		return fmt.Sprintf("%dd %dh", int(duration/(24*time.Hour)), int(duration/time.Hour)%24)
	}
	if duration >= time.Hour {
		return fmt.Sprintf("%dh %dm", int(duration/time.Hour), int(duration/time.Minute)%60)
	}
	return fmt.Sprintf("%dm", int(duration/time.Minute))
}

func credentialName(value string) string {
	if value == "openai" {
		return "chatgpt"
	}
	return value
}

// permissionPromptFor lists the declared answers, so a tool which offers a
// narrower set is prompted for accordingly without naming the tool here.
func permissionPromptFor(item v1.Permission) string {
	choices := chatview.PermissionChoiceLabels(item)
	values := make([]string, 0, len(choices))
	deny := ""
	for _, choice := range choices {
		values = append(values, choice.Value)
		if deny == "" && choice.Decision == "deny" && !choice.RequiresReason {
			deny = choice.Value
		}
	}
	if deny == "" {
		deny = "deny"
	}
	return strings.Join(values, "/") + "? [" + deny + "]: "
}

// requiresPermissionReason reports whether the chosen answer asks for a reason.
func requiresPermissionReason(item v1.Permission, value string) bool {
	for _, choice := range chatview.PermissionChoiceLabels(item) {
		if choice.Value == value {
			return choice.RequiresReason
		}
	}
	return false
}

// permissionDefaultReply is the answer for unrecognised input: the first deny
// the tool offers, so refusing is always what an unclear answer means.
func permissionDefaultReply(item v1.Permission) v1.PermissionReply {
	for _, choice := range chatview.PermissionChoiceLabels(item) {
		if choice.Decision == "deny" && !choice.RequiresReason {
			return v1.PermissionReply{Decision: choice.Decision}
		}
	}
	return v1.PermissionReply{Decision: "deny"}
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newFlagSet(name string, output io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(output)
	return fs
}

func chatExitReason(code int) string {
	if code == exitInterrupt {
		return "turn_interrupted"
	}
	return "chat_exited"
}

// permissionChoicesFor renders the answers the requesting tool declared, so a
// tool which refuses a broader scope simply does not offer one.
func permissionChoicesFor(item v1.Permission) []terminal.Candidate {
	declared := chatview.PermissionChoiceLabels(item)
	candidates := make([]terminal.Candidate, 0, len(declared))
	for _, choice := range declared {
		candidates = append(candidates, terminal.Candidate{Value: choice.Value, Description: choice.Description})
	}
	return candidates
}

func permissionReplyFromAnswer(value string) v1.PermissionReply {
	answer := strings.ToLower(strings.TrimSpace(value))
	reply := v1.PermissionReply{Decision: "deny"}
	switch answer {
	case "y", "yes", "once", "grant":
		reply.Decision = "allow"
	}
	return reply
}

func flagCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	return exitUsage
}

func flagReason(err error) string {
	if errors.Is(err, flag.ErrHelp) {
		return "help_displayed"
	}
	return "invalid_arguments"
}

func appOpenReason(err error) string {
	// Keep the logged value coarse and content-free while making the most
	// common multi-process failure distinguishable from other startup errors.
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "database is locked") || strings.Contains(text, "sqlite_busy") || strings.Contains(text, "database table is locked") {
		return "app_open_database_busy"
	}
	return "app_open_failed"
}
