// Package tui is the persona tui terminal client for a local or remote Persona service.
package tui

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
)

// DefaultServer is the local service address that persona tui opens without --server.
const DefaultServer = "http://127.0.0.1:4310"

// Run parses the persona tui flags and runs the terminal UI until the user quits.
func Run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("persona tui", flag.ContinueOnError)
	flags.SetOutput(stdout)
	server := flags.String("server", envOr("PERSONA_SERVER", DefaultServer), "Persona service URL (env PERSONA_SERVER)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	c := &client.Client{BaseURL: *server, Token: os.Getenv("PERSONA_TOKEN")}
	_, err := tea.NewProgram(New(ctx, c), tea.WithContext(ctx), tea.WithOutput(stdout)).Run()
	return err
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
