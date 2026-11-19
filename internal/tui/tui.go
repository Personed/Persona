// Package tui is the persona tui terminal client for a local or remote Persona service.
package tui

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
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
	if c.Token != "" && !secure(c.BaseURL) {
		return fmt.Errorf("refusing to send PERSONA_TOKEN over plain HTTP to %s: use https", c.BaseURL)
	}
	_, err := tea.NewProgram(New(ctx, c), tea.WithContext(ctx), tea.WithOutput(stdout)).Run()
	return err
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// secure reports whether requests to server are protected in transit: HTTPS, or HTTP to this computer.
func secure(server string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	if host == "localhost" {
		return u.Scheme == "http"
	}
	ip := net.ParseIP(host)
	return u.Scheme == "http" && ip != nil && ip.IsLoopback()
}
