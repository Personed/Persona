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

// Instance is a per-Bot engine connection that the pool owns.
type Instance interface {
	Close() error
	Done() <-chan struct{}
}

// Pool keeps at most one Instance per Bot. An Instance lives while it is leased
// and for the idle timeout after the last release, so an idle Bot has no process.
type Pool[T Instance] struct {
	open func(ctx context.Context, botID int64) (T, error)
	idle time.Duration

	mu      sync.Mutex
	slots   map[int64]*slot[T]
	closed  bool
	closing sync.WaitGroup
}

type slot[T Instance] struct {
	ready  chan struct{}
	value  T
	err    error
	leases int
	timer  *time.Timer
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
			s = &slot[T]{ready: make(chan struct{})}
			p.slots[botID] = s
			s.leases++
			p.mu.Unlock()
			return p.start(ctx, botID, s)
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
				p.mu.Lock()
				if p.slots[botID] == s {
					delete(p.slots, botID)
				}
				p.mu.Unlock()
			default:
				return s.value, p.releaser(botID, s), nil
			}
		}
		// The instance failed to start or already exited; it left the map.
		p.mu.Lock()
		s.leases--
		p.mu.Unlock()
	}
}

func (p *Pool[T]) start(ctx context.Context, botID int64, s *slot[T]) (T, func(), error) {
	value, err := p.open(ctx, botID)
	p.mu.Lock()
	s.value, s.err = value, err
	if err != nil {
		delete(p.slots, botID)
		p.mu.Unlock()
		close(s.ready)
		var zero T
		return zero, nil, err
	}
	if p.closed {
		delete(p.slots, botID)
		p.mu.Unlock()
		close(s.ready)
		_ = value.Close()
		var zero T
		return zero, nil, ErrPoolClosed
	}
	p.mu.Unlock()
	close(s.ready)
	go func() {
		<-value.Done()
		p.mu.Lock()
		if p.slots[botID] == s {
			delete(p.slots, botID)
			if s.timer != nil {
				s.timer.Stop()
			}
		}
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
	if s.leases > 0 || p.slots[botID] != s {
		return
	}
	s.timer = time.AfterFunc(p.idle, func() {
		p.mu.Lock()
		if p.slots[botID] != s || s.leases > 0 {
			p.mu.Unlock()
			return
		}
		delete(p.slots, botID)
		p.closeLocked(s)
		p.mu.Unlock()
	})
}

// Stop closes the Bot's instance now, whether or not it is leased. The next
// Acquire starts a new one.
func (p *Pool[T]) Stop(botID int64) {
	p.mu.Lock()
	s, ok := p.slots[botID]
	if ok {
		delete(p.slots, botID)
		if s.timer != nil {
			s.timer.Stop()
		}
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	<-s.ready
	if s.err == nil {
		_ = s.value.Close()
	}
}

// closeLocked closes a ready slot without blocking the pool.
func (p *Pool[T]) closeLocked(s *slot[T]) {
	p.closing.Add(1)
	go func() {
		defer p.closing.Done()
		<-s.ready
		if s.err == nil {
			_ = s.value.Close()
		}
	}()
}

// Live returns how many Bots have an instance starting or running.
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
		delete(p.slots, botID)
		if s.timer != nil {
			s.timer.Stop()
		}
		p.closeLocked(s)
	}
	p.mu.Unlock()
	p.closing.Wait()
}
