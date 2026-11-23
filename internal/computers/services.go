package computers

import (
	"context"
	"sync"

	"github.com/Personed/Persona/internal/computer"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// services.ts: one work-process service per bound computer. Browser work runs
// per Bot; the desktop is shared (bot 0).
type serviceKey struct {
	service string
	botID   int64
}

func keyFor(service string, botID int64) serviceKey {
	if service != "browser" {
		botID = 0
	}
	return serviceKey{service: service, botID: botID}
}

// control is the shared control state of one service (Control in services.ts):
// who owns input, which viewer lease is outstanding, and the revision every
// settle cycle bumps.
type control struct {
	owner     bool // input belongs to the Bot (false while a viewer holds it)
	lease     int64
	releasing bool
	revision  int
}

// serviceWorker is the worker.Client surface the services layer needs. The
// concrete client is worker.Client; tests substitute a fake.
type serviceWorker interface {
	ComputerRequest(ctx context.Context, input worker.ComputerInput) (worker.ComputerResult, error)
	ComputerCatalog(ctx context.Context, dir, service string) ([]*mcp.Tool, error)
	Close() error
}

// errUnavailable reports a missing binding or disabled service.
func errUnavailable(message string) error {
	return rpc.NewError("SERVICE_UNAVAILABLE", message)
}

// services coordinates workers and control leases for computer services.
type services struct {
	c *Catalog

	mu       sync.Mutex
	workers  map[serviceKey]*serviceWorker
	records  map[serviceKey]string // key -> record id
	controls map[string]*control
	leased   int64 // next viewer lease
}

func newServices(c *Catalog) *services {
	return &services{
		c:        c,
		workers:  map[serviceKey]*serviceWorker{},
		records:  map[serviceKey]string{},
		controls: map[string]*control{},
		leased:   1,
	}
}

// binding resolves the Bot's bound computer record and target.
func (s *services) binding(ctx context.Context, botID int64, service string) (*Record, *Target, error) {
	binding, err := s.c.Binding(ctx, botID)
	if err != nil {
		return nil, nil, err
	}
	if binding.ComputerID == nil {
		return nil, nil, errUnavailable("computer not bound")
	}
	if !binding.Services[service] {
		return nil, nil, errUnavailable(service + " service is not enabled")
	}
	record, err := s.c.recordOf(ctx, *binding.ComputerID)
	if err != nil {
		return nil, nil, err
	}
	// The same gates as catalog.ts workspace: a revoked or mid-operation
	// computer must not serve its retained binding.
	if !record.Authorized {
		return nil, nil, errUnavailable("这台电脑的授权已撤销。")
	}
	if s.c.isChanging(record.ID) {
		return nil, nil, rpc.NewError("CONFLICT", "工作电脑正在操作中。")
	}
	if record.Scope != nil {
		if err := s.c.ensureReady(ctx, record.ID); err != nil {
			return nil, nil, err
		}
	}
	return record, s.c.target(*record), nil
}

// workerFor opens (or reuses) the worker process for the Bot's computer.
func (s *services) workerFor(ctx context.Context, botID int64, service string) (serviceWorker, string, error) {
	record, target, err := s.binding(ctx, botID, service)
	if err != nil {
		return nil, "", err
	}
	key := keyFor(service, botID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.workers[key]; w != nil {
		if s.records[key] == record.ID {
			return *w, target.workDir(botID), nil
		}
		_ = (*w).Close()
		delete(s.workers, key)
		delete(s.records, key)
	}
	transport, workDir, err := target.transport(botID)
	if err != nil {
		return nil, "", err
	}
	client, err := worker.Dial(ctx, transport, worker.Options{})
	if err != nil {
		return nil, "", err
	}
	var w serviceWorker = client
	s.workers[key] = &w
	s.records[key] = record.ID
	return w, workDir, nil
}

// controlOf returns the service control state, creating it owned by the Bot.
func (s *services) controlOf(service string) *control {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl := s.controls[service]
	if ctl == nil {
		ctl = &control{owner: true}
		s.controls[service] = ctl
	}
	return ctl
}

// closeRecord ends every worker for the record (disable/close, services.ts
// close: control by the caller's computer is destroyed).
func (s *services) closeRecord(recordID string) {
	s.mu.Lock()
	var dead []serviceWorker
	for key, w := range s.workers {
		if s.records[key] == recordID {
			dead = append(dead, *w)
			delete(s.workers, key)
			delete(s.records, key)
		}
	}
	s.mu.Unlock()
	for _, w := range dead {
		_ = w.Close()
	}
}

// close ends everything (service shutdown).
func (s *services) close() {
	s.mu.Lock()
	var dead []serviceWorker
	for key, w := range s.workers {
		dead = append(dead, *w)
		delete(s.workers, key)
	}
	s.mu.Unlock()
	for _, w := range dead {
		_ = w.Close()
	}
}

// takeover reserves a viewer lease for the service (services.ts takeover):
// serializes on the lane, waits for the service to settle, then records the
// lease so viewAccess is reserved before the grant waits.
func (s *services) takeover(ctx context.Context, service string, botID int64) (int64, error) {
	w, dir, err := s.workerFor(ctx, botID, service)
	if err != nil {
		return 0, err
	}
	// The worker settles dispatched work before control changes.
	if _, err := w.ComputerRequest(ctx, worker.ComputerInput{Action: "settle", Service: service, Dir: dir}); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl := s.controls[service]
	if ctl == nil {
		ctl = &control{owner: true}
		s.controls[service] = ctl
	}
	lease := s.leased
	s.leased++
	ctl.lease = lease
	ctl.owner = false
	return lease, nil
}

// Workspace dials the worker of the Bot's bound work computer and returns it
// with the work directory, as catalog.ts workspace: the file Target behind
// attach_file and skill packages. Unlike services, workspace needs no enabled
// flag; it fails while the computer is being changed.
func (c *Catalog) Workspace(ctx context.Context, botID int64) (computer.Target, string, error) {
	binding, err := c.Binding(ctx, botID)
	if err != nil {
		return nil, "", err
	}
	if binding.ComputerID == nil {
		return nil, "", rpc.NewError("BAD_REQUEST", "请先为 Bot 分配工作电脑。")
	}
	record, err := c.recordOf(ctx, *binding.ComputerID)
	if err != nil {
		return nil, "", err
	}
	if !record.Authorized {
		return nil, "", rpc.NewError("BAD_REQUEST", "这台电脑的授权已撤销。")
	}
	if record.Scope != nil {
		if err := c.ensureReady(ctx, record.ID); err != nil {
			return nil, "", err
		}
	}
	if c.isChanging(record.ID) {
		return nil, "", rpc.NewError("CONFLICT", "工作电脑正在操作中。")
	}
	target := c.target(*record)
	key := serviceKey{service: "workspace", botID: botID}
	s := c.svcs
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.workers[key]; w != nil {
		if s.records[key] != record.ID {
			_ = (*w).Close()
			delete(s.workers, key)
			delete(s.records, key)
		} else if client, ok := (*w).(*worker.Client); ok {
			return client, target.workDir(botID), nil
		}
	}
	transport, workDir, err := target.transport(botID)
	if err != nil {
		return nil, "", err
	}
	client, err := worker.Dial(ctx, transport, worker.Options{})
	if err != nil {
		return nil, "", err
	}
	var w serviceWorker = client
	s.workers[key] = &w
	s.records[key] = record.ID
	return client, workDir, nil
}

// release returns control to the Bot (services.ts release).
func (s *services) release(service string, lease int64) {
	ctl := s.controlOf(service)
	if ctl.lease != lease || ctl.releasing {
		return
	}
	ctl.releasing = true
	go func() {
		// A small grace lets the last viewer input reach the worker before
		// the Bot regains input (services.ts releases after "back" lands).
		ctl.owner = true
		ctl.releasing = false
		ctl.lease = 0
		ctl.revision++
	}()
}
