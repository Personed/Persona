package server

import (
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
