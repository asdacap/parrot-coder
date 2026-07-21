package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/app"
	"github.com/amirulashraf/parrot-coder/internal/appdirs"
	"github.com/amirulashraf/parrot-coder/internal/auth"
	chatpkg "github.com/amirulashraf/parrot-coder/internal/cli/chat"
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

// ExitResult describes a controlled CLI exit without including potentially
// private error text. Reason is a stable, machine-readable explanation for the
// exit code; ErrorType is populated when an error caused the exit.
type ExitResult struct {
	Code      int
	Reason    string
	ErrorType string
}

type exitState struct {
	mu        sync.Mutex
	reason    string
	errorType string
}

type exitStateKey struct{}

var (
	enableRawMode     = terminal.EnableRawMode
	setBracketedPaste = terminal.SetBracketedPaste
)

func New(build BuildInfo) *App { return &App{build: build, open: app.Open} }

// Run executes the CLI and returns its conventional process exit code.
func (a *App) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return a.RunResult(ctx, args, stdin, stdout, stderr).Code
}

// RunResult executes the CLI and returns both the code and a structured reason
// suitable for diagnostics. Error strings are deliberately not retained.
func (a *App) RunResult(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) ExitResult {
	state := &exitState{}
	ctx = context.WithValue(ctx, exitStateKey{}, state)
	code := a.run(ctx, args, stdin, stdout, stderr)
	state.mu.Lock()
	defer state.mu.Unlock()
	reason := state.reason
	if reason == "" {
		reason = defaultExitReason(args, code)
	}
	return ExitResult{Code: code, Reason: reason, ErrorType: state.errorType}
}

func (a *App) run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
			return exitWithReason(ctx, exitOK, "help_displayed", nil)
		}
		args = []string{"chat"}
	}
	switch args[0] {
	case "help", "-h", "--help":
		printHelp(out)
		return exitWithReason(ctx, exitOK, "help_displayed", nil)
	case "version", "-v", "--version":
		fmt.Fprintf(out, "parrot %s\ncommit: %s\nbuilt: %s\n", a.build.Version, a.build.Commit, a.build.Date)
		return exitWithReason(ctx, exitOK, "version_displayed", nil)
	case "run":
		return a.runCommand(ctx, args[1:], stdin, out, errout)
	case "chat":
		// The enhanced renderer owns its ANSI output, so chat receives the
		// underlying writer and sanitizes all committed content itself.
		return a.chatCommand(ctx, args[1:], stdin, stdout, errout, noColor)
	case "models":
		return a.modelsCommand(ctx, args[1:], out, errout)
	case "usage":
		return a.usageCommand(ctx, args[1:], out, errout)
	case "agents":
		return a.agentsCommand(ctx, args[1:], out, errout)
	case "modes":
		return a.modesCommand(ctx, args[1:], out, errout)
	case "session":
		return a.sessionCommand(ctx, args[1:], out, errout)
	case "auth":
		return a.authCommand(ctx, args[1:], stdin, out, errout)
	case "serve":
		return a.serveCommand(ctx, args[1:], out, errout)
	default:
		fmt.Fprintf(errout, "unknown command %q\n\n", args[0])
		printHelp(errout)
		return exitWithReason(ctx, exitUsage, "unknown_command", nil)
	}
}

func exitWithReason(ctx context.Context, code int, reason string, err error) int {
	if state, ok := ctx.Value(exitStateKey{}).(*exitState); ok && state != nil {
		state.mu.Lock()
		state.reason = reason
		state.errorType = exitErrorType(err)
		state.mu.Unlock()
	}
	return code
}

func exitErrorType(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	return fmt.Sprintf("%T", err)
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

func defaultExitReason(args []string, code int) string {
	command := "chat"
	if len(args) > 0 {
		command = args[0]
	}
	switch code {
	case exitOK:
		switch command {
		case "help", "-h", "--help":
			return "help_displayed"
		case "version", "-v", "--version":
			return "version_displayed"
		default:
			return "command_completed"
		}
	case exitError:
		return command + "_failed"
	case exitUsage:
		if command != "run" && command != "chat" && command != "models" && command != "usage" && command != "agents" && command != "modes" && command != "session" && command != "auth" && command != "serve" {
			return "unknown_command"
		}
		return "invalid_arguments"
	case exitInterrupt:
		return "interrupted"
	default:
		return "command_returned_exit_code"
	}
}

func (a *App) runCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	result := chatpkg.RunPrompt(chatpkg.PromptConfig{
		Context: ctx, Interrupts: interruptChannel(ctx), Args: args,
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
		Build: chatpkg.BuildInfo{Version: a.build.Version, Commit: a.build.Commit, Date: a.build.Date}, Open: a.open,
	})
	return exitWithReason(ctx, result.Code, result.Reason, result.Err)
}
func (a *App) chatCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, noColor bool) int {
	result := chatpkg.Run(chatpkg.Config{
		Context: ctx, Interrupts: interruptChannel(ctx), Args: args,
		Stdin: stdin, Stdout: stdout, Stderr: stderr, NoColor: noColor,
		Build: chatpkg.BuildInfo{Version: a.build.Version, Commit: a.build.Commit, Date: a.build.Date},
		Open:  a.open, EnableRawMode: enableRawMode, SetBracketedPaste: setBracketedPaste,
	})
	return exitWithReason(ctx, result.Code, result.Reason, result.Err)
}

func (a *App) modelsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("models", stderr)
	format := fs.String("format", "lines", "output format: lines or json")
	if err := fs.Parse(args); err != nil {
		return exitWithReason(ctx, flagCode(err), flagReason(err), nil)
	}
	if fs.NArg() != 0 || *format != "lines" && *format != "json" {
		return usageError(ctx, stderr, "invalid_models_arguments", "models accepts --format lines|json")
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, NonInteractive: true, Permission: permission.Deny, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, appOpenReason(err), err)
	}
	defer runtime.Close()
	items, err := runtime.Client.Models(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, "model_list_failed", err)
	}
	if *format == "json" {
		return encodeOutput(ctx, stdout, stderr, items.Items)
	}
	for _, item := range items.Items {
		fmt.Fprintf(stdout, "%s/%s\t%s\n", item.Provider, item.ID, item.Name)
	}
	return exitOK
}

func (a *App) usageCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("usage", stderr)
	format := fs.String("format", "lines", "output format: lines or json")
	if err := fs.Parse(args); err != nil {
		return exitWithReason(ctx, flagCode(err), flagReason(err), nil)
	}
	if fs.NArg() != 0 || *format != "lines" && *format != "json" {
		return usageError(ctx, stderr, "invalid_usage_arguments", "usage accepts --format lines|json")
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, NonInteractive: true, Permission: permission.Deny, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, appOpenReason(err), err)
	}
	defer runtime.Close()
	usage, err := runtime.Client.SubscriptionUsage(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, "subscription_usage_failed", err)
	}
	if *format == "json" {
		return encodeOutput(ctx, stdout, stderr, usage)
	}
	fmt.Fprintln(stdout, formatSubscriptionUsage(usage, time.Now()))
	return exitOK
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

func (a *App) agentsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents", stderr)
	if err := fs.Parse(args); err != nil {
		return exitWithReason(ctx, flagCode(err), flagReason(err), nil)
	}
	if fs.NArg() != 0 {
		return usageError(ctx, stderr, "invalid_agents_arguments", "agents takes no arguments")
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, NonInteractive: true, Permission: permission.Deny, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, appOpenReason(err), err)
	}
	defer runtime.Close()
	items, err := runtime.Client.Agents(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, "agent_list_failed", err)
	}
	for _, item := range items.Items {
		fmt.Fprintf(stdout, "%s\tread_only=%t\tmax_turns=%d\n", item.ID, item.ReadOnly, item.MaxTurns)
	}
	return exitOK
}

func (a *App) modesCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("modes", stderr)
	if err := fs.Parse(args); err != nil {
		return exitWithReason(ctx, flagCode(err), flagReason(err), nil)
	}
	if fs.NArg() != 0 {
		return usageError(ctx, stderr, "invalid_modes_arguments", "modes takes no arguments")
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, NonInteractive: true, Permission: permission.Deny, AllowNoModel: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, appOpenReason(err), err)
	}
	defer runtime.Close()
	items, err := runtime.Client.Modes(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, "mode_list_failed", err)
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
		return exitWithReason(ctx, exitError, appOpenReason(err), err)
	}
	defer runtime.Close()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usageError(ctx, stderr, "invalid_session_arguments", "session list takes no arguments")
		}
		items, err := runtime.Client.Sessions(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "session_list_failed", err)
		}
		for _, item := range items.Items {
			fmt.Fprintf(stdout, "%s\t%s\t%s/%s\t%s\n", item.ID, item.Agent, item.Provider, item.Model, item.Title)
		}
	case "show":
		if len(args) != 2 {
			return usageError(ctx, stderr, "invalid_session_arguments", "session show requires one ID")
		}
		item, err := runtime.Client.Session(ctx, args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "session_lookup_failed", err)
		}
		messages, err := runtime.Client.Messages(ctx, args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "message_list_failed", err)
		}
		return encodeOutput(ctx, stdout, stderr, struct {
			Session  v1.Session   `json:"session"`
			Messages []v1.Message `json:"messages"`
		}{item, messages.Items})
	case "delete":
		if len(args) != 2 {
			return usageError(ctx, stderr, "invalid_session_arguments", "session delete requires one ID")
		}
		if err := runtime.Client.DeleteSession(ctx, args[1]); err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "session_delete_failed", err)
		}
	case "compact":
		if len(args) != 2 {
			return usageError(ctx, stderr, "invalid_session_arguments", "session compact requires one ID")
		}
		result, err := runtime.Client.Compact(ctx, args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "session_compaction_failed", err)
		}
		fmt.Fprintln(stdout, "compaction", result.Status)
	default:
		return usageError(ctx, stderr, "unknown_session_command", "unknown session command")
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
		return exitWithReason(ctx, exitError, "app_directories_failed", err)
	}
	if err := app.MigrateLegacyCredentials(paths); err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, "credential_migration_failed", err)
	}
	store := auth.NewFileStore(app.CredentialFilePath(paths))
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usageError(ctx, stderr, "invalid_auth_arguments", "auth list takes no arguments")
		}
		names, err := store.List(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "credential_list_failed", err)
		}
		for _, name := range names {
			credential, getErr := store.Get(ctx, name)
			if getErr != nil {
				fmt.Fprintln(stderr, getErr)
				return exitWithReason(ctx, exitError, "credential_read_failed", getErr)
			}
			fmt.Fprintf(stdout, "%s\t%s\n", name, credential.Type)
		}
	case "logout":
		if len(args) != 2 {
			return usageError(ctx, stderr, "invalid_auth_arguments", "auth logout requires one provider")
		}
		if err := store.Delete(ctx, credentialName(args[1])); err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "credential_delete_failed", err)
		}
		fmt.Fprintln(stdout, "credential removed")
	case "login":
		return authLogin(ctx, store, args[1:], stdin, stdout, stderr)
	default:
		return usageError(ctx, stderr, "unknown_auth_command", "unknown auth command")
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
		return exitWithReason(ctx, flagCode(err), flagReason(err), nil)
	}
	if fs.NArg() != 1 {
		return usageError(ctx, stderr, "invalid_auth_arguments", "auth login requires one provider")
	}
	name := fs.Arg(0)
	if name != "openai" && name != "chatgpt" {
		if *noBrowser {
			return usageError(ctx, stderr, "invalid_auth_arguments", "--no-browser is only valid for OpenAI")
		}
		key := os.Getenv("PARROT_API_KEY")
		if *apiKeyStdin {
			data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
			if err != nil {
				fmt.Fprintln(stderr, err)
				return exitWithReason(ctx, exitError, "credential_input_failed", err)
			}
			key = strings.TrimSpace(string(data))
		}
		if key == "" {
			fmt.Fprintln(stderr, "compatible provider login requires --api-key-stdin or PARROT_API_KEY; command-line key arguments are not accepted")
			return exitWithReason(ctx, exitUsage, "credential_required", nil)
		}
		if err := store.Put(ctx, name, auth.NewAPIKeyCredential(key)); err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "credential_write_failed", err)
		}
		fmt.Fprintln(stdout, "API key stored")
		return exitOK
	}
	if *apiKeyStdin {
		return usageError(ctx, stderr, "invalid_auth_arguments", "--api-key-stdin is not valid for OpenAI OAuth")
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
		return exitWithReason(ctx, exitError, "authentication_failed", err)
	}
	if err := store.Put(ctx, "chatgpt", auth.NewOAuthCredential(credential)); err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, "credential_write_failed", err)
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
		return exitWithReason(ctx, flagCode(err), flagReason(err), nil)
	}
	if fs.NArg() != 0 || *port < 1 || *port > 65535 {
		return usageError(ctx, stderr, "invalid_serve_arguments", "serve requires a port from 1 to 65535")
	}
	if !loopbackHost(*host) {
		fmt.Fprintln(stderr, "refusing unauthenticated non-loopback binding")
		return exitWithReason(ctx, exitUsage, "non_loopback_binding_refused", nil)
	}
	runtime, err := a.open(ctx, app.Options{Version: a.build.Version, Permission: permission.Ask})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, appOpenReason(err), err)
	}
	defer runtime.Close()
	listener, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, "listen_failed", err)
	}
	server := &http.Server{Handler: runtime.Handler, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Fprintln(stdout, "listening on http://"+listener.Addr().String())
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "server_failed", err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "server_shutdown_failed", err)
		}
		<-done
	case <-interruptChannel(ctx):
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			fmt.Fprintln(stderr, err)
			return exitWithReason(ctx, exitError, "server_shutdown_failed", err)
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

func flagReason(err error) string {
	if errors.Is(err, flag.ErrHelp) {
		return "help_displayed"
	}
	return "invalid_arguments"
}

func usageError(ctx context.Context, output io.Writer, reason, message string) int {
	fmt.Fprintln(output, message)
	return exitWithReason(ctx, exitUsage, reason, nil)
}

func encodeOutput(ctx context.Context, stdout, stderr io.Writer, value any) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return exitWithReason(ctx, exitError, "output_write_failed", err)
	}
	return exitOK
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

func printHelp(w io.Writer) {
	fmt.Fprint(w, `Parrot Coder

Usage:
  parrot <command>

Commands:
  chat       start an interactive coding session
  run        execute one prompt
  auth       manage provider credentials
  models     list configured models
  usage      show ChatGPT subscription usage
  modes      list foreground modes
  agents     list reusable child agents
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

func interruptChannel(ctx context.Context) <-chan os.Signal {
	value, _ := ctx.Value(interruptKey{}).(<-chan os.Signal)
	return value
}
