package migrate

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Personed/Persona/internal/store"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// legacyDataDir builds a data directory from the store's pi-durable fixture.
func legacyDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script, err := os.ReadFile(filepath.Join("..", "store", "testdata", "pidurable", "fixture.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, SourceName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), string(script)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	files := filepath.Join(dir, SourceName+".files")
	writeFile(t, filepath.Join(files, "2", "workspace", "notes", "a.txt"), "a")
	writeFile(t, filepath.Join(files, "2", "workspace", "b.txt"), "b")
	writeFile(t, filepath.Join(files, "2", "artifacts", "ok", "content"), "c")
	writeFile(t, filepath.Join(files, "2", "artifacts", "ok", "metadata.json"), `{"name":"c.txt"}`)
	writeFile(t, filepath.Join(files, "2", "artifacts", "broken", "content"), "d")
	writeFile(t, filepath.Join(files, "stray.txt"), "e")
	return dir
}

func TestRunImportsOnceAndKeepsTheBackup(t *testing.T) {
	ctx := t.Context()
	dir := legacyDataDir(t)
	source := filepath.Join(dir, SourceName)
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(filepath.Dir(dir))
	report, err := Run(ctx, filepath.Base(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !report.BackupCreated || report.AlreadyImported || report.Store.Entries != 2 || report.Store.Documents != 6 {
		t.Fatalf("first run = %+v", report)
	}
	files := report.Files
	wantProblems := []string{"2/artifacts/broken: missing or invalid metadata.json", "stray.txt: not a Bot directory"}
	if files.Bots != 1 || files.WorkspaceFiles != 2 || files.Artifacts != 1 || !slices.Equal(files.Problems, wantProblems) {
		t.Fatalf("files = %+v", files)
	}
	if after, err := os.ReadFile(source); err != nil || !bytes.Equal(after, original) {
		t.Fatalf("source changed: %v", err)
	}
	backup, err := sql.Open("sqlite", report.Backup)
	if err != nil {
		t.Fatal(err)
	}
	var entries int
	if err := backup.QueryRowContext(ctx, "SELECT count(*) FROM entries").Scan(&entries); err != nil || entries != 2 {
		t.Fatalf("backup entries = %d, %v", entries, err)
	}
	if err := backup.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(report.Backup)
	if err != nil {
		t.Fatal(err)
	}

	again, err := Run(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.BackupCreated || !again.AlreadyImported || again.Store.Entries != 0 {
		t.Fatalf("second run = %+v", again)
	}
	if later, err := os.Stat(report.Backup); err != nil || !later.ModTime().Equal(info.ModTime()) {
		t.Fatalf("backup replaced: %v", err)
	}

	s, err := store.Open(ctx, report.Target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, seq, err := s.Entry(ctx, 8); err != nil || seq != 3 {
		t.Fatalf("imported entry 8 = seq %d, %v", seq, err)
	}
}

func TestRunRefusesALeasedDataDirectory(t *testing.T) {
	dir := legacyDataDir(t)
	s, err := store.Open(t.Context(), filepath.Join(dir, "other.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := Run(t.Context(), dir); err == nil || !errors.Is(err, store.ErrInUse) {
		t.Fatalf("Run while leased = %v, want ErrInUse", err)
	}
	if _, err := os.Stat(filepath.Join(dir, SourceName+BackupSuffix)); !os.IsNotExist(err) {
		t.Fatalf("backup written while leased: %v", err)
	}
}
