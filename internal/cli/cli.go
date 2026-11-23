// Package cli dispatches the persona subcommands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/Personed/Persona/internal/controller"
	"github.com/Personed/Persona/internal/tui"
	"github.com/Personed/Persona/internal/worker"
)

// ErrNotImplemented is returned by subcommands that later migration phases fill in.
var ErrNotImplemented = errors.New("not implemented")

// Version is set at build time with -ldflags "-X github.com/Personed/Persona/internal/cli.Version=...".
var Version = "dev"

// Command is one persona subcommand.
type Command struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, args []string, stdout io.Writer) error
}

func pending(context.Context, []string, io.Writer) error { return ErrNotImplemented }

// Commands lists the subcommands in help order.
var Commands = []Command{
	{Name: "serve", Summary: "Run the HTTP/RPC service and web UI.", Run: serve},
	{Name: "tui", Summary: "Open the terminal UI against a local or remote service.", Run: tui.Run},
	{Name: "chat", Summary: "Send one message to a Bot and stream the reply.", Run: tui.Chat},
	{Name: "worker", Summary: "Run the work-computer process for a Bot.", Run: runWorker},
	{Name: "controller", Summary: "Run the authenticated control service for remote sandboxes.", Run: controller.Run},
	{Name: "models", Summary: "Detect local model servers and sync Pi providers.", Run: models},
	{Name: "migrate", Summary: "Import an existing pi-durable database.", Run: pending},
	{Name: "version", Summary: "Print the build version.", Run: func(_ context.Context, _ []string, stdout io.Writer) error {
		_, err := fmt.Fprintln(stdout, Version)
		return err
	}},
}

// Run executes the subcommand named by args[0] and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || slices.Contains([]string{"help", "-h", "--help"}, args[0]) {
		usage(stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	index := slices.IndexFunc(Commands, func(command Command) bool { return command.Name == args[0] })
	if index < 0 {
		fmt.Fprintf(stderr, "persona: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
	if err := Commands[index].Run(ctx, args[1:], stdout); err != nil {
		fmt.Fprintf(stderr, "persona %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: persona <command> [arguments]")
	fmt.Fprintln(w)
	for _, command := range Commands {
		fmt.Fprintf(w, "  %-11s %s\n", command.Name, command.Summary)
	}
}

func runWorker(ctx context.Context, args []string, stdout io.Writer) error {
	worker.Version = Version
	return worker.Main(ctx, args, os.Stdin, stdout, os.Stderr)
}
