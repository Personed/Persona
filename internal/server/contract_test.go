package server

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Personed/Persona/internal/apitest"
)

// TestContractFixtures replays contracts/api against the Go service. Until the
// domain pieces register handlers, validation exchanges must match and every
// other exchange must answer NOT_IMPLEMENTED.
func TestContractFixtures(t *testing.T) {
	results, err := apitest.Replay(filepath.Join("..", "..", "contracts", "api"), func(string) http.Handler {
		h, err := New(Config{RPC: newRPC(t)})
		if err != nil {
			t.Fatal(err)
		}
		return h
	})
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, r := range results {
		switch {
		case r.Match():
			matched++
		case strings.Contains(r.WantBody, `"Input validation failed"`):
			t.Errorf("validation: %s", r)
		case r.Status != http.StatusNotImplemented || !strings.Contains(r.Body, `"NOT_IMPLEMENTED"`):
			t.Errorf("unregistered procedure: %s", r)
		}
	}
	t.Logf("%d of %d fixture exchanges match; the rest wait for their domain handlers", matched, len(results))
	if matched == 0 {
		t.Error("no fixture exchange matched")
	}
}
