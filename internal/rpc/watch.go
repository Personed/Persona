package rpc

import (
	"bytes"
	"context"
)

// WatchValues streams read's value now and again after commits, like
// watchValues in apps/api/src/subscribe.ts: commits that arrive while a value
// is being read or sent coalesce into one read, and a value equal to the last
// one sent is skipped. When commits closes before ctx ends, the stream fails
// with SERVICE_UNAVAILABLE.
func WatchValues[S any](ctx context.Context, commits <-chan S, read func(context.Context) (any, error), send func(any) error) (any, error) {
	var last []byte
	for {
		value, err := read(ctx)
		if err != nil {
			return nil, err
		}
		data, err := marshal(value)
		if err != nil {
			return nil, err
		}
		if last == nil || !bytes.Equal(data, last) {
			if err := send(value); err != nil {
				return nil, err
			}
			last = data
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case _, open := <-commits:
			if !open {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, NewError("SERVICE_UNAVAILABLE", "")
			}
		}
		for drained := false; !drained; {
			select {
			case _, open := <-commits:
				if !open {
					drained = true
				}
			default:
				drained = true
			}
		}
	}
}
