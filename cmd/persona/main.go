// Command persona is the single Go binary that replaces the Node service.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/Personed/Persona/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
