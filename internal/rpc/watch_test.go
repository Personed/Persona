package rpc

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Personed/Persona/internal/store"
)

// watchStore is a store mock: only Watch is used.
type watchStore struct {
	store.Store
	commits chan store.Seq
}

func (s *watchStore) Watch(context.Context) (<-chan store.Seq, error) { return s.commits, nil }

func TestWatchValues(t *testing.T) {
	s := &watchStore{commits: make(chan store.Seq, 8)}
	values := []string{"a", "a", "b", "c"}
	reads := 0
	var sent []any
	commits, _ := s.Watch(context.Background())
	_, err := WatchValues(context.Background(), commits, func(context.Context) (any, error) {
		value := values[reads]
		reads++
		switch reads {
		case 1, 2:
			s.commits <- store.Seq(reads)
		case 3:
			s.commits <- 3
			s.commits <- 4 // coalesced with 3
		case 4:
			close(s.commits)
		}
		return value, nil
	}, func(value any) error {
		sent = append(sent, value)
		return nil
	})
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != "SERVICE_UNAVAILABLE" {
		t.Errorf("closed watch: %v", err)
	}
	if want := []any{"a", "b", "c"}; !slices.Equal(sent, want) || reads != 4 {
		t.Errorf("sent %v after %d reads, want %v after 4", sent, reads, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err = WatchValues(ctx, make(chan store.Seq), func(context.Context) (any, error) { return 1, nil }, func(any) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled watch: %v", err)
	}
}
