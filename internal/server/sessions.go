package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // sqlite driver for database/sql

	"github.com/Personed/Persona/internal/hosting"
)

// Session policy mirrors packages/hosting/src/sessions.ts.
const (
	pairingDuration   = 5 * time.Minute
	sessionDuration   = 30 * 24 * time.Hour
	exchangeAttempts  = 10
	exchangeAttemptMs = int64(60_000)
)

// SessionStore persists device sessions in the shared hosting.sqlite database,
// in the same devices and pairings tables the TypeScript service writes, so
// paired devices carry over between the two services. A token's digest is the
// same sha256-hex both sides compute; a revoked device is simply a deleted
// row, and expired rows stay harmless on disk while never authenticating.
type SessionStore struct {
	dir string
	db  *sql.DB

	mu       sync.Mutex
	attempts int
	window   int64
	tracked  map[string]*sessionTracker
	revoked  map[string]bool
	now      func() time.Time
}

type sessionTracker struct {
	ctx    context.Context
	cancel func()
	count  int
	done   chan struct{}
}

// NewSessionStore opens (or initializes) the session tables under dir. A
// service using it must hold the data directory's runtime lease for its
// lifetime, as with the credential store. Devices and pairings a previous
// sessions.json build wrote move into the shared tables once.
func NewSessionStore(dir string) (*SessionStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The credential key must exist before hosting.sqlite does: creating the
	// database without it would strand the profile for OpenCredentials and
	// for the TypeScript backup's credential-key check.
	if err := hosting.EnsureCredentialKey(dir); err != nil {
		return nil, err
	}
	db, err := openSessionDB(filepath.Join(dir, "hosting.sqlite"))
	if err != nil {
		return nil, err
	}
	// Keep the TypeScript table layout: devices and pairings use the same
	// columns and sha256-hex digests as packages/hosting/src/store.ts.
	if _, err := db.Exec(`
		PRAGMA journal_mode=WAL;
		CREATE TABLE IF NOT EXISTS devices (
			id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, label TEXT NOT NULL,
			scope TEXT NOT NULL, created INTEGER NOT NULL, expires INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS pairings (
			hash TEXT PRIMARY KEY, scope TEXT NOT NULL, expires INTEGER NOT NULL
		);
	`); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err := importLegacySessions(dir, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &SessionStore{
		dir: dir, db: db,
		tracked: map[string]*sessionTracker{}, revoked: map[string]bool{},
		now: time.Now,
	}, nil
}

func openSessionDB(filename string) (*sql.DB, error) {
	filename, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	path := filepath.ToSlash(filename)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = url.Values{"mode": {"rwc"}, "_pragma": {"busy_timeout(5000)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Close releases the database handle.
func (s *SessionStore) Close() error { return s.db.Close() }

type sessionFile struct {
	Devices []struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Scope   string `json:"scope"`
		Created int64  `json:"created"`
		Expires int64  `json:"expires"`
		Hash    string `json:"hash"`
	} `json:"devices"`
	Pairings []struct {
		Hash    string `json:"hash"`
		Scope   string `json:"scope"`
		Expires int64  `json:"expires"`
	} `json:"pairings"`
}

// importLegacySessions moves rows the earlier sessions.json store wrote into
// the shared SQLite tables. The file is retired before the rows are copied:
// an interruption then loses legacy devices rather than re-importing — and
// possibly resurrecting — sessions the TypeScript service revoked meanwhile.
func importLegacySessions(dir string, db *sql.DB) error {
	filename := filepath.Join(dir, "sessions.json")
	migrated := filename + ".migrated"
	if err := os.Rename(filename, migrated); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := os.ReadFile(migrated)
	if err != nil {
		return err
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("server: invalid sessions file: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, d := range file.Devices {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO devices VALUES (?,?,?,?,?,?)",
			d.ID, d.Hash, d.Label, d.Scope, d.Created, d.Expires,
		); err != nil {
			return err
		}
	}
	for _, p := range file.Pairings {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO pairings VALUES (?,?,?)",
			p.Hash, p.Scope, p.Expires,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SessionStore) ownerCode() string { return filepath.Join(s.dir, "owner-code") }

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func millis(t time.Time) int64 { return t.UnixMilli() }

// List returns unexpired devices in creation order.
func (s *SessionStore) List() ([]Device, error) {
	rows, err := s.db.Query(
		"SELECT id,label,scope,created,expires FROM devices WHERE expires>? ORDER BY created",
		millis(s.now()),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Label, &d.Scope, &d.Created, &d.Expires); err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

// Pairing issues a one-time pairing code for a scope.
func (s *SessionStore) Pairing(scope string) (Pairing, error) {
	code, err := token(15)
	if err != nil {
		return Pairing{}, err
	}
	now := millis(s.now())
	expires := now + pairingDuration.Milliseconds()
	tx, err := s.db.Begin()
	if err != nil {
		return Pairing{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("DELETE FROM pairings WHERE expires<=?", now); err != nil {
		return Pairing{}, err
	}
	if _, err := tx.Exec(
		"INSERT INTO pairings VALUES (?,?,?)", digest(code), scope, expires,
	); err != nil {
		return Pairing{}, err
	}
	if err := tx.Commit(); err != nil {
		return Pairing{}, err
	}
	return Pairing{Code: code, Expires: expires}, nil
}

// Exchange trades a pairing code for a session token.
func (s *SessionStore) Exchange(code, label string) (string, Device, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := millis(s.now())
	if now-s.window >= exchangeAttemptMs {
		s.attempts = 0
		s.window = now
	}
	s.attempts++
	if s.attempts > exchangeAttempts {
		return "", Device{}, false, errors.New("server: too many pairing attempts")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", Device{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var scope string
	var pExpires int64
	err = tx.QueryRow(
		"SELECT scope,expires FROM pairings WHERE hash=?", digest(code),
	).Scan(&scope, &pExpires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && pExpires <= now {
		return "", Device{}, false, nil
	}
	if err != nil {
		return "", Device{}, false, err
	}
	token, err := token(32)
	if err != nil {
		return "", Device{}, false, err
	}
	device := Device{
		ID:      uuid(),
		Label:   label,
		Scope:   scope,
		Created: now,
		Expires: now + sessionDuration.Milliseconds(),
	}
	if _, err := tx.Exec(
		"INSERT INTO devices VALUES (?,?,?,?,?,?)",
		device.ID, digest(token), label, scope, device.Created, device.Expires,
	); err != nil {
		return "", Device{}, false, err
	}
	if _, err := tx.Exec("DELETE FROM pairings WHERE hash=?", digest(code)); err != nil {
		return "", Device{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return "", Device{}, false, err
	}
	return token, device, true, nil
}

// Authenticate resolves a session token to its device.
func (s *SessionStore) Authenticate(token string) (Device, bool) {
	var d Device
	err := s.db.QueryRow(
		"SELECT id,label,scope,created,expires FROM devices WHERE hash=? AND expires>?",
		digest(token), millis(s.now()),
	).Scan(&d.ID, &d.Label, &d.Scope, &d.Created, &d.Expires)
	if err != nil {
		return Device{}, false
	}
	s.mu.Lock()
	revoked := s.revoked[d.ID]
	s.mu.Unlock()
	if revoked {
		return Device{}, false
	}
	return d, true
}

// Revoke removes a device, refusing the last admin device. The count and the
// delete stay under one mutex so two concurrent revokes cannot both pass the
// check before either removes an admin.
func (s *SessionStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var scope string
	err := s.db.QueryRow("SELECT scope FROM devices WHERE id=?", id).Scan(&scope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && scope == "admin" {
		var admins int
		if err := s.db.QueryRow(
			"SELECT COUNT(*) FROM devices WHERE scope='admin' AND expires>?",
			millis(s.now()),
		).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return errors.New("server: keep at least one admin device")
		}
	}
	return s.logoutLocked(id)
}

// Logout removes a device and ends its tracked contexts.
func (s *SessionStore) Logout(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.logoutLocked(id)
}

// logoutLocked deletes the device row. When the delete fails but reads still
// work, the device is remembered as revoked for this process's lifetime: a
// logout reported to the client must not leave the token authenticating.
func (s *SessionStore) logoutLocked(id string) error {
	if _, err := s.db.Exec("DELETE FROM devices WHERE id=?", id); err != nil {
		s.revoked[id] = true
		return err
	}
	if t := s.tracked[id]; t != nil {
		t.cancel()
		delete(s.tracked, id)
	}
	return nil
}

// Track returns the device's context: it ends when the device logs out or
// expires. release ends this caller's tracking; the last release ends the
// device context early, as the AbortController teardown in sessions.ts does.
func (s *SessionStore) Track(device Device) (context.Context, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tracked[device.ID]
	if t == nil {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var once sync.Once
		t = &sessionTracker{
			ctx:    ctx,
			done:   done,
			cancel: func() { once.Do(func() { cancel(); close(done) }) },
		}
		s.tracked[device.ID] = t
		go func() {
			timer := time.NewTimer(time.Until(time.UnixMilli(device.Expires)))
			defer timer.Stop()
			select {
			case <-timer.C:
				t.cancel()
			case <-done:
			}
		}()
	}
	t.count++
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if current := s.tracked[device.ID]; current == t {
				t.count--
				if t.count == 0 {
					t.cancel()
					delete(s.tracked, device.ID)
				}
			}
		})
	}
	return t.ctx, release
}

// Bootstrap writes a new owner pairing code when no admin device is left.
func (s *SessionStore) Bootstrap() error {
	now := millis(s.now())
	var admins int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM devices WHERE scope='admin' AND expires>?", now,
	).Scan(&admins); err != nil {
		return err
	}
	if admins > 0 {
		return nil
	}
	if data, err := os.ReadFile(s.ownerCode()); err == nil {
		var pending int
		if err := s.db.QueryRow(
			"SELECT COUNT(*) FROM pairings WHERE hash=? AND expires>?",
			digest(string(data)), now,
		).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	code, err := token(15)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("DELETE FROM pairings WHERE expires<=?", now); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"INSERT INTO pairings VALUES (?,'admin',?)",
		digest(code), now+pairingDuration.Milliseconds(),
	); err != nil {
		return err
	}
	if err := os.WriteFile(s.ownerCode(), []byte(code), 0o600); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveBootstrap deletes the owner pairing code file.
func (s *SessionStore) RemoveBootstrap() error {
	err := os.Remove(s.ownerCode())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func token(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
