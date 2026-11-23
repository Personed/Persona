package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/approval"
	"github.com/Personed/Persona/internal/computers"
	"github.com/Personed/Persona/internal/contract"
	"github.com/Personed/Persona/internal/delivery"
	"github.com/Personed/Persona/internal/engine/pi"
	"github.com/Personed/Persona/internal/history"
	"github.com/Personed/Persona/internal/hosting"
	"github.com/Personed/Persona/internal/localmodels"
	"github.com/Personed/Persona/internal/mcp"
	"github.com/Personed/Persona/internal/memory"
	"github.com/Personed/Persona/internal/routines"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/server"
	"github.com/Personed/Persona/internal/skills"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/takeover"
	"github.com/Personed/Persona/internal/tools"
)

// productTools is the registry each Bot task's MCP gateway serves to Pi.
func productTools(records store.Store, routineSvc *routines.Service, skillSvc *skills.Service, handoff takeover.Handoff) (tools.Registry, error) {
	registry := tools.NewRegistry()
	for _, register := range []func() error{
		func() error { return takeover.Register(registry, handoff) },
		func() error { return routines.Register(registry, routineSvc) },
		func() error { return skills.Register(registry, skillSvc) },
		func() error { return history.Register(registry, history.NewReader(records)) },
	} {
		if err := register(); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

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
	var piExtensions []string
	flags.Func("pi-extension", "Pi extension (pi-tools.mjs) that enables chat through pi --mode rpc; repeat to load more", func(path string) error {
		piExtensions = append(piExtensions, path)
		return nil
	})
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
	// Managed MCP secrets and Jev keys share the hosting credential store with
	// the TypeScript service.
	credentials, err := hosting.OpenCredentials(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = credentials.Close() }()
	serviceCfg := computers.ServiceConfig{
		DataDir:    *dataDir,
		Deployment: "native",
		Computer:   computers.Config{Kind: "process"},
		Legacy:     true,
	}
	if cfg != nil {
		serviceCfg.Deployment = cfg.Deployment
		serviceCfg.Credentials = credentials
		computer, err := computerConfig(cfg.Computer)
		if err != nil {
			return err
		}
		serviceCfg.Computer = computer
		// preparation.ts template: workComputer ?? (kind !== "process" ? computer).
		switch {
		case cfg.WorkComputer != nil:
			tmpl, err := computerConfig(*cfg.WorkComputer)
			if err != nil {
				return err
			}
			serviceCfg.Template = &tmpl
		case serviceCfg.Computer.Kind != "process":
			tmpl := serviceCfg.Computer
			serviceCfg.Template = &tmpl
		}
	}
	catalog := computers.New(serviceCfg, bots.Harness)
	approvals := approval.NewService(records, nil, 0)
	// Work services shared by the RPC layer and the product tools below.
	memorySvc := memory.NewService(records, nil)
	routineSvc := routines.NewService(records, nil)
	routineSvc.Confirm = approvals.Request
	skillSvc := skills.NewService(records, os.DirFS(filepath.Join(filesRoot, "skills")), catalog.Workspace)
	skillSvc.UsePackageDir(filepath.Join(filesRoot, "skills"))
	work := server.Work{Memory: memorySvc, Routines: routineSvc, Skills: skillSvc}
	var runtime *agent.Runtime
	if len(piExtensions) > 0 {
		// An idle Bot has no Pi: each closes with the last session of its task.
		engine := pi.New(pi.Config{Extensions: piExtensions, IdleTimeout: -1, LocalModels: true,
			SessionDir: filepath.Join(*dataDir, "pi", "sessions"), StateDir: filepath.Join(*dataDir, "pi", "state")})
		defer engine.Close()
		registry, err := productTools(records, routineSvc, skillSvc, catalog.Handoff(approvals))
		if err != nil {
			return err
		}
		if err := memory.Register(registry, memorySvc); err != nil {
			return err
		}
		if err := delivery.Register(registry, delivery.NewService(delivery.Options{
			Store: records, Root: filesRoot,
			Workspace: func(ctx context.Context, botID int64) (delivery.Workspace, error) {
				target, dir, err := catalog.Workspace(ctx, botID)
				return delivery.Workspace{Target: target, Dir: dir}, err
			},
		})); err != nil {
			return err
		}
		runtime = agent.NewRuntime(bots, agent.RuntimeOptions{Engine: engine, Native: true,
			Tools: registry, Approvals: approvals, Memory: memorySvc})
		if err := runtime.Recover(ctx); err != nil {
			return err
		}
		runner := routines.NewRunner(routineSvc,
			func(ctx context.Context, in routines.RunSubmit) (store.SubmissionID, error) {
				return runtime.Submit(ctx, agent.SendInput{BotID: in.BotID, Text: in.Text,
					RequestID: in.RequestID, WhenBusy: "followUp", DisplayText: in.DisplayText,
					Source: "routine", RoutineRunID: in.RoutineRunID})
			},
			runtime.Stop, approvals)
		routineSvc.OnChange = runner.Kick
		go runner.Start(ctx)
	}
	defer catalog.Close(context.Background())
	var botIDs []int64
	if contacts, err := bots.Bots.List(ctx); err == nil {
		for _, contact := range contacts {
			botIDs = append(botIDs, contact.ID)
		}
	}
	if err := catalog.Initialize(ctx, botIDs); err != nil {
		return err
	}
	server.RegisterAgent(procedures, bots, runtime, catalog)
	server.RegisterComputer(procedures, catalog)
	server.RegisterEngines(procedures, bots, runtime)
	// The takeover card lifecycle is the computers slice's part of engines.*.
	server.RegisterComputerTakeover(procedures, catalog, approvals)
	server.RegisterWork(procedures, bots, runtime, work)
	connections := mcp.NewService(records, credentials, nil)
	connections.Runner = catalog.MCPRunner()
	service.MCP = connections
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
	public := service.PublicURL
	if service.Hosting != nil {
		public = service.Hosting.PublicURL
	}
	base, err := url.Parse(public)
	if err != nil {
		_ = listener.Close()
		return err
	}
	server.RegisterMCP(procedures, bots, connections, base.ResolveReference(&url.URL{Path: "/mcp/oauth/callback"}).String())
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

// computerConfig maps a hosting computer config onto the catalog's Config:
// the JSON tags line up one to one.
func computerConfig(in hosting.ComputerConfig) (computers.Config, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return computers.Config{}, err
	}
	var out computers.Config
	return out, json.Unmarshal(raw, &out)
}
