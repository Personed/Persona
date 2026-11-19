package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{args: nil, code: 2, stdout: "Usage: persona"},
		{args: []string{"help"}, code: 0, stdout: "migrate"},
		{args: []string{"version"}, code: 0, stdout: "dev"},
		{args: []string{"serve"}, code: 1, stderr: "persona serve: not implemented"},
		{args: []string{"migrate", "-data-dir", t.TempDir()}, code: 1, stderr: "persona migrate: no pi-durable database"},
		{args: []string{"migrate", "-h"}, code: 0, stdout: "-data-dir"},
		{args: []string{"unknown"}, code: 2, stderr: `unknown command "unknown"`},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), c.args, &stdout, &stderr); code != c.code {
			t.Errorf("Run(%q) = %d, want %d", c.args, code, c.code)
		}
		if !strings.Contains(stdout.String(), c.stdout) || !strings.Contains(stderr.String(), c.stderr) {
			t.Errorf("Run(%q) stdout %q stderr %q", c.args, stdout.String(), stderr.String())
		}
	}
}
