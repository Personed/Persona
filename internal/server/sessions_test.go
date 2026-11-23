package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionStoreExchange(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := s.Pairing("admin")
	if err != nil {
		t.Fatal(err)
	}
	token, device, ok, err := s.Exchange(pairing.Code, "laptop")
	if err != nil || !ok {
		t.Fatalf("exchange: ok=%v err=%v", ok, err)
	}
	if device.Scope != "admin" || device.Label != "laptop" || device.ID == "" {
		t.Fatalf("device: %+v", device)
	}
	if got, ok := s.Authenticate(token); !ok || got.ID != device.ID {
		t.Fatalf("authenticate: %+v ok=%v", got, ok)
	}
	if _, _, ok, _ := s.Exchange(pairing.Code, "again"); ok {
		t.Fatal("consumed pairing code exchanged twice")
	}
	if _, _, ok, _ := s.Exchange("wrong-code", "bad"); ok {
		t.Fatal("unknown code exchanged")
	}
	devices, err := s.List()
	if err != nil || len(devices) != 1 {
		t.Fatalf("list: %v %v", devices, err)
	}
}

func TestSessionStorePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	pairing, _ := s.Pairing("client")
	token, device, _, err := s.Exchange(pairing.Code, "phone")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.Authenticate(token); !ok || got.ID != device.ID {
		t.Fatalf("reloaded authenticate: %+v ok=%v", got, ok)
	}
}

func TestSessionStoreRevokeKeepsLastAdmin(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewSessionStore(dir)
	p, _ := s.Pairing("admin")
	_, admin, _, _ := s.Exchange(p.Code, "a")
	if err := s.Revoke(admin.ID); err == nil {
		t.Fatal("revoked the last admin device")
	}
	p2, _ := s.Pairing("admin")
	_, admin2, _, _ := s.Exchange(p2.Code, "b")
	if err := s.Revoke(admin.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := s.Authenticate(""); ok {
		t.Fatal("unexpected device")
	}
	if devices, _ := s.List(); len(devices) != 1 || devices[0].ID != admin2.ID {
		t.Fatalf("devices: %+v", devices)
	}
}

func TestSessionStoreLogoutEndsTrack(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewSessionStore(dir)
	p, _ := s.Pairing("client")
	_, device, _, _ := s.Exchange(p.Code, "x")
	ctx, release := s.Track(device)
	select {
	case <-ctx.Done():
		t.Fatal("tracked context ended early")
	default:
	}
	s.Logout(device.ID)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("tracked context did not end on logout")
	}
	release()
}

// A TypeScript profile keeps its paired devices and pairings on the Go
// service: both sides share data/hosting.sqlite, so valid rows carry over and
// expired ones still do not authenticate.
func TestSessionStorePreservesTypescriptDevices(t *testing.T) {
	dir := t.TempDir()
	db, err := openSessionDB(filepath.Join(dir, "hosting.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// A real TypeScript profile carries its credential key beside the database.
	if err := os.WriteFile(filepath.Join(dir, "credentials.key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	oldToken := "typescript-session-token"
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS devices (
			id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, label TEXT NOT NULL,
			scope TEXT NOT NULL, created INTEGER NOT NULL, expires INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS pairings (
			hash TEXT PRIMARY KEY, scope TEXT NOT NULL, expires INTEGER NOT NULL
		);
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO devices VALUES
			('dev-old', ?, 'laptop', 'admin', ?, ?),
			('dev-dead', ?, 'phone', 'admin', ?, ?)`,
		digest(oldToken), now-1000, now+30*24*3600*1000,
		digest("expired-token"), now-60*24*3600*1000, now-1000,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO pairings VALUES (?, 'admin', ?)",
		digest("old-pairing"), now+60_000,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Authenticate(oldToken)
	if !ok || got.ID != "dev-old" || got.Scope != "admin" || got.Label != "laptop" {
		t.Fatalf("typescript device lost: %+v ok=%v", got, ok)
	}
	if _, ok := s.Authenticate("expired-token"); ok {
		t.Fatal("expired device authenticated")
	}
	if err := s.Revoke("dev-old"); err == nil {
		t.Fatal("revoked the last admin device")
	}
	token, device, ok, err := s.Exchange("old-pairing", "new")
	if err != nil || !ok || device.Scope != "admin" {
		t.Fatalf("typescript pairing code rejected: %v %v %+v", ok, err, device)
	}
	if _, ok := s.Authenticate(token); !ok {
		t.Fatal("new device token not stored in sqlite")
	}
	devices, err := s.List()
	if err != nil || len(devices) != 2 || devices[0].ID != "dev-old" {
		t.Fatalf("list: %+v", devices)
	}
}

// Devices a sessions.json build wrote migrate into hosting.sqlite once.
func TestSessionStoreMigratesLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	token := "legacy-json-token"
	now := time.Now().UnixMilli()
	if err := os.WriteFile(
		filepath.Join(dir, "sessions.json"),
		[]byte(`{"devices":[{"id":"dev-json","hash":"`+digest(token)+
			`","label":"laptop","scope":"admin","created":`+
			fmt.Sprint(now)+`,"expires":`+fmt.Sprint(now+3600_000)+`}],"pairings":[]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	s, err := NewSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Authenticate(token); !ok || got.ID != "dev-json" {
		t.Fatalf("legacy device lost: %+v ok=%v", got, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json.migrated")); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreBootstrapOwnerCode(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewSessionStore(dir)
	if err := s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	code, err := os.ReadFile(filepath.Join(dir, "owner-code"))
	if err != nil {
		t.Fatal(err)
	}
	if len(code) == 0 {
		t.Fatal("empty owner code")
	}
	// The owner code pairs an admin device; bootstrap stops writing once paired.
	token, device, ok, err := s.Exchange(string(code), "owner")
	if err != nil || !ok || device.Scope != "admin" {
		t.Fatalf("exchange owner code: %v %v %+v", ok, err, device)
	}
	if err := s.RemoveBootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "owner-code")); !os.IsNotExist(err) {
		t.Fatal("owner-code still present")
	}
	if err := s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "owner-code")); !os.IsNotExist(err) {
		t.Fatal("bootstrap wrote a code with an admin device present")
	}
	_ = token
}
