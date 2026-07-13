package cli

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

func New(build BuildInfo) *App { return &App{build: build, open: app.Open} }

func (a *App) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	ctx = context.WithValue(ctx, interruptKey{}, (<-chan os.Signal)(interrupts))
	args = removeNoColor(args)
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
		return a.chatCommand(ctx, args[1:], stdin, out, errout)
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
	thinking  bool
}

func addCodingFlags(fs *flag.FlagSet, options *codingFlags) {
	fs.BoolVar(&options.continued, "continue", false, "continue the most recent session")
	fs.StringVar(&options.session, "session", "", "continue a session ID")
	fs.StringVar(&options.model, "model", "", "select provider/model")
	fs.StringVar(&options.agent, "agent", "", "select an agent profile")
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
	if err := applySelection(ctx, runtime.Client, sessionItem.ID, options.agent, options.model); err != nil {
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
	Sessions(context.Context) (v1.SessionList, error)
	CreateSession(context.Context, v1.CreateSessionRequest) (v1.Session, error)
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

func applySelection(ctx context.Context, api apiClient, sessionID, agentID, model string) error {
	if agentID == "" && model == "" {
		return nil
	}
	selector, ok := api.(interface {
		SelectSession(context.Context, string, string, string) error
	})
	if !ok {
		return errors.New("connected server does not support session selection")
	}
	return selector.SelectSession(ctx, sessionID, agentID, model)
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
	for {
		select {
		case <-interrupts:
			interruptCount++
			if interruptCount > 1 {
				return streamResult{err: errSecondInterrupt}
			}
			interrupted = true
			fmt.Fprintln(options.stderr, "status: interrupt requested")
			go func() {
				interruptCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = api.Interrupt(interruptCtx, sessionID)
			}()
		case <-ctx.Done():
			interruptCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = api.Interrupt(interruptCtx, sessionID)
			cancel()
			return streamResult{err: ctx.Err()}
		case <-ticker.C:
			if err := settlePrompts(ctx, api, sessionID, options.promptInput, options.stderr); err != nil {
				return streamResult{err: err}
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
				fmt.Fprintln(options.stderr, "tool:", strings.TrimPrefix(item.Type, "session.tool."))
			}
			payload, decodeErr := v1.DecodeEventData(item)
			if decodeErr != nil {
				return streamResult{err: decodeErr}
			}
			switch value := payload.(type) {
			case *v1.MessagePartDelta:
				if value.Kind == "text" {
					streamed.WriteString(value.Delta)
					if options.chat && options.format != "jsonl" {
						if _, err := io.WriteString(options.stdout, terminal.Sanitize(value.Delta)); err != nil {
							return streamResult{err: err}
						}
					}
				} else if options.format != "jsonl" && (value.Kind != "reasoning" || options.thinking) {
					fmt.Fprintf(options.stderr, "status: %s\n", value.Kind)
				}
			case *v1.SessionStatus:
				if options.format != "jsonl" && value.Kind != "idle" && value.Kind != "finish" && value.Kind != "usage" {
					fmt.Fprintf(options.stderr, "status: %s\n", value.Kind)
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

func finishStream(api apiClient, sessionID string, before v1.MessageList, streamed string, statusError bool, options streamOptions) streamResult {
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
		if streamed == "" && final != "" {
			_, _ = io.WriteString(options.stdout, terminal.Sanitize(final))
		} else if strings.HasPrefix(final, streamed) && len(final) > len(streamed) {
			_, _ = io.WriteString(options.stdout, terminal.Sanitize(final[len(streamed):]))
		}
		_, _ = io.WriteString(options.stdout, "\n")
	}
	if finalError != "" {
		fmt.Fprintln(options.stderr, "error:", finalError)
	}
	if statusError || finalError != "" {
		return streamResult{text: final, err: errors.New("session turn failed")}
	}
	return streamResult{text: final}
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
		fmt.Fprintf(output, "permission: %s (%s)\nallow once/session/workspace? [deny]: ", item.ToolID, item.Reason)
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		reply := v1.PermissionReply{Decision: "deny"}
		switch answer {
		case "y", "yes", "once":
			reply.Decision = "allow"
		case "session":
			reply.Decision, reply.Scope = "allow", "session"
		case "workspace":
			reply.Decision, reply.Scope = "allow", "workspace"
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

func (a *App) chatCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, Model: options.model, Agent: options.agent, Permission: permission.Ask})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer runtime.Close()
	api := apiClient(runtime.Client)
	var current v1.Session
	if options.continued || options.session != "" {
		current, err = chooseSession(ctx, api, runtime.Project.ID, options.continued, options.session, "")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
	}
	reader := bufio.NewReader(stdin)
	first := ""
	if fs.NArg() == 1 {
		first = fs.Arg(0)
	}
	for {
		line := first
		first = ""
		if line == "" {
			fmt.Fprint(stdout, "you> ")
			idleRead := make(chan idleLine, 1)
			go func() {
				read, readErr := reader.ReadString('\n')
				idleRead <- idleLine{read, readErr}
			}()
			var read string
			var readErr error
			select {
			case value := <-idleRead:
				read, readErr = value.line, value.err
			case <-interruptChannel(ctx):
				return exitInterrupt
			case <-ctx.Done():
				return exitInterrupt
			}
			if errors.Is(readErr, io.EOF) && read == "" {
				return exitOK
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				fmt.Fprintln(stderr, readErr)
				return exitError
			}
			line = strings.TrimSuffix(read, "\n")
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "/") {
			trimmed := strings.TrimSpace(line)
			name, arguments := slashParts(trimmed)
			if isBuiltinSlash(name) {
				exit, code := chatSlash(ctx, &api, &current, trimmed, stdout, stderr, &options, runtime.Commands)
				if exit {
					return code
				}
				continue
			}
			expansion, expandErr := runtime.Commands.Expand(strings.TrimPrefix(name, "/"), arguments)
			if expandErr != nil {
				fmt.Fprintf(stderr, "unknown slash command %q: %v\n", name, expandErr)
				continue
			}
			if expansion.Subtask {
				line = subtaskPrompt(expansion)
			} else {
				if expansion.Agent != "" {
					options.agent = expansion.Agent
				}
				if expansion.Model != "" {
					options.model = expansion.Model
				}
				if current.ID != "" {
					if err := applySelection(ctx, api, current.ID, expansion.Agent, expansion.Model); err != nil {
						fmt.Fprintln(stderr, err)
						continue
					}
				}
				line = expansion.Prompt
			}
		}
		if current.ID == "" {
			current, err = chooseSession(ctx, api, runtime.Project.ID, false, "", line)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
			if err := applySelection(ctx, api, current.ID, options.agent, options.model); err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
		}
		fmt.Fprint(stdout, "assistant> ")
		result := streamTurn(ctx, api, current.ID, line, streamOptions{format: "text", stdout: stdout, stderr: stderr, promptInput: reader, thinking: options.thinking, chat: true})
		if result.err != nil {
			if errors.Is(result.err, errSecondInterrupt) {
				return exitInterrupt
			}
			if errors.Is(result.err, context.Canceled) {
				return exitInterrupt
			}
			fmt.Fprintln(stderr, result.err)
		}
	}
}

type idleLine struct {
	line string
	err  error
}

func chatSlash(ctx context.Context, api *apiClient, current *v1.Session, line string, stdout, stderr io.Writer, options *codingFlags, commands *customcommand.Registry) (bool, int) {
	fields := strings.Fields(line)
	command := fields[0]
	argument := strings.TrimSpace(strings.TrimPrefix(line, command))
	switch command {
	case "/help":
		fmt.Fprint(stdout, "/help /model /agent /new /resume /connect /thinking /undo /redo /exit\n")
		for _, item := range commands.List() {
			fmt.Fprintf(stdout, "/%s\t%s\n", item.Name, item.Description)
		}
	case "/model":
		if argument == "" {
			fmt.Fprintf(stdout, "model: %s\n", options.model)
		} else {
			options.model = argument
			if current.ID != "" {
				if err := applySelection(ctx, *api, current.ID, "", argument); err != nil {
					fmt.Fprintln(stderr, err)
					break
				}
			}
			fmt.Fprintln(stderr, "status: model selected", argument)
		}
	case "/agent":
		if argument == "" {
			fmt.Fprintf(stdout, "agent: %s\n", options.agent)
		} else {
			options.agent = argument
			if current.ID != "" {
				if err := applySelection(ctx, *api, current.ID, argument, ""); err != nil {
					fmt.Fprintln(stderr, err)
					break
				}
			}
			fmt.Fprintln(stderr, "status: agent selected", argument)
		}
	case "/new":
		*current = v1.Session{}
		fmt.Fprintln(stderr, "status: new session")
	case "/resume":
		if argument != "" {
			item, err := (*api).Session(ctx, argument)
			if err != nil {
				fmt.Fprintln(stderr, err)
				break
			}
			*current = item
		}
		if current.ID == "" {
			fmt.Fprintln(stderr, "resume requires a session ID")
			break
		}
		fmt.Fprint(stdout, "assistant> ")
		result := streamTurn(ctx, *api, current.ID, "", streamOptions{format: "text", stdout: stdout, stderr: stderr, chat: true, resume: true})
		if result.err != nil {
			fmt.Fprintln(stderr, result.err)
		}
	case "/connect":
		if argument == "" {
			fmt.Fprintln(stderr, "connect requires an http:// or https:// API URL")
			break
		}
		remote, err := client.New(argument, nil)
		if err != nil {
			fmt.Fprintln(stderr, err)
			break
		}
		*api = remote
		*current = v1.Session{}
		fmt.Fprintln(stderr, "status: connected", argument)
	case "/thinking":
		options.thinking = !options.thinking
		fmt.Fprintf(stderr, "status: thinking %t\n", options.thinking)
	case "/undo", "/redo":
		if current.ID == "" {
			fmt.Fprintln(stderr, "no active session")
			break
		}
		var err error
		if command == "/undo" {
			_, err = (*api).Undo(ctx, current.ID)
		} else {
			_, err = (*api).Redo(ctx, current.ID)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
		} else {
			fmt.Fprintln(stderr, "status:", strings.TrimPrefix(command, "/"), "complete")
		}
	case "/exit":
		return true, exitOK
	default:
		fmt.Fprintf(stderr, "unknown slash command %q\n", command)
	}
	return false, exitOK
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
	case "/help", "/model", "/agent", "/new", "/resume", "/connect", "/thinking", "/undo", "/redo", "/exit":
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
		fmt.Fprint(stdout, "Usage: parrot session list|show <id>|delete <id>\n")
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

func removeNoColor(args []string) []string {
	result := make([]string, 0, len(args))
	for _, argument := range args {
		if argument != "--no-color" {
			result = append(result, argument)
		}
	}
	return result
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
