package controller

import (
	"context"
	"time"
)

// How often and how long the proxy health is read while it starts. Tests lower them.
var (
	pollInterval      = 250 * time.Millisecond
	proxyStartTimeout = 20 * time.Second
)

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
