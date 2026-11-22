package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Session policy mirrors packages/hosting/src/sessions.ts.
const (
	pairingDuration   = 5 * time.Minute
	sessionDuration   = 30 * 24 * time.Hour
	exchangeAttempts  = 10
	exchangeAttemptMs = int64(60_000)
)

// SessionStore persists device sessions in a JSON file inside the service data
// directory, as packages/hosting/src/sessions.ts does on its HostingStore.
type SessionStore struct {
	dir string

	mu       sync.Mutex
	devices  []sessionDevice
	pairings []sessionPairing
	attempts int
	window   int64
	tracked  map[string]*sessionTracker
	now      func() time.Time
}

type sessionDevice struct {
	Device
	Hash string `json:"hash"`
}

type sessionPairing struct {
	Hash    string `json:"hash"`
	Scope   string `json:"scope"`
	Expires int64  `json:"expires"`
}

type sessionTracker struct {
	ctx    context.Context
	cancel func()
	count  int
	done   chan struct{}
}

type sessionFile struct {
	Devices  []sessionDevice  `json:"devices"`
	Pairings []sessionPairing `json:"pairings"`
}

// NewSessionStore loads (or creates) the session store under dir.
func NewSessionStore(dir string) (*SessionStore, error) {
	s := &SessionStore{dir: dir, tracked: map[string]*sessionTracker{}, now: time.Now}
	data, err := os.ReadFile(s.filename())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return s, nil
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("server: invalid sessions file: %w", err)
	}
	s.devices = file.Devices
	s.pairings = file.Pairings
	return s, nil
}

func (s *SessionStore) filename() string  { return filepath.Join(s.dir, "sessions.json") }
func (s *SessionStore) ownerCode() string { return filepath.Join(s.dir, "owner-code") }

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func (s *SessionStore) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(sessionFile{Devices: s.devices, Pairings: s.pairings})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".sessions-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.filename())
}

// List returns unexpired devices in creation order.
func (s *SessionStore) List() ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := millis(s.now())
	var list []Device
	for _, d := range s.devices {
		if d.Expires > now {
			list = append(list, d.Device)
		}
	}
	return list, nil
}

// Pairing issues a one-time pairing code for a scope.
func (s *SessionStore) Pairing(scope string) (Pairing, error) {
	code, err := token(15)
	if err != nil {
		return Pairing{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := millis(s.now())
	expires := now + pairingDuration.Milliseconds()
	s.pairings = append(prunePairings(s.pairings, now), sessionPairing{
		Hash: digest(code), Scope: scope, Expires: expires,
	})
	if err := s.saveLocked(); err != nil {
		return Pairing{}, err
	}
	return Pairing{Code: code, Expires: expires}, nil
}

func prunePairings(pairings []sessionPairing, now int64) []sessionPairing {
	kept := pairings[:0]
	for _, p := range pairings {
		if p.Expires > now {
			kept = append(kept, p)
		}
	}
	return kept
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
	hash := digest(code)
	scope := ""
	kept := s.pairings[:0]
	for _, p := range s.pairings {
		if p.Hash == hash && p.Expires > now {
			scope = p.Scope
			continue
		}
		kept = append(kept, p)
	}
	s.pairings = kept
	if scope == "" {
		return "", Device{}, false, nil
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
	s.devices = append(s.devices, sessionDevice{Device: device, Hash: digest(token)})
	if err := s.saveLocked(); err != nil {
		return "", Device{}, false, err
	}
	return token, device, true, nil
}

// Authenticate resolves a session token to its device.
func (s *SessionStore) Authenticate(token string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := millis(s.now())
	hash := digest(token)
	for _, d := range s.devices {
		if d.Hash == hash && d.Expires > now {
			return d.Device, true
		}
	}
	return Device{}, false
}

// Revoke removes a device, refusing the last admin device.
func (s *SessionStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var target *sessionDevice
	admins := 0
	now := millis(s.now())
	for i := range s.devices {
		d := &s.devices[i]
		if d.Expires > now && d.Scope == "admin" {
			admins++
		}
		if d.ID == id {
			target = d
		}
	}
	if target != nil && target.Scope == "admin" && admins <= 1 {
		return errors.New("server: keep at least one admin device")
	}
	return s.logoutLocked(id)
}

// Logout removes a device and ends its tracked contexts.
func (s *SessionStore) Logout(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.logoutLocked(id)
}

func (s *SessionStore) logoutLocked(id string) error {
	kept := s.devices[:0]
	for _, d := range s.devices {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	s.devices = kept
	if t := s.tracked[id]; t != nil {
		t.cancel()
		delete(s.tracked, id)
	}
	return s.saveLocked()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	now := millis(s.now())
	for _, d := range s.devices {
		if d.Scope == "admin" && d.Expires > now {
			return nil
		}
	}
	if data, err := os.ReadFile(s.ownerCode()); err == nil {
		hash := digest(string(data))
		for _, p := range s.pairings {
			if p.Hash == hash && p.Expires > now {
				return nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	code, err := token(15)
	if err != nil {
		return err
	}
	s.pairings = append(prunePairings(s.pairings, now), sessionPairing{
		Hash: digest(code), Scope: "admin", Expires: now + pairingDuration.Milliseconds(),
	})
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.ownerCode(), []byte(code), 0o600); err != nil {
		return err
	}
	return s.saveLocked()
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
