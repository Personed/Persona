// Package tui is the persona tui terminal client for a local or remote Persona service.
package tui

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
)

// DefaultServer is the local service address that persona tui opens without --server.
const DefaultServer = "http://127.0.0.1:4310"

// Run parses the persona tui flags and runs the terminal UI until the user quits.
func Run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("persona tui", flag.ContinueOnError)
	flags.SetOutput(stdout)
	server := flags.String("server", "", "Persona service URL (env PERSONA_SERVER; default the last paired service, else "+DefaultServer+")")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	path, err := serversPath()
	if err != nil {
		return err
	}
	saved, err := loadServers(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	base := strings.TrimRight(cmp.Or(*server, os.Getenv("PERSONA_SERVER"), saved.Current, DefaultServer), "/")
	c := &client.Client{BaseURL: base, Token: cmp.Or(os.Getenv("PERSONA_TOKEN"), saved.Tokens[base])}
	if c.Token != "" && !secure(c.BaseURL) {
		return errInsecure(c.BaseURL)
	}
	m := New(ctx, c)
	m.servers = saved
	_, err = tea.NewProgram(m, tea.WithContext(ctx), tea.WithOutput(stdout)).Run()
	return err
}

func errInsecure(server string) error {
	return fmt.Errorf("refusing to send device credentials over plain HTTP to %s: use https", server)
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
