package cli

import (
	"bufio"
	"bytes"
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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/app"
	"github.com/amirulashraf/parrot-coder/internal/appdirs"
	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/client"
	customcommand "github.com/amirulashraf/parrot-coder/internal/command"
	"github.com/amirulashraf/parrot-coder/internal/permission"
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

type App struct {
	build BuildInfo
	open  func(context.Context, app.Options) (*app.App, error)
}

var (
	enableRawMode     = terminal.EnableRawMode
	setBracketedPaste = terminal.SetBracketedPaste
)

func New(build BuildInfo) *App { return &App{build: build, open: app.Open} }

func (a *App) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	ctx = context.WithValue(ctx, interruptKey{}, (<-chan os.Signal)(interrupts))
	args, noColor := removeNoColor(args)
	var controllingTerminal *os.File
	if len(args) == 0 && !terminal.IsTTY(stdin) {
		if _, fileInput := stdin.(*os.File); fileInput {
			if file, err := terminal.OpenInput(); err == nil && terminal.IsTTY(file) {
				controllingTerminal = file
				defer controllingTerminal.Close()
				stdin = controllingTerminal
				if !terminal.IsTTY(stdout) {
					stdout = controllingTerminal
				}
			}
		}
	}
	out := terminal.Writer{W: stdout}
	errout := terminal.Writer{W: stderr}
	if len(args) == 0 {
		if !terminal.IsTTY(stdin) {
			printHelp(out)
			return exitOK
		}
		args = []string{"chat"}
	}
	switch args[0] {
	case "help", "-h", "--help":
		printHelp(out)
		return exitOK
	case "version", "-v", "--version":
		fmt.Fprintf(out, "parrot %s\ncommit: %s\nbuilt: %s\n", a.build.Version, a.build.Commit, a.build.Date)
		return exitOK
	case "run":
		return a.runCommand(ctx, args[1:], stdin, out, errout)
	case "chat":
		// The enhanced renderer owns its ANSI output, so chat receives the
		// underlying writer and sanitizes all committed content itself.
		return a.chatCommand(ctx, args[1:], stdin, stdout, errout, noColor)
	case "models":
		return a.modelsCommand(ctx, args[1:], out, errout)
	case "agents":
		return a.agentsCommand(ctx, args[1:], out, errout)
	case "session":
		return a.sessionCommand(ctx, args[1:], out, errout)
	case "auth":
		return a.authCommand(ctx, args[1:], stdin, out, errout)
	case "serve":
		return a.serveCommand(ctx, args[1:], out, errout)
	default:
		fmt.Fprintf(errout, "unknown command %q\n\n", args[0])
		printHelp(errout)
		return exitUsage
	}
}

type codingFlags struct {
	continued bool
	session   string
	model     string
	agent     string
	variant   string
	thinking  bool
}

func addCodingFlags(fs *flag.FlagSet, options *codingFlags) {
	fs.BoolVar(&options.continued, "continue", false, "continue the most recent session")
	fs.StringVar(&options.session, "session", "", "continue a session ID")
	fs.StringVar(&options.model, "model", "", "select provider/model")
	fs.StringVar(&options.agent, "agent", "", "select an agent profile")
	fs.StringVar(&options.variant, "variant", "", "select a model reasoning variant")
	fs.BoolVar(&options.thinking, "thinking", false, "show reasoning status")
}

func (a *App) runCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("run", stderr)
	args = normalizeLeadingPrompt(args)
	var options codingFlags
	var format, permissionMode string
	var interactive bool
	addCodingFlags(fs, &options)
	fs.StringVar(&format, "format", "text", "output format: text or jsonl")
	fs.StringVar(&permissionMode, "permission", "deny", "mutating tool policy: deny or ask")
	fs.BoolVar(&interactive, "interactive-prompts", false, "answer prompts from the controlling terminal")
	if err := fs.Parse(args); err != nil {
		return flagCode(err)
	}
	if options.continued && options.session != "" || fs.NArg() > 1 || format != "text" && format != "jsonl" || permissionMode != "deny" && permissionMode != "ask" {
		fmt.Fprintln(stderr, "invalid run flags; see parrot run --help")
		return exitUsage
	}
	prompt, err := promptInput(stdin, fs.Args())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(stderr, "run requires a prompt argument or stdin data")
		return exitUsage
	}
	var tty io.ReadCloser
	if interactive {
		file, openErr := terminal.OpenInput()
		if openErr != nil {
			fmt.Fprintln(stderr, "interactive prompts require /dev/tty:", openErr)
			return exitError
		}
		tty = file
		defer tty.Close()
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, Model: options.model, Agent: options.agent, Permission: permission.Decision(permissionMode), NonInteractive: !interactive})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer runtime.Close()
	sessionItem, err := chooseSession(ctx, runtime.Client, runtime.Project.ID, options.continued, options.session, prompt)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if err := applySelection(ctx, runtime.Client, sessionItem.ID, options.agent, options.model, options.variant); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	result := streamTurn(ctx, runtime.Client, sessionItem.ID, prompt, streamOptions{format: format, stdout: stdout, stderr: stderr, promptInput: tty, thinking: options.thinking})
	if result.err != nil {
		if errors.Is(result.err, context.Canceled) {
			return exitInterrupt
		}
		fmt.Fprintln(stderr, result.err)
		return exitError
	}
	return exitOK
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
	Undo(context.Context, string) (v1.SnapshotTransaction, error)
	Redo(context.Context, string) (v1.SnapshotTransaction, error)
	Models(context.Context) (v1.ModelList, error)
	Agents(context.Context) (v1.AgentList, error)
}

type resumableClient interface {
	apiClient
	Resume(context.Context, string) error
}

func applySelection(ctx context.Context, api apiClient, sessionID, agentID, model, variant string) error {
	if agentID == "" && model == "" && variant == "" {
		return nil
	}
	request := v1.UpdateSessionSelectionRequest{Agent: agentID, Model: model}
	if variant != "" {
		request.Variant = &variant
	}
	_, err := api.UpdateSessionSelection(ctx, sessionID, request)
	return err
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
}

type streamResult struct {
	text string
	err  error
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
	interrupts, _ := ctx.Value(interruptKey{}).(<-chan os.Signal)
	requestInterrupt := func() error {
		interruptCount++
		if interruptCount > 1 {
			return errSecondInterrupt
		}
		interrupted = true
		if options.renderer != nil {
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
				if err := writeJSONLine(options.stdout, item); err != nil {
					return streamResult{err: err}
				}
			}
			if options.format != "jsonl" && strings.HasPrefix(item.Type, "session.tool.") {
				line := streamToolStatus(strings.TrimPrefix(item.Type, "session.tool."))
				if options.renderer != nil {
					_ = options.renderer.Update([]string{line})
				} else {
					fmt.Fprintln(options.stderr, line)
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
						if err := options.renderer.UpdateMessage("- ", streamed.String()); err != nil {
							return streamResult{err: err}
						}
					}
				} else if options.format != "jsonl" && (value.Kind != "reasoning" || options.thinking) {
					if options.renderer != nil {
						_ = options.renderer.Update([]string{"status: " + value.Kind})
					} else {
						fmt.Fprintf(options.stderr, "status: %s\n", value.Kind)
					}
				}
			case *v1.SessionStatus:
				if options.format != "jsonl" && value.Kind != "idle" && value.Kind != "finish" && value.Kind != "usage" {
					if options.renderer != nil {
						_ = options.renderer.Update([]string{"status: " + value.Kind})
					} else {
						fmt.Fprintf(options.stderr, "status: %s\n", value.Kind)
					}
				}
				if value.Kind == "error" || value.Kind == "provider_error" {
					statusError = true
				}
				if value.Kind == "idle" || value.Kind == "error" {
					finished := finishStream(api, sessionID, before, streamed.String(), statusError, options)
					if interrupted {
						finished.err = nil
					}
					return finished
				}
			}
		}
	}
}

type eventResult struct {
	event v1.Event
	err   error
}

type messageClient interface {
	Messages(context.Context, string) (v1.MessageList, error)
}

func finishStream(api messageClient, sessionID string, before v1.MessageList, streamed string, statusError bool, options streamOptions) streamResult {
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
	if options.format == "text" && !options.chat {
		if _, err := io.WriteString(options.stdout, terminal.Sanitize(strings.TrimRight(final, "\n")+"\n")); err != nil {
			return streamResult{err: err}
		}
	}
	if options.chat {
		if options.renderer != nil {
			if err := options.renderer.CommitMessage("- ", final, false); err != nil {
				return streamResult{err: err}
			}
		} else if final = strings.TrimRight(final, "\r\n"); final != "" {
			writer := &hangingWriter{w: options.stdout}
			if _, err := writer.Write([]byte(terminal.Sanitize(final))); err != nil {
				return streamResult{err: err}
			}
			if _, err := io.WriteString(options.stdout, "\n"); err != nil {
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

func streamToolStatus(status string) string {
	switch status {
	case "pending":
		return "○ Queued tool"
	case "running":
		return "◌ Working: tool"
	case "success":
		return "✓ Done: tool"
	case "failure":
		return "✗ Failed: tool"
	case "interrupted":
		return "■ Interrupted: tool"
	default:
		return "Status: tool " + status
	}
}

type hangingWriter struct {
	w         io.Writer
	started   bool
	lineStart bool
}

func (w *hangingWriter) Write(data []byte) (int, error) {
	var output strings.Builder
	if len(data) > 0 && !w.started {
		output.WriteString("- ")
		w.started = true
	}
	for _, value := range data {
		if w.lineStart && value != '\n' {
			output.WriteString("  ")
			w.lineStart = false
		}
		output.WriteByte(value)
		if value == '\n' {
			w.lineStart = true
		}
	}
	if err := writeAll(w.w, output.String()); err != nil {
		return 0, err
	}
	return len(data), nil
}

func writeAll(w io.Writer, value string) error {
	n, err := io.WriteString(w, value)
	if err == nil && n != len(value) {
		return io.ErrShortWrite
	}
	return err
}

// permissionContextLines describes the exact, hash-bound tool invocation being
// approved instead of asking the user to decide from only a tool name.
func permissionContextLines(item v1.Permission) []string {
	lines := []string{"permission: " + item.ToolID, "reason: " + item.Reason}
	if len(item.CanonicalInput) > 0 {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, item.CanonicalInput, "", "  "); err != nil {
			formatted.Write(item.CanonicalInput)
		}
		lines = append(lines, "tool request:")
		for _, line := range strings.Split(formatted.String(), "\n") {
			lines = append(lines, "  "+line)
		}
	}
	for _, resource := range item.Resources {
		lines = append(lines, fmt.Sprintf("resource: %s %s %s", resource.Kind, resource.Operation, resource.Identifier))
	}
	return lines
}

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
		fmt.Fprint(output, "allow once/session/workspace/process or enable yolo? [deny]: ")
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		reply := permissionReplyFromAnswer(line)
		if err := api.ReplyPermission(ctx, sessionID, item.ID, reply); err != nil {
			return err
		}
		// Enabling YOLO settles every permission already pending for this
		// session. The remaining entries came from the now-stale list snapshot;
		// replying to them would fail with permission-not-found and make the
		// successful YOLO choice appear to have failed.
		if reply.Scope == "yolo" {
			break
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
		picker := terminal.NewPickerDecoder(options.keyInput, options.stdout, permissionChoices(),
			terminal.WithPickerPrompt("permission decision: "), terminal.WithPickerRenderer(options.renderer))
		choice, readErr := picker.Pick(ctx)
		if errors.Is(readErr, terminal.ErrCanceled) || errors.Is(readErr, io.EOF) {
			choice.Value, readErr = "no", nil
		}
		if readErr != nil {
			return readErr
		}
		reply := permissionReplyFromAnswer(choice.Value)
		if err := api.ReplyPermission(ctx, sessionID, item.ID, reply); err != nil {
			return err
		}
		if reply.Scope == "yolo" {
			break
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

func (a *App) chatCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, noColor bool) int {
	fs := newFlagSet("chat", stderr)
	args = normalizeLeadingPrompt(args)
	var options codingFlags
	addCodingFlags(fs, &options)
	if err := fs.Parse(args); err != nil {
		return flagCode(err)
	}
	if options.continued && options.session != "" || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "invalid chat flags; see parrot chat --help")
		return exitUsage
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, Model: options.model, Agent: options.agent, Permission: permission.Ask, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer runtime.Close()
	api := apiClient(runtime.Client)
	models, err := api.Models(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	var current v1.Session
	if options.continued || options.session != "" {
		current, err = chooseSession(ctx, api, runtime.Project.ID, options.continued, options.session, "")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		if err := applySelection(ctx, api, current.ID, options.agent, options.model, options.variant); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		if options.agent != "" || options.model != "" || options.variant != "" {
			current, err = api.Session(ctx, current.ID)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
		}
	}
	selection := chatSelection{agent: runtime.DefaultSelection.Agent, provider: runtime.DefaultSelection.Provider, model: runtime.DefaultSelection.Model, variant: options.variant}
	if current.ID != "" {
		selection = selectionFromSession(current, selection.agent)
	}
	plainOut := terminal.Writer{W: stdout}
	shell := &chatShell{
		ctx: ctx, api: api, current: current, selection: selection, options: options,
		projectID: runtime.Project.ID, projectRoot: runtime.Project.Root, commands: runtime.Commands,
		models: models.Items,
		stdout: plainOut, stderr: stderr, inputTTY: terminal.IsTTY(stdin), outputTTY: terminal.IsTTY(stdout),
		inputEcho: terminal.InputEchoed(stdin, stdout), columns: terminal.Columns(stdout),
	}
	if inputFile, ok := stdin.(*os.File); ok && terminal.IsTTY(inputFile) && terminal.IsTTY(stdout) && os.Getenv("TERM") != "dumb" {
		raw, rawErr := enableRawMode(inputFile)
		if rawErr != nil {
			fmt.Fprintln(stderr, "enhanced terminal unavailable; using plain input:", rawErr)
		} else {
			if err := setBracketedPaste(stdout, true); err != nil {
				fmt.Fprintln(stderr, "enhanced terminal unavailable; using plain input:", err)
				_ = raw.Close()
			} else {
				defer setBracketedPaste(stdout, false)
				defer raw.Close()
				shell.enhanced = true
				shell.stdout = stdout
				shell.renderer = terminal.NewLiveRenderer(stdout, terminal.RendererConfig{
					TTY: true, Color: terminal.ColorEnabled(stdout, noColor), Columns: terminal.Columns(stdout), MaxRows: 6, MaxInputRows: 12,
					ColumnsFunc: func() int { return terminal.Columns(stdout) },
				})
				defer shell.renderer.Close()
				shell.decoder = terminal.NewKeyDecoder(inputFile)
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
		return shell.runEnhanced(first)
	}
	return shell.run(first)
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
	for _, item := range s.models {
		if item.Provider == s.selection.provider && item.ID == s.selection.model && item.ContextWindow > 0 {
			return fmt.Sprintf("%s (%s/%s)", label, compactTokenCount(currentTokens), compactTokenCount(item.ContextWindow))
		}
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

type chatShell struct {
	ctx         context.Context
	api         apiClient
	current     v1.Session
	selection   chatSelection
	options     codingFlags
	projectID   string
	projectRoot string
	commands    *customcommand.Registry
	models      []v1.Model
	stdout      io.Writer
	stderr      io.Writer
	reader      *bufio.Reader
	decoder     *terminal.KeyDecoder
	editor      *terminal.Editor
	renderer    *terminal.LiveRenderer
	enhanced    bool
	inputTTY    bool
	outputTTY   bool
	inputEcho   bool
	inputEchoed bool
	columns     int
}

func (s *chatShell) run(first string) int {
	draft := first
	readDraft := draft == ""
	for {
		if readDraft {
			line, err := s.readPrompt(draft)
			if errors.Is(err, io.EOF) {
				return exitOK
			}
			if errors.Is(err, terminal.ErrInterrupted) || errors.Is(err, terminal.ErrCanceled) {
				draft = ""
				readDraft = true
				continue
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return exitInterrupt
				}
				s.commitError(err.Error())
				return exitError
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
			if isBuiltinSlash(name) {
				exit, code := s.slash(name, arguments)
				draft = ""
				if exit {
					return code
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
			item, err := createChatSession(s.ctx, s.api, s.projectID, line, s.selection)
			if err != nil {
				s.commitError(err.Error())
				readDraft = true
				continue
			}
			s.current = item
		}
		if err := s.commitUser(line); err != nil {
			s.commitError(err.Error())
			return exitError
		}
		result := streamTurn(s.ctx, s.api, s.current.ID, line, s.streamOptions(false))
		draft = ""
		if result.err != nil {
			if errors.Is(result.err, errSecondInterrupt) || errors.Is(result.err, context.Canceled) {
				return exitInterrupt
			}
			s.commitError(result.err.Error())
		}
	}
}

func (s *chatShell) commitUser(text string) error {
	text = strings.TrimRight(text, "\r\n")
	if s.renderer != nil {
		return s.renderer.CommitMessage("$ ", text, true)
	}
	if !s.inputEchoed || !s.outputTTY {
		fmt.Fprintln(s.stdout, "$ "+strings.ReplaceAll(text, "\n", "\n  "))
	}
	columns := s.columns
	if columns <= 0 {
		columns = 80
	}
	fmt.Fprintln(s.stdout, strings.Repeat("-", max(3, columns-1)))
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
		thinking: s.options.thinking, chat: true, resume: resume, renderer: s.renderer, keyInput: s.decoder}
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

type sessionCreator interface {
	CreateSession(context.Context, v1.CreateSessionRequest) (v1.Session, error)
}

func createChatSession(ctx context.Context, api sessionCreator, projectID, title string, selection chatSelection) (v1.Session, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(title), "\n")
	if len(line) > 80 {
		line = line[:80]
	}
	request := v1.CreateSessionRequest{ProjectID: projectID, Title: line, Agent: selection.agent, Model: selection.modelName()}
	if selection.variant != "" {
		request.Variant = &selection.variant
	}
	return api.CreateSession(ctx, request)
}

var builtinChatCommands = []terminal.Candidate{
	{Value: "/help", Description: "show commands and keybindings"},
	{Value: "/models", Description: "list available models"},
	{Value: "/model", Description: "select a model"},
	{Value: "/agents", Description: "list available agents"},
	{Value: "/agent", Description: "select an agent"},
	{Value: "/sessions", Description: "list sessions"},
	{Value: "/session", Description: "switch sessions"},
	{Value: "/resume", Description: "resume an interrupted session"},
	{Value: "/new", Description: "start a new session"},
	{Value: "/compact", Description: "compact the current conversation"},
	{Value: "/connect", Description: "connect to an API server"},
	{Value: "/thinking", Description: "toggle reasoning status"},
	{Value: "/undo", Description: "undo the last change"},
	{Value: "/redo", Description: "redo the last undone change"},
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
		text.WriteString("Keys: Enter submit/queue; Ctrl-J newline; Ctrl-C clear draft/interrupt turn; Ctrl-D exit when idle; Tab complete; Escape cancel\nCommands:\n")
		for _, item := range chatCompletionCandidates(s.commands) {
			fmt.Fprintf(&text, "%s\t%s\n", item.Value, item.Description)
		}
		s.commit(strings.TrimSuffix(text.String(), "\n"))
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
	case "/agent":
		if argument == "" {
			items, err := s.api.Agents(s.ctx)
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
			picked, pickErr := s.pick("agent> ", candidates)
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
	case "/session", "/resume":
		item, err := s.chooseSession(argument)
		if err != nil {
			if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
				s.commitError(err.Error())
			}
			break
		}
		s.current = item
		s.selection = selectionFromSession(item, s.selection.agent)
		s.commitStatus("✓ Session selected: " + item.ID)
		if command == "/resume" {
			result := streamTurn(s.ctx, s.api, item.ID, "", s.streamOptions(true))
			if errors.Is(result.err, errSecondInterrupt) || errors.Is(result.err, context.Canceled) {
				return true, exitInterrupt
			}
			if result.err != nil {
				s.commitError(result.err.Error())
			}
		}
	case "/new":
		s.current = v1.Session{}
		s.commitStatus("✓ New session")
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
		s.api = remote
		s.current = v1.Session{}
		s.selection = chatSelection{agent: agent}
		s.models = models.Items
		s.commitStatus("✓ Connected: " + argument)
	case "/thinking":
		s.options.thinking = !s.options.thinking
		state := "disabled"
		if s.options.thinking {
			state = "enabled"
		}
		s.commitStatus("✓ Thinking: " + state)
	case "/compact":
		if s.current.ID == "" {
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
		result, err := compactor.Compact(s.ctx, s.current.ID)
		if err != nil {
			s.commitError(err.Error())
		} else {
			s.commitStatus("✓ Compaction: " + result.Status)
		}
	case "/undo", "/redo":
		if s.current.ID == "" {
			s.commitError("no active session")
			break
		}
		var err error
		if command == "/undo" {
			_, err = s.api.Undo(s.ctx, s.current.ID)
		} else {
			_, err = s.api.Redo(s.ctx, s.current.ID)
		}
		if err != nil {
			s.commitError(err.Error())
		} else {
			action := strings.TrimPrefix(command, "/")
			s.commitStatus("✓ " + strings.ToUpper(action[:1]) + action[1:] + " complete")
		}
	case "/status":
		sessionID := s.current.ID
		if sessionID == "" {
			sessionID = "none"
		}
		model := s.selection.modelName()
		if model == "" {
			model = "no model"
		}
		s.commit(fmt.Sprintf("project: %s\nsession: %s\nagent: %s\nmodel: %s\nthinking: %t",
			s.projectRoot, sessionID, s.selection.agent, model, s.options.thinking))
	case "/exit":
		return true, exitOK
	default:
		s.commitError(fmt.Sprintf("unknown slash command %q", command))
	}
	return false, exitOK
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
	if s.current.ID != "" {
		if err := applySelection(s.ctx, s.api, s.current.ID, "", value, ""); err != nil {
			return err
		}
	}
	s.selection.provider, s.selection.model = provider, model
	s.current.Provider, s.current.Model = provider, model
	s.commitStatus("✓ Model selected: " + value)
	return nil
}

func (s *chatShell) selectAgent(argument string) error {
	return s.applyAgent(argument, true)
}

func (s *chatShell) applyAgent(argument string, announce bool) error {
	items, err := s.api.Agents(s.ctx)
	if err != nil {
		return err
	}
	found := false
	for _, item := range items.Items {
		found = found || item.ID == argument
	}
	if !found {
		return fmt.Errorf("unknown agent %q", argument)
	}
	if s.current.ID != "" {
		if err := applySelection(s.ctx, s.api, s.current.ID, argument, "", ""); err != nil {
			return err
		}
	}
	s.selection.agent = argument
	s.current.Agent = argument
	if announce {
		s.commitStatus("✓ Agent selected: " + argument)
	}
	return nil
}

func (s *chatShell) nextAgent(current string) (string, error) {
	items, err := s.api.Agents(s.ctx)
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

func slashParts(line string) (string, string) {
	name, arguments, found := strings.Cut(line, " ")
	if !found {
		return line, ""
	}
	return name, strings.TrimSpace(arguments)
}

func isBuiltinSlash(name string) bool {
	switch name {
	case "/help", "/models", "/model", "/agents", "/agent", "/sessions", "/session", "/resume", "/new", "/compact", "/connect", "/thinking", "/undo", "/redo", "/status", "/exit":
		return true
	default:
		return false
	}
}

func subtaskPrompt(expansion customcommand.Expansion) string {
	var request strings.Builder
	request.WriteString("Delegate the following task using the task tool")
	if expansion.Agent != "" {
		fmt.Fprintf(&request, " with agent %q", expansion.Agent)
	}
	if expansion.Model != "" {
		fmt.Fprintf(&request, " and model %q", expansion.Model)
	}
	request.WriteString(". Return the child task's result.\n\n")
	request.WriteString(expansion.Prompt)
	return request.String()
}

func (a *App) modelsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("models", stderr)
	format := fs.String("format", "lines", "output format: lines or json")
	if err := fs.Parse(args); err != nil {
		return flagCode(err)
	}
	if fs.NArg() != 0 || *format != "lines" && *format != "json" {
		return usageError(stderr, "models accepts --format lines|json")
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, NonInteractive: true, Permission: permission.Deny, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer runtime.Close()
	items, err := runtime.Client.Models(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if *format == "json" {
		return encodeOutput(stdout, stderr, items.Items)
	}
	for _, item := range items.Items {
		fmt.Fprintf(stdout, "%s/%s\t%s\n", item.Provider, item.ID, item.Name)
	}
	return exitOK
}

func (a *App) agentsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents", stderr)
	if err := fs.Parse(args); err != nil {
		return flagCode(err)
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "agents takes no arguments")
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, NonInteractive: true, Permission: permission.Deny, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer runtime.Close()
	items, err := runtime.Client.Agents(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	for _, item := range items.Items {
		fmt.Fprintf(stdout, "%s\tread_only=%t\tmax_turns=%d\n", item.ID, item.ReadOnly, item.MaxTurns)
	}
	return exitOK
}

func (a *App) sessionCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(stdout, "Usage: parrot session list|show <id>|compact <id>|delete <id>\n")
		return exitOK
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, NonInteractive: true, Permission: permission.Deny, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer runtime.Close()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usageError(stderr, "session list takes no arguments")
		}
		items, err := runtime.Client.Sessions(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		for _, item := range items.Items {
			fmt.Fprintf(stdout, "%s\t%s\t%s/%s\t%s\n", item.ID, item.Agent, item.Provider, item.Model, item.Title)
		}
	case "show":
		if len(args) != 2 {
			return usageError(stderr, "session show requires one ID")
		}
		item, err := runtime.Client.Session(ctx, args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		messages, err := runtime.Client.Messages(ctx, args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		return encodeOutput(stdout, stderr, struct {
			Session  v1.Session   `json:"session"`
			Messages []v1.Message `json:"messages"`
		}{item, messages.Items})
	case "delete":
		if len(args) != 2 {
			return usageError(stderr, "session delete requires one ID")
		}
		if err := runtime.Client.DeleteSession(ctx, args[1]); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
	case "compact":
		if len(args) != 2 {
			return usageError(stderr, "session compact requires one ID")
		}
		result, err := runtime.Client.Compact(ctx, args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		fmt.Fprintln(stdout, "compaction", result.Status)
	default:
		return usageError(stderr, "unknown session command")
	}
	return exitOK
}

func (a *App) authCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(stdout, "Usage: parrot auth list|login <openai|provider>|logout <provider>\n")
		return exitOK
	}
	paths, err := appdirs.ResolveAndEnsure(appdirs.Overrides{})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	store := auth.NewFileStore(filepath.Join(paths.Data, app.CredentialFile))
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usageError(stderr, "auth list takes no arguments")
		}
		names, err := store.List(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		for _, name := range names {
			credential, getErr := store.Get(ctx, name)
			if getErr != nil {
				fmt.Fprintln(stderr, getErr)
				return exitError
			}
			fmt.Fprintf(stdout, "%s\t%s\n", name, credential.Type)
		}
	case "logout":
		if len(args) != 2 {
			return usageError(stderr, "auth logout requires one provider")
		}
		if err := store.Delete(ctx, credentialName(args[1])); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		fmt.Fprintln(stdout, "credential removed")
	case "login":
		return authLogin(ctx, store, args[1:], stdin, stdout, stderr)
	default:
		return usageError(stderr, "unknown auth command")
	}
	return exitOK
}

func authLogin(ctx context.Context, store auth.Store, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("auth login", stderr)
	noBrowser := fs.Bool("no-browser", false, "use OpenAI device authorization")
	apiKeyStdin := fs.Bool("api-key-stdin", false, "read a compatible API key from stdin")
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string(nil), args[1:]...), args[0])
	}
	if err := fs.Parse(args); err != nil {
		return flagCode(err)
	}
	if fs.NArg() != 1 {
		return usageError(stderr, "auth login requires one provider")
	}
	name := fs.Arg(0)
	if name != "openai" && name != "chatgpt" {
		if *noBrowser {
			return usageError(stderr, "--no-browser is only valid for OpenAI")
		}
		key := os.Getenv("PARROT_API_KEY")
		if *apiKeyStdin {
			data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
			if err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
			key = strings.TrimSpace(string(data))
		}
		if key == "" {
			fmt.Fprintln(stderr, "compatible provider login requires --api-key-stdin or PARROT_API_KEY; command-line key arguments are not accepted")
			return exitUsage
		}
		if err := store.Put(ctx, name, auth.NewAPIKeyCredential(key)); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		fmt.Fprintln(stdout, "API key stored")
		return exitOK
	}
	if *apiKeyStdin {
		return usageError(stderr, "--api-key-stdin is not valid for OpenAI OAuth")
	}
	openAI := &auth.OpenAI{OpenBrowser: terminal.OpenBrowser}
	var credential auth.OAuthCredential
	var err error
	if *noBrowser {
		device, startErr := openAI.StartDeviceAuthorization(ctx)
		if startErr != nil {
			err = startErr
		} else {
			fmt.Fprintf(stdout, "Open %s and enter code %s\n", device.VerificationURL, device.UserCode.Value())
			credential, err = openAI.AwaitDeviceAuthorization(ctx, device)
		}
	} else {
		credential, err = openAI.BrowserLogin(ctx)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if err := store.Put(ctx, "chatgpt", auth.NewOAuthCredential(credential)); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	fmt.Fprintln(stdout, "OpenAI OAuth credential stored")
	return exitOK
}

func credentialName(value string) string {
	if value == "openai" {
		return "chatgpt"
	}
	return value
}

func (a *App) serveCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	host := fs.String("host", "127.0.0.1", "listen host")
	port := fs.Int("port", 4096, "listen port")
	if err := fs.Parse(args); err != nil {
		return flagCode(err)
	}
	if fs.NArg() != 0 || *port < 1 || *port > 65535 {
		return usageError(stderr, "serve requires a port from 1 to 65535")
	}
	if !loopbackHost(*host) {
		fmt.Fprintln(stderr, "refusing unauthenticated non-loopback binding")
		return exitUsage
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, Permission: permission.Ask})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer runtime.Close()
	listener, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	server := &http.Server{Handler: runtime.Handler, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Fprintln(stdout, "listening on http://"+listener.Addr().String())
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, err)
			return exitError
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		<-done
	case <-interruptChannel(ctx):
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		<-done
	}
	return exitOK
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

func flagCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	return exitUsage
}

func usageError(output io.Writer, message string) int {
	fmt.Fprintln(output, message)
	return exitUsage
}

func encodeOutput(stdout, stderr io.Writer, value any) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	return exitOK
}

func writeJSONLine(output io.Writer, item v1.Event) error {
	return json.NewEncoder(output).Encode(item)
}

func opaqueID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(value), nil
}

func removeNoColor(args []string) ([]string, bool) {
	result := make([]string, 0, len(args))
	disabled := false
	options := true
	for _, argument := range args {
		if options && argument == "--" {
			options = false
			result = append(result, argument)
		} else if options && argument == "--no-color" {
			disabled = true
		} else {
			result = append(result, argument)
		}
	}
	return result, disabled
}

func normalizeLeadingPrompt(args []string) []string {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") {
		return args
	}
	return append(append([]string(nil), args[1:]...), args[0])
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `Parrot Coder

Usage:
  parrot <command>

Commands:
  chat       start an interactive coding session
  run        execute one prompt
  auth       manage provider credentials
  models     list configured models
  agents     list available agents
  session    manage sessions
  serve      start the HTTP API server
  version    print build information
  help       show this help
`)
}

// SignalContext is used by main so serve and startup receive SIGTERM while
// coding commands can still implement command-specific interrupt behavior.
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGTERM)
}

type interruptKey struct{}

var errSecondInterrupt = errors.New("second interrupt")

func interruptChannel(ctx context.Context) <-chan os.Signal {
	value, _ := ctx.Value(interruptKey{}).(<-chan os.Signal)
	return value
}
