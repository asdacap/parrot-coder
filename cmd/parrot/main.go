package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/amirulashraf/parrot-coder/internal/appdirs"
	"github.com/amirulashraf/parrot-coder/internal/cli"
	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
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
	defer func() {
		if !crashOnPanic {
			if recovered := recover(); recovered != nil {
				diagnostics.Panic("main", recovered)
				fmt.Fprintf(os.Stderr, "parrot: panic in main; see the diagnostics log\n%s", debug.Stack())
				exitCode = 2
			}
		}
		if run != nil {
			run.Finish(exitCode)
		}
	}()
	stopSignals := logSignals()
	defer stopSignals()

	ctx, stop := cli.SignalContext(context.Background())
	defer stop()

	app := cli.New(cli.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	})
	command := "chat"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	diagnostics.Event("command_started", "command", command, "argument_count", max(0, len(os.Args)-1))
	exitCode = app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	contextError := ""
	if ctx.Err() != nil {
		contextError = ctx.Err().Error()
	}
	diagnostics.Event("command_finished", "command", command, "exit_code", exitCode, "context_error", contextError)
	return exitCode
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
