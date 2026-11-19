package pi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeInstance struct {
	once   sync.Once
	done   chan struct{}
	closes atomic.Int32
	slow   chan struct{}
}

func newFakeInstance() *fakeInstance { return &fakeInstance{done: make(chan struct{})} }

func (f *fakeInstance) Close() error {
	if f.slow != nil {
		<-f.slow
	}
	f.closes.Add(1)
	f.exit()
	return nil
}

func (f *fakeInstance) exit()                 { f.once.Do(func() { close(f.done) }) }
func (f *fakeInstance) Done() <-chan struct{} { return f.done }

type opener struct {
	mu        sync.Mutex
	instances []*fakeInstance
	gate      chan struct{}
	err       error
}

func (o *opener) open(ctx context.Context, _ int64) (*fakeInstance, error) {
	if o.gate != nil {
		select {
		case <-o.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if o.err != nil {
		return nil, o.err
	}
	f := newFakeInstance()
	o.mu.Lock()
	o.instances = append(o.instances, f)
	o.mu.Unlock()
	return f, nil
}

func (o *opener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.instances)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPoolReusesAndClosesAfterIdle(t *testing.T) {
	o := &opener{}
	pool := NewPool(30*time.Millisecond, o.open)
	defer pool.Close()
	ctx := context.Background()
	a, releaseA, err := pool.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, releaseB, err := pool.Acquire(ctx, 1)
	if err != nil || a != b {
		t.Fatalf("second lease got %p, %v; want %p", b, err, a)
	}
	other, releaseOther, _ := pool.Acquire(ctx, 2)
	if other == a {
		t.Fatal("Bots share an instance")
	}
	releaseA()
	releaseA()
	time.Sleep(60 * time.Millisecond)
	if a.closes.Load() != 0 {
		t.Fatal("closed while still leased")
	}
	releaseB()
	releaseOther()
	waitFor(t, "idle close", func() bool { return pool.Live() == 0 && a.closes.Load() == 1 && other.closes.Load() == 1 })
}

func TestPoolAcquireCancelsIdleClose(t *testing.T) {
	o := &opener{}
	pool := NewPool(50*time.Millisecond, o.open)
	defer pool.Close()
	a, release, _ := pool.Acquire(context.Background(), 1)
	release()
	time.Sleep(20 * time.Millisecond)
	b, release, _ := pool.Acquire(context.Background(), 1)
	time.Sleep(80 * time.Millisecond)
	if a != b || a.closes.Load() != 0 {
		t.Fatalf("reacquire before idle timeout restarted the instance")
	}
	release()
	waitFor(t, "idle close", func() bool { return a.closes.Load() == 1 })
}

func TestPoolStartsOncePerBot(t *testing.T) {
	o := &opener{gate: make(chan struct{})}
	pool := NewPool(time.Hour, o.open)
	defer pool.Close()
	var wg sync.WaitGroup
	got := make([]*fakeInstance, 8)
	for i := range got {
		wg.Go(func() {
			value, release, err := pool.Acquire(context.Background(), 7)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			got[i] = value
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(o.gate)
	wg.Wait()
	if o.count() != 1 {
		t.Fatalf("started %d instances", o.count())
	}
	for _, value := range got {
		if value != got[0] {
			t.Fatal("callers got different instances")
		}
	}
}

func TestPoolDropsExitedInstance(t *testing.T) {
	o := &opener{}
	pool := NewPool(time.Hour, o.open)
	defer pool.Close()
	a, release, _ := pool.Acquire(context.Background(), 1)
	release()
	a.exit()
	b, release, err := pool.Acquire(context.Background(), 1)
	if err != nil || a == b {
		t.Fatalf("got exited instance again: %v", err)
	}
	release()
}

func TestPoolStartFailure(t *testing.T) {
	o := &opener{err: errors.New("no pi")}
	pool := NewPool(time.Hour, o.open)
	defer pool.Close()
	if _, _, err := pool.Acquire(context.Background(), 1); err == nil || pool.Live() != 0 {
		t.Fatalf("Acquire = %v, live %d", err, pool.Live())
	}
}

func TestPoolStopAndClose(t *testing.T) {
	o := &opener{}
	pool := NewPool(time.Hour, o.open)
	a, _, _ := pool.Acquire(context.Background(), 1)
	b, _, _ := pool.Acquire(context.Background(), 2)
	pool.Stop(1)
	if a.closes.Load() != 1 || pool.Live() != 1 {
		t.Fatal("Stop left the instance running")
	}
	pool.Close()
	if b.closes.Load() != 1 || pool.Live() != 0 {
		t.Fatal("Close left an instance running")
	}
	if _, _, err := pool.Acquire(context.Background(), 3); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Acquire after Close = %v", err)
	}
}

// TestPoolLeavesNoProcessWhenIdle runs real processes: once every Bot is idle,
// no Pi process remains.
func TestPoolLeavesNoProcessWhenIdle(t *testing.T) {
	var mu sync.Mutex
	var started []*Process
	pool := NewPool(50*time.Millisecond, func(_ context.Context, _ int64) (*Process, error) {
		p, err := Start(fakePiCommand(t, "rpc"))
		if err == nil {
			mu.Lock()
			started = append(started, p)
			mu.Unlock()
		}
		return p, err
	})
	defer pool.Close()
	for botID := int64(1); botID <= 3; botID++ {
		p, release, err := pool.Acquire(context.Background(), botID)
		if err != nil {
			t.Fatal(err)
		}
		roundTrip(t, p)
		release()
	}
	waitFor(t, "idle processes to exit", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range started {
			select {
			case <-p.Done():
			default:
				return false
			}
		}
		return pool.Live() == 0
	})
}

func TestPoolNeverRunsTwoInstancesPerBot(t *testing.T) {
	o := &opener{}
	pool := NewPool(time.Hour, o.open)
	defer pool.Close()
	a, release, _ := pool.Acquire(context.Background(), 1)
	release()
	a.slow = make(chan struct{})
	stopped := make(chan struct{})
	go func() { pool.Stop(1); close(stopped) }()
	waitFor(t, "retiring", func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.slots[1] != nil && pool.slots[1].retiring != nil
	})
	acquired := make(chan *fakeInstance, 1)
	go func() {
		b, release, err := pool.Acquire(context.Background(), 1)
		if err == nil {
			release()
		}
		acquired <- b
	}()
	time.Sleep(30 * time.Millisecond)
	if o.count() != 1 {
		t.Fatal("started a second instance while the first was closing")
	}
	close(a.slow)
	<-stopped
	if b := <-acquired; b == a || o.count() != 2 {
		t.Fatal("Acquire after Stop did not start a new instance")
	}
}

func TestPoolStaleStartFailureKeepsReplacement(t *testing.T) {
	gate := make(chan struct{})
	var calls atomic.Int32
	pool := NewPool(time.Hour, func(_ context.Context, _ int64) (*fakeInstance, error) {
		if calls.Add(1) == 1 {
			<-gate
			return nil, errors.New("first start failed")
		}
		return newFakeInstance(), nil
	})
	defer pool.Close()
	failed := make(chan error, 1)
	go func() { _, _, err := pool.Acquire(context.Background(), 1); failed <- err }()
	waitFor(t, "first start", func() bool { return calls.Load() == 1 })
	stopped := make(chan struct{})
	go func() { pool.Stop(1); close(stopped) }()
	waitFor(t, "Stop to retire the starting slot", func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.slots[1] != nil && pool.slots[1].retiring != nil
	})
	close(gate)
	if err := <-failed; err == nil {
		t.Fatal("first start succeeded")
	}
	<-stopped
	b, release, err := pool.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	release()
	pool.mu.Lock()
	current := pool.slots[1]
	pool.mu.Unlock()
	if current == nil || current.value != b {
		t.Fatal("replacement left the pool")
	}
}

func TestPoolClosesExitedInstance(t *testing.T) {
	o := &opener{}
	pool := NewPool(time.Hour, o.open)
	defer pool.Close()
	a, release, _ := pool.Acquire(context.Background(), 1)
	defer release()
	a.exit()
	waitFor(t, "exited instance to be closed", func() bool { return a.closes.Load() == 1 && pool.Live() == 0 })
}
