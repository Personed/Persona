package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Personed/Persona/internal/contract"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/server"
)

// serve runs the local service. Procedures without a Go handler answer NOT_IMPLEMENTED.
func serve(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stdout)
	host := flags.String("host", "127.0.0.1", "listen address")
	port := flags.Int("port", 4310, "listen port")
	static := flags.String("static", "apps/web/dist", "web build directory, unless built with -tags embedweb")
	contractDir := flags.String("contract", "contracts/api", "API contract directory")
	headless := flags.Bool("headless", false, "serve only the API, without the web UI, for a server reached through persona tui")
	if err := flags.Parse(args); err != nil {
		return err
	}
	c, err := contract.Load(*contractDir)
	if err != nil {
		return err
	}
	procedures, err := rpc.New(c)
	if err != nil {
		return err
	}
	var web fs.FS
	if !*headless {
		if web, err = webFS(*static); err != nil {
			return err
		}
	}
	handler, err := server.New(server.Config{RPC: procedures, Static: web})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	stopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopped <- httpServer.Shutdown(shutdown)
	}()
	fmt.Fprintf(stdout, "persona: listening on http://%s\n", listener.Addr())
	if *headless {
		fmt.Fprintf(stdout, "persona: headless, no web UI; connect with persona tui --server http://%s\n", listener.Addr())
	}
	if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-stopped
}
