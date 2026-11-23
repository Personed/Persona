package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
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

// startServe runs serve with args and returns its URL; the server stops when the test ends.
func startServe(t *testing.T, args ...string) string {
	t.Helper()
	data := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer writer.Close()
		done <- serve(ctx, append([]string{"--port", "0", "--data", data, "--contract", filepath.Join("..", "..", "contracts", "api")}, args...), writer)
	}()
	lines := make(chan string, 1)
	go func() {
		r := bufio.NewReader(reader)
		line, _ := r.ReadString('\n')
		lines <- line
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() {
		defer reader.Close()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	})
	const prefix = "persona: listening on "
	var line string
	select {
	case line = <-lines:
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("unexpected serve startup output: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	return strings.TrimSpace(strings.TrimPrefix(line, prefix))
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

func TestServe(t *testing.T) {
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<p>Persona</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	static, err := webFS(web)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fs.ReadFile(static, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	url := startServe(t, "--static", web)
	if code, body := get(t, url+"/health"); code != http.StatusOK || body != `{"ok":true}` {
		t.Errorf("health: %d %s", code, body)
	}
	if code, body := get(t, url+"/"); code != http.StatusOK || body != string(want) {
		t.Errorf("web: status %d; expected the selected index.html", code)
	}
}

func TestServeHeadless(t *testing.T) {
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<p>Persona</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	url := startServe(t, "--headless", "--static", web)
	if code, body := get(t, url+"/health"); code != http.StatusOK || body != `{"ok":true}` {
		t.Errorf("health: %d %s", code, body)
	}
	if code, _ := get(t, url+"/"); code != http.StatusNotFound {
		t.Errorf("headless served the web UI: %d", code)
	}
	if code, _ := get(t, url+"/rpc/bots/list"); code == http.StatusNotFound {
		t.Errorf("headless did not serve the API: %d", code)
	}
}
