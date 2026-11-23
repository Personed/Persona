package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/approval"
	"github.com/Personed/Persona/internal/contract"
	"github.com/Personed/Persona/internal/engine/pi"
	"github.com/Personed/Persona/internal/hosting"
	"github.com/Personed/Persona/internal/localmodels"
	"github.com/Personed/Persona/internal/mcp"
	"github.com/Personed/Persona/internal/memory"
	"github.com/Personed/Persona/internal/routines"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/server"
	"github.com/Personed/Persona/internal/skills"
	"github.com/Personed/Persona/internal/store"
)

// serve runs the local service. Procedures without a Go handler answer NOT_IMPLEMENTED.
// Without a model registry in Go yet, models.list is empty.
func serve(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stdout)
	host := flags.String("host", "127.0.0.1", "listen address")
	port := flags.Int("port", 4310, "listen port")
	static := flags.String("static", "apps/web/dist", "web build directory, unless built with -tags embedweb")
	contractDir := flags.String("contract", "contracts/api", "API contract directory")
	headless := flags.Bool("headless", false, "serve only the API, without the web UI")
	dataDir := flags.String("data", "data", "data directory, unless a hosting config sets one")
	piExtension := flags.String("pi-extension", "", "Pi extension (pi-tools.mjs) that enables chat through pi --mode rpc")
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
	service := server.Config{RPC: procedures}
	listenHost, listenPort := *host, *port
	// Hosted mode: a config file (PERSONA_CONFIG or persona.config.json) puts the
	// service behind device authentication, as apps/api's openHostedRuntime does.
	var cfg *hosting.Config
	if _, ok := os.LookupEnv("PERSONA_CONFIG"); ok || fileExists("persona.config.json") {
		loaded, err := hosting.LoadConfig()
		if err != nil {
			return err
		}
		cfg = &loaded
		listenHost, listenPort, *dataDir = cfg.Host, cfg.Port, cfg.DataDir
	}
	// The store holds the runtime lease of the data directory, so no second
	// service (or maintenance) shares it. Take it before anything writes there,
	// as openHostedRuntime takes acquireDataLease before the session bootstrap.
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	records, err := store.Open(ctx, filepath.Join(*dataDir, "persona-go.sqlite"))
	if err != nil {
		return err
	}
	defer func() { _ = records.Close() }()
	if cfg != nil {
		sessions, err := server.NewSessionStore(cfg.DataDir)
		if err != nil {
			return err
		}
		defer func() { _ = sessions.Close() }()
		if err := sessions.Bootstrap(); err != nil {
			return err
		}
		service.Hosting = &server.Hosting{
			PublicURL:             cfg.PublicURL,
			Sessions:              sessions,
			DesktopCredentialFile: cfg.DesktopCredentialFile,
		}
		if service.Hosting.PublicURL == "" {
			service.Hosting.PublicURL = "http://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
		}
		if cfg.StaticDir != "" {
			*static = cfg.StaticDir
		}
	}
	// Go keeps its own files root until the importer copies the TypeScript
	// history: discarding an upload checks only the Go chats, so it must never
	// see artifacts that TypeScript chats reference.
	filesRoot := filepath.Join(*dataDir, "persona-go.sqlite.files")
	bots, err := agent.Open(ctx, records, agent.Options{
		Models:    agent.NewLocalModels(localmodels.DefaultEndpoints()),
		FilesRoot: filesRoot,
	})
	if err != nil {
		return err
	}
	var runtime *agent.Runtime
	if *piExtension != "" {
		// An idle Bot has no Pi: each closes with the last session of its task.
		engine := pi.New(pi.Config{Extensions: []string{*piExtension}, IdleTimeout: -1, LocalModels: true,
			SessionDir: filepath.Join(*dataDir, "pi", "sessions"), StateDir: filepath.Join(*dataDir, "pi", "state")})
		defer engine.Close()
		runtime = agent.NewRuntime(bots, agent.RuntimeOptions{Engine: engine, Native: true,
			Approvals: approval.NewService(records, nil, 0)})
		if err := runtime.Recover(ctx); err != nil {
			return err
		}
	}
	server.RegisterAgent(procedures, bots, runtime)
	server.RegisterEngines(procedures, bots, runtime)
	server.RegisterWork(procedures, bots, runtime, server.Work{
		Memory:   memory.NewService(records, nil),
		Routines: routines.NewService(records, nil),
		Skills:   skills.NewService(records, os.DirFS(filepath.Join(filesRoot, "skills")), nil),
	})
	// Managed MCP secrets share the hosting credential store with the
	// TypeScript service. Until the computer runtime is composed, connections
	// have no Runner or native catalog, and no OAuth callback route is served.
	credentials, err := hosting.OpenCredentials(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = credentials.Close() }()
	server.RegisterMCP(procedures, bots, mcp.NewService(records, credentials, nil), "")
	service.Files = server.AgentFiles(bots.Files)
	if !*headless {
		if service.Static, err = webFS(*static); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(listenPort)))
	if err != nil {
		return err
	}
	if service.Hosting == nil {
		service.PublicURL = "http://" + listener.Addr().String()
	}
	handler, err := server.New(service)
	if err != nil {
		_ = listener.Close()
		return err
	}
	// Managed supervisors have no IPC channel: SIGTERM stops the service on
	// POSIX, and on Windows (or any piped stdin) closing stdin ends it.
	run := ctx
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
		stdinCtx, done := context.WithCancel(ctx)
		defer done()
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			done()
		}()
		run = stdinCtx
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	// Shutdown waits for open requests; event streams such as bots.subscribe
	// never finish on their own, so end them when it starts.
	httpServer.RegisterOnShutdown(procedures.CloseStreams)
	stopped := make(chan error, 1)
	go func() {
		<-run.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Connections that outlive the graceful drain are dropped; the service
		// still exits cleanly.
		if err := httpServer.Shutdown(shutdown); err != nil {
			_ = httpServer.Close()
		}
		stopped <- nil
	}()
	fmt.Fprintf(stdout, "persona: listening on http://%s\n", listener.Addr())
	if *headless {
		fmt.Fprintln(stdout, "persona: headless, no web UI")
	}
	if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-stopped
}

func fileExists(name string) bool {
	info, err := os.Stat(name)
	return err == nil && info.Mode().IsRegular()
}
