// Package migrate imports a pi-durable data directory into the Go store.
package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Personed/Persona/internal/store"
)

const (
	// SourceName is pi-durable's database in the data directory.
	SourceName = "agent.sqlite"
	// TargetName is the Go store's database in the data directory.
	TargetName = "persona-go.sqlite"
	// BackupSuffix names the copy of the source taken before the first import.
	BackupSuffix = ".v2-backup"
)

// Report describes one run. A run after a completed import imports nothing.
type Report struct {
	Target          string
	Backup          string
	BackupCreated   bool
	AlreadyImported bool
	Store           store.ImportReport
	Files           FilesReport
	// Engines lists Bot conversations whose engine is not Pi yet. The legacy
	// runtime migration switches them; until then each needs the user's action.
	Engines []string
}

// FilesReport describes the Files root, which stays in place beside the source.
type FilesReport struct {
	Root           string
	Bots           int
	WorkspaceFiles int
	Artifacts      int
	Problems       []string
}

// Run leases dataDir, backs up its pi-durable database once, and imports it into
// the Go store. The source and its Files root are only read.
func Run(ctx context.Context, dataDir string) (Report, error) {
	// SQLite file URIs need an absolute path.
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return Report{}, err
	}
	source := filepath.Join(dataDir, SourceName)
	if _, err := os.Stat(source); err != nil {
		return Report{}, fmt.Errorf("no pi-durable database: %w", err)
	}
	report := Report{Target: filepath.Join(dataDir, TargetName), Backup: source + BackupSuffix}
	s, err := store.Open(ctx, report.Target)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = s.Close() }()
	src, err := openReadOnly(source)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = src.Close() }()
	if report.BackupCreated, err = backup(ctx, src, report.Backup); err != nil {
		return Report{}, err
	}
	report.Store, err = s.ImportPiDurable(ctx, src)
	if errors.Is(err, store.ErrNotEmpty) {
		report.AlreadyImported, err = true, nil
	}
	if err != nil {
		return Report{}, err
	}
	if report.Engines, err = legacyEngines(ctx, src, s); err != nil {
		return Report{}, err
	}
	report.Files, err = checkFiles(source + ".files")
	return report, err
}

// legacyEngines finds live persona.engine documents through pi-durable's index and
// reads their imported value from the Go store.
func legacyEngines(ctx context.Context, src *sql.DB, s *store.SQLite) ([]string, error) {
	rows, err := src.QueryContext(ctx, `SELECT id, owner_id FROM documents
		WHERE kind = '"persona.engine"' AND scope_kind = 'conversation' AND retired_at IS NULL ORDER BY owner_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var engines []string
	for rows.Next() {
		var id, conversation int64
		if err := rows.Scan(&id, &conversation); err != nil {
			return nil, err
		}
		doc, err := s.Document(ctx, store.DocumentID(id), store.Current)
		if errors.Is(err, store.ErrNotFound) {
			// Written to the source after an earlier import.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read engine document %d: %w", id, err)
		}
		var state struct {
			Config struct {
				Provider string  `json:"provider"`
				Model    *string `json:"model"`
			} `json:"config"`
		}
		if err := json.Unmarshal(doc.Value, &state); err != nil {
			return nil, fmt.Errorf("read engine document %d: %w", id, err)
		}
		if state.Config.Provider == "pi" {
			continue
		}
		model := "no model"
		if state.Config.Model != nil {
			model = "model " + *state.Config.Model
		}
		engines = append(engines, fmt.Sprintf("conversation %d uses engine %q (%s)", conversation, state.Config.Provider, model))
	}
	return engines, rows.Err()
}

func openReadOnly(path string) (*sql.DB, error) {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: url.Values{
		"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"},
	}.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// backup keeps the first copy: a later run must not replace the pre-import state.
func backup(ctx context.Context, src *sql.DB, path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	partial := path + ".partial"
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := src.ExecContext(ctx, "VACUUM INTO ?", partial); err != nil {
		return false, fmt.Errorf("back up %s: %w", path, err)
	}
	if err := os.Rename(partial, path); err != nil {
		return false, err
	}
	return true, nil
}

// checkFiles reads <botId>/workspace and <botId>/artifacts/<id>/{content,metadata.json}.
func checkFiles(root string) (FilesReport, error) {
	report := FilesReport{Root: root}
	bots, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	problem := func(path, format string, args ...any) {
		rel, _ := filepath.Rel(root, path)
		report.Problems = append(report.Problems, filepath.ToSlash(rel)+": "+fmt.Sprintf(format, args...))
	}
	for _, bot := range bots {
		dir := filepath.Join(root, bot.Name())
		if id, err := strconv.ParseInt(bot.Name(), 10, 64); err != nil || id <= 0 || !bot.IsDir() {
			problem(dir, "not a Bot directory")
			continue
		}
		report.Bots++
		children, err := os.ReadDir(dir)
		if err != nil {
			return report, err
		}
		for _, child := range children {
			path := filepath.Join(dir, child.Name())
			switch {
			case child.Name() == "workspace" && child.IsDir():
				err = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
					if err == nil && !entry.IsDir() {
						report.WorkspaceFiles++
					}
					return err
				})
			case child.Name() == "artifacts" && child.IsDir():
				err = checkArtifacts(path, &report, problem)
			default:
				problem(path, "unexpected entry")
			}
			if err != nil {
				return report, err
			}
		}
	}
	return report, nil
}

func checkArtifacts(dir string, report *FilesReport, problem func(string, string, ...any)) error {
	artifacts, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		path := filepath.Join(dir, artifact.Name())
		if !artifact.IsDir() {
			problem(path, "unexpected entry")
			continue
		}
		if info, err := os.Stat(filepath.Join(path, "content")); err != nil || !info.Mode().IsRegular() {
			problem(path, "missing content")
			continue
		}
		metadata, err := os.ReadFile(filepath.Join(path, "metadata.json"))
		if err != nil || !json.Valid(metadata) {
			problem(path, "missing or invalid metadata.json")
			continue
		}
		report.Artifacts++
	}
	return nil
}
