package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// health probes a service readiness endpoint; containers have no shell or curl,
// so their HEALTHCHECK calls this instead.
func health(ctx context.Context, args []string, _ io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: persona health <url>")
	}
	target, err := url.Parse(args[0])
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return errors.New("persona health: expected an http(s) URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return errors.New("persona health: not reachable")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("persona health: %s returned %d", target, response.StatusCode)
	}
	return nil
}
