package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrInUse means the data directory is already leased by another runtime or by maintenance.
var ErrInUse = errors.New("store: data directory is in use")

// ErrClosed means the store has closed.
var ErrClosed = errors.New("store: closed")

const (
	leaseFilename = "runtime.sqlite"
	applicationID = 0x5052534e // PRSN
	schemaVersion = 1
)

//go:embed schema.sql
var schema string

// SQLite is the disk-backed Store. Construct it with Open.
type SQLite struct {
	db    *sql.DB
	lease *sql.DB

	writer sync.Mutex
	closed bool

	watchMu sync.Mutex
	seq     Seq
	changed chan struct{}
	done    chan struct{}
}

var _ Store = (*SQLite)(nil)

// Open opens the Go store at path, a separate database from pi-durable's. Its parent
// directory is leased until Close, including against the legacy service and backup.
func Open(ctx context.Context, path string) (*SQLite, error) {
	if path == "" {
		return nil, errors.New("store: database path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, name := filepath.Split(path)
	if name == "" {
		return nil, errors.New("store: database file name is required")
	}
	if lower := strings.ToLower(name); lower == leaseFilename || strings.HasPrefix(lower, leaseFilename+"-") {
		return nil, errors.New("store: database path is reserved for the runtime lease")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lease, err := acquireLease(ctx, filepath.Join(dir, leaseFilename))
	if err != nil {
		return nil, err
	}
	s := &SQLite{lease: lease, changed: make(chan struct{}), done: make(chan struct{})}
	if err := s.openDatabase(ctx, path); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if err := s.recoverInterruptedCalls(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("store: recover interrupted calls: %w", err), s.Close())
	}
	return s, nil
}

func acquireLease(ctx context.Context, path string) (*sql.DB, error) {
	// Use the same exclusive rollback-mode transaction as the TypeScript data lease.
	// Keep it on the single connection, not attached to the startup context's lifetime.
	db, err := openSQLite(path, "rwc", "journal_mode(DELETE)")
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		if sqliteCode(err, sqlite3.SQLITE_BUSY) || sqliteCode(err, sqlite3.SQLITE_LOCKED) {
			err = fmt.Errorf("%w: %s", ErrInUse, filepath.Dir(path))
		}
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func sqliteCode(err error, code int) bool {
	var cause *sqlite.Error
	return errors.As(err, &cause) && cause.Code()&0xff == code
}

func openSQLite(path, mode string, pragmas ...string) (*sql.DB, error) {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: url.Values{
		"mode": {mode}, "_pragma": pragmas,
	}.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (s *SQLite) openDatabase(ctx context.Context, path string) error {
	// Create a private file; SQLite gives its WAL and SHM the same permissions.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if err := f.Close(); err != nil {
			return err
		}
	} else if errors.Is(err, os.ErrExist) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("store: database must be a regular file")
		}
	} else {
		return err
	}
	s.db, err = openSQLite(path, "rw", "synchronous(FULL)", "foreign_keys(ON)", "busy_timeout(5000)")
	if err != nil {
		return err
	}
	var id, version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&id); err != nil {
		return err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch {
	case id == 0 && version == 0:
		var objects int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&objects); err != nil {
			return err
		}
		if objects != 0 {
			return errors.New("store: refusing to open an existing non-Persona Go database")
		}
	case id != applicationID || version != schemaVersion:
		return fmt.Errorf("store: unsupported SQLite application ID %d or schema version %d", id, version)
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("store: SQLite did not enable WAL (mode %q)", mode)
	}
	if version == 0 {
		if err := transaction(ctx, s.db, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, schema+fmt.Sprintf(
				"\nPRAGMA application_id=%d; PRAGMA user_version=%d;", applicationID, schemaVersion))
			return err
		}); err != nil {
			return fmt.Errorf("store: initialize schema: %w", err)
		}
	}
	return s.db.QueryRowContext(ctx, "SELECT seq FROM store_state WHERE singleton=1").Scan(&s.seq)
}

func transaction(ctx context.Context, db *sql.DB, run func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Close stops watches, closes the database, and releases the directory lease.
func (s *SQLite) Close() error {
	s.writer.Lock()
	defer s.writer.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.done)
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	return errors.Join(err, s.lease.Close())
}

// MintID reserves an ID durably, even if no record is written with it.
func (s *SQLite) MintID(ctx context.Context) (int64, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	var id int64
	err := transaction(ctx, s.db, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			UPDATE store_state SET last_id=last_id+1 WHERE singleton=1 RETURNING last_id`).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func nextSeq(ctx context.Context, tx *sql.Tx) (Seq, error) {
	var seq Seq
	err := tx.QueryRowContext(ctx, `
		UPDATE store_state SET seq=seq+1 WHERE singleton=1 RETURNING seq`).Scan(&seq)
	return seq, err
}

// publish runs under writer, strictly after the transaction has committed. Because
// sequences are consecutive, a slow watcher only needs its next sequence, not a queue.
func (s *SQLite) publish(seq Seq) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	s.seq = seq
	close(s.changed)
	s.changed = make(chan struct{})
}

// Watch delivers new commit sequences in order until cancellation or Close.
func (s *SQLite) Watch(ctx context.Context) (<-chan Seq, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	select {
	case <-s.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	out := make(chan Seq)
	go s.watch(ctx, out, s.seq)
	return out, nil
}

func (s *SQLite) watch(ctx context.Context, out chan<- Seq, last Seq) {
	defer close(out)
	for {
		if ctx.Err() != nil {
			return
		}
		s.watchMu.Lock()
		latest, changed := s.seq, s.changed
		s.watchMu.Unlock()
		if last == latest {
			select {
			case <-changed:
			case <-ctx.Done():
				return
			case <-s.done:
				return
			}
			continue
		}
		select {
		case out <- last + 1:
			last++
		case <-ctx.Done():
			return
		case <-s.done:
			return
		}
	}
}
