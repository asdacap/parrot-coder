package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/amirulashraf/parrot-coder/internal/appdirs"
	"github.com/amirulashraf/parrot-coder/internal/cli"
	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/process"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	// Abort with a core dump on unrecovered panics and fatal errors instead
	// of a plain exit(2). SetTraceback can only raise the level, so a
	// GOTRACEBACK environment override stays in effect.
	debug.SetTraceback("crash")
	enableCoreDumps()
	// GOTRACEBACK=crash means the user wants cores from every panic, so the
	// main goroutine must not recover either.
	crashOnPanic := os.Getenv("GOTRACEBACK") == "crash"
	run := startDiagnostics()
	warnMissingCLIUtilities(os.Stderr, exec.LookPath)
	stopSignals := logSignals()
	defer stopSignals()
	ctx, stop := cli.SignalContext(context.Background())
	defer stop()

	command := "chat"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	exitReason := "command_not_completed"
	errorType := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			if crashOnPanic {
				// Keep the run marker and crash output open so the runtime records
				// this as an unclean crash rather than an orderly exit.
				panic(recovered)
			}
			diagnostics.Panic("main", recovered)
			fmt.Fprintf(os.Stderr, "parrot: panic in main; see the diagnostics log\n%s", debug.Stack())
			exitCode = 2
			exitReason = "panic_recovered"
			errorType = fmt.Sprintf("%T", recovered)
		}
		contextError := diagnostics.ErrorType(ctx.Err())
		if contextError == "" && exitCode == 130 {
			contextError = "interrupted"
		}
		attributes := []any{"command", command, "exit_code", exitCode, "exit_reason", exitReason, "context_error", contextError}
		if errorType != "" {
			attributes = append(attributes, "error_type", errorType)
		}
		// A failing command must be discoverable as an error in the log; an
		// interrupt (130) is a user-requested stop, not a failure.
		logExit := diagnostics.Event
		if exitCode != 0 && exitCode != 130 {
			logExit = diagnostics.Error
		}
		logExit("command_finished", attributes...)
		if run != nil {
			run.Finish(exitCode, exitReason, errorType)
		}
	}()
	app := cli.New(cli.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	})
	diagnostics.Event("command_started", "command", command, "argument_count", max(0, len(os.Args)-1))
	result := app.RunResult(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	exitCode, exitReason, errorType = result.Code, result.Reason, result.ErrorType
	return exitCode
}

func warnMissingCLIUtilities(stderr *os.File, lookPath func(string) (string, error)) {
	_, missing := process.InspectCLIUtilities(lookPath)
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(stderr, "warning: expected CLI utilities are unavailable: %s; Bash shell commands may fail\n", strings.Join(missing, ", "))
	diagnostics.Warn("cli_utilities_missing", "utilities", strings.Join(missing, ","), "count", len(missing))
}

func startDiagnostics() *diagnostics.Run {
	paths, err := appdirs.Resolve(appdirs.Overrides{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "parrot: diagnostics unavailable:", err)
		return nil
	}
	if err := os.MkdirAll(paths.State, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "parrot: diagnostics unavailable:", err)
		return nil
	}
	if err := os.Chmod(paths.State, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "parrot: diagnostics unavailable:", err)
		return nil
	}
	run, err := diagnostics.Start(paths.State, diagnostics.Build{Version: version, Commit: commit, Date: date})
	if err != nil {
		fmt.Fprintln(os.Stderr, "parrot: diagnostics unavailable:", err)
		return nil
	}
	return run
}

func logSignals() func() {
	signals := make(chan os.Signal, 4)
	done := make(chan struct{})
	stopped := make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer close(stopped)
		for {
			select {
			case received := <-signals:
				diagnostics.Critical("signal_received", "signal", received.String())
			case <-done:
				for {
					select {
					case received := <-signals:
						diagnostics.Critical("signal_received", "signal", received.String())
					default:
						return
					}
				}
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
		<-stopped
	}
}
