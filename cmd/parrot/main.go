package main

import (
	"context"
	"os"

	"github.com/amirulashraf/parrot-coder/internal/cli"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	ctx, stop := cli.SignalContext(context.Background())
	defer stop()

	app := cli.New(cli.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	})
	os.Exit(app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
