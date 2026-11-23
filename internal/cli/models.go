package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Personed/Persona/internal/localmodels"
)

// models detects local model servers and syncs their providers into Pi's
// models.json. Run it on the Bot's work computer, where pi executes.
func models(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: persona models <scan|sync>")
	}
	flags := flag.NewFlagSet("models "+args[0], flag.ContinueOnError)
	flags.SetOutput(stdout)
	agentDir := flags.String("agent-dir", defaultAgentDir(), "Pi agent directory containing models.json")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "scan":
		servers := localmodels.Discover(ctx, localmodels.DefaultEndpoints())
		return json.NewEncoder(stdout).Encode(servers)
	case "sync":
		servers := localmodels.Discover(ctx, localmodels.DefaultEndpoints())
		if len(servers) == 0 {
			fmt.Fprintln(stdout, "persona: no local model servers detected")
			return nil
		}
		path := filepath.Join(*agentDir, "models.json")
		changed, err := localmodels.SyncProviders(path, servers)
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintf(stdout, "persona: %s already up to date\n", path)
			return nil
		}
		for _, server := range servers {
			fmt.Fprintf(stdout, "persona: %s: %d model(s) from %s\n", server.Kind, len(server.Models), server.BaseURL)
		}
		fmt.Fprintf(stdout, "persona: wrote %s\n", path)
		return nil
	default:
		return fmt.Errorf("unknown models subcommand %q (want scan or sync)", args[0])
	}
}

// defaultAgentDir mirrors Pi: $PI_CODING_AGENT_DIR or ~/.pi/agent.
func defaultAgentDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".pi/agent"
	}
	return filepath.Join(home, ".pi", "agent")
}
