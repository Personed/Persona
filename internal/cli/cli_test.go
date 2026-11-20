package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		{args: []string{"migrate"}, code: 1, stderr: "persona migrate: not implemented"},
		{args: []string{"serve", "--contract", "missing"}, code: 1, stderr: "persona serve: open missing"},
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

func TestServe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, []string{"--port", "0", "--contract", filepath.Join("..", "..", "contracts", "api"), "--static", t.TempDir()}, writer)
	}()
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(reader).ReadString('\n')
		lines <- line
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	url := strings.TrimSpace(strings.TrimPrefix(line, "persona: listening on "))
	resp, err := http.Get(url + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Errorf("health: %d %s", resp.StatusCode, body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}
