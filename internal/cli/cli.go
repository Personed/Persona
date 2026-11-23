// Package cli dispatches the persona subcommands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/Personed/Persona/internal/controller"
	"github.com/Personed/Persona/internal/migrate"
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

// Commands lists the subcommands in help order.
var Commands = []Command{
	{Name: "serve", Summary: "Run the HTTP/RPC service and web UI.", Run: serve},
	{Name: "tui", Summary: "Open the terminal UI against a local or remote service.", Run: tui.Run},
	{Name: "chat", Summary: "Send one message to a Bot and stream the reply.", Run: tui.Chat},
	{Name: "worker", Summary: "Run the work-computer process for a Bot.", Run: runWorker},
	{Name: "controller", Summary: "Run the authenticated control service for remote sandboxes.", Run: controller.Run},
	{Name: "models", Summary: "Detect local model servers and sync Pi providers.", Run: models},
	{Name: "migrate", Summary: "Import an existing pi-durable database.", Run: runMigrate},
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

func runMigrate(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stdout)
	dataDir := os.Getenv("PERSONA_DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	flags.StringVar(&dataDir, "data-dir", dataDir, "data directory holding "+migrate.SourceName+" (default $PERSONA_DATA_DIR or ./data)")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}
	report, err := migrate.Run(ctx, dataDir)
	if err != nil {
		return err
	}
	var out strings.Builder
	if report.BackupCreated {
		fmt.Fprintf(&out, "backup: created %s\n", report.Backup)
	} else {
		fmt.Fprintf(&out, "backup: kept %s\n", report.Backup)
	}
	if report.AlreadyImported {
		fmt.Fprintf(&out, "store: %s was already imported\n", report.Target)
	} else {
		r := report.Store
		fmt.Fprintf(&out, "store: imported %d conversations, %d entries, %d tasks, %d submissions, %d documents (%d revisions) into %s\n",
			r.Conversations, r.Entries, r.Tasks, r.Submissions, r.Documents, r.Revisions, report.Target)
	}
	for _, record := range report.Store.Unmodeled {
		fmt.Fprintf(&out, "not converted: %s has fields the Go store does not read; its record keeps them\n", record)
	}
	f := report.Files
	fmt.Fprintf(&out, "files: %s kept in place: %d Bots, %d workspace files, %d artifacts\n", f.Root, f.Bots, f.WorkspaceFiles, f.Artifacts)
	for _, problem := range f.Problems {
		fmt.Fprintf(&out, "not converted: %s\n", problem)
	}
	_, err = io.WriteString(stdout, out.String())
	return err
}

func runWorker(ctx context.Context, args []string, stdout io.Writer) error {
	worker.Version = Version
	return worker.Main(ctx, args, os.Stdin, stdout, os.Stderr)
}
