package cli

import (
	"context"
	"fmt"
	"io"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// BuildInfo is populated through linker flags for release builds.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// App owns command dispatch. Runtime dependencies will be added as explicit
// fields as their phases are implemented.
type App struct {
	build BuildInfo
}

func New(build BuildInfo) *App {
	return &App{build: build}
}

func (a *App) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	_ = ctx
	_ = stdin

	if len(args) == 0 {
		printHelp(stdout)
		return exitOK
	}

	switch args[0] {
	case "help", "-h", "--help":
		printHelp(stdout)
		return exitOK
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "parrot %s\ncommit: %s\nbuilt: %s\n", a.build.Version, a.build.Commit, a.build.Date)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printHelp(stderr)
		return exitUsage
	}
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
