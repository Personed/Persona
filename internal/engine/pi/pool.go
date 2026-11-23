package pi

import (
	"context"
	"errors"
	"sync"
	"time"
)

// IdleTimeout is how long a Bot's Pi stays up after its last lease is released.
const IdleTimeout = 5 * time.Minute

// ErrPoolClosed reports an Acquire after Close.
var ErrPoolClosed = errors.New("pi: pool closed")

// ErrStopped reports an Acquire whose instance was stopped while it started.
var ErrStopped = errors.New("pi: bot stopped while starting")

// Instance is a per-Bot engine connection that the pool owns.
type Instance interface {
	// Close stops the instance and everything it started. It must be safe to
	// call after the instance exited on its own.
	Close() error
	Done() <-chan struct{}
}

// Pool keeps at most one Instance per Bot. An Instance lives while it is leased
// and for the idle timeout after the last release, so an idle Bot has no process.
// A retiring instance keeps its slot until Close returns, so a Bot never has two.
type Pool[T Instance] struct {
	open func(ctx context.Context, botID int64) (T, error)
	idle time.Duration

	mu      sync.Mutex
	slots   map[int64]*slot[T]
	closed  bool
	closing sync.WaitGroup
}

type slot[T Instance] struct {
	ready    chan struct{}
	value    T
	err      error
	leases   int
	timer    *time.Timer
	retiring chan struct{}
}

// NewPool returns a pool that starts instances with open and closes them after idle.
func NewPool[T Instance](idle time.Duration, open func(ctx context.Context, botID int64) (T, error)) *Pool[T] {
	return &Pool[T]{open: open, idle: idle, slots: map[int64]*slot[T]{}}
}

// Acquire returns the Bot's instance, starting it if needed, and a release
// function that must be called once the caller is done with it.
func (p *Pool[T]) Acquire(ctx context.Context, botID int64) (T, func(), error) {
	var zero T
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return zero, nil, ErrPoolClosed
		}
		s, ok := p.slots[botID]
		if !ok {
			s = &slot[T]{ready: make(chan struct{}), leases: 1}
			p.slots[botID] = s
			p.mu.Unlock()
			return p.start(ctx, botID, s)
		}
		if retiring := s.retiring; retiring != nil {
			p.mu.Unlock()
			select {
			case <-retiring:
				continue
			case <-ctx.Done():
				return zero, nil, ctx.Err()
			}
		}
		s.leases++
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		p.mu.Unlock()
		select {
		case <-s.ready:
		case <-ctx.Done():
			p.release(botID, s)
			return zero, nil, ctx.Err()
		}
		if s.err == nil {
			select {
			case <-s.value.Done():
			default:
				return s.value, p.releaser(botID, s), nil
			}
		}
		// The instance failed to start or exited; wait for it to leave the pool.
		p.mu.Lock()
		s.leases--
		retiring := p.retireLocked(botID, s)
		p.mu.Unlock()
		select {
		case <-retiring:
		case <-ctx.Done():
			return zero, nil, ctx.Err()
		}
	}
}

func (p *Pool[T]) start(ctx context.Context, botID int64, s *slot[T]) (T, func(), error) {
	var zero T
	value, err := p.open(ctx, botID)
	p.mu.Lock()
	s.value, s.err = value, err
	close(s.ready)
	if err != nil {
		if p.slots[botID] == s {
			delete(p.slots, botID)
		}
		if s.retiring == nil {
			s.retiring = make(chan struct{})
			close(s.retiring)
		}
		p.mu.Unlock()
		return zero, nil, err
	}
	if p.closed || s.retiring != nil {
		closed := p.closed
		retiring := p.retireLocked(botID, s)
		p.mu.Unlock()
		<-retiring
		if closed {
			return zero, nil, ErrPoolClosed
		}
		return zero, nil, ErrStopped
	}
	p.mu.Unlock()
	go func() {
		<-value.Done()
		// Pi exited on its own; Close still ends the tools it left running.
		p.mu.Lock()
		p.retireLocked(botID, s)
		p.mu.Unlock()
	}()
	return value, p.releaser(botID, s), nil
}

func (p *Pool[T]) releaser(botID int64, s *slot[T]) func() {
	var once sync.Once
	return func() { once.Do(func() { p.release(botID, s) }) }
}

func (p *Pool[T]) release(botID int64, s *slot[T]) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s.leases--
	if s.leases > 0 || s.retiring != nil || p.slots[botID] != s {
		return
	}
	s.timer = time.AfterFunc(p.idle, func() {
		p.mu.Lock()
		if s.leases == 0 {
			p.retireLocked(botID, s)
		}
		p.mu.Unlock()
	})
}

// retireLocked closes the slot's instance in the background. The slot stays in
// the map until Close returns; the returned channel closes then.
func (p *Pool[T]) retireLocked(botID int64, s *slot[T]) <-chan struct{} {
	if s.retiring != nil {
		return s.retiring
	}
	s.retiring = make(chan struct{})
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	p.closing.Add(1)
	go func() {
		defer p.closing.Done()
		<-s.ready
		if s.err == nil {
			_ = s.value.Close()
		}
		p.mu.Lock()
		if p.slots[botID] == s {
			delete(p.slots, botID)
		}
		p.mu.Unlock()
		close(s.retiring)
	}()
	return s.retiring
}

// Stop closes the Bot's instance now, whether or not it is leased, and waits
// for it to exit. The next Acquire starts a new one.
func (p *Pool[T]) Stop(botID int64) {
	p.mu.Lock()
	s, ok := p.slots[botID]
	if !ok {
		p.mu.Unlock()
		return
	}
	retiring := p.retireLocked(botID, s)
	p.mu.Unlock()
	<-retiring
}

// stopIf is Stop for the Bot's instance only while it is still match, so a
// late caller never stops a replacement.
func (p *Pool[T]) stopIf(botID int64, match T) {
	p.mu.Lock()
	s, ok := p.slots[botID]
	if !ok || s.err != nil || !isReady(s) || any(s.value) != any(match) {
		p.mu.Unlock()
		return
	}
	retiring := p.retireLocked(botID, s)
	p.mu.Unlock()
	<-retiring
}

func isReady[T Instance](s *slot[T]) bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

// Live returns how many Bots have an instance starting, running or stopping.
func (p *Pool[T]) Live() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.slots)
}

// Close stops every instance and waits for them to exit.
func (p *Pool[T]) Close() {
	p.mu.Lock()
	p.closed = true
	for botID, s := range p.slots {
		p.retireLocked(botID, s)
	}
	p.mu.Unlock()
	p.closing.Wait()
}
