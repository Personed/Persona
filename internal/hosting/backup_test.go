package hosting

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const testPassphrase = "generated fixture backup passphrase"

func backupFixture(t *testing.T) (Config, string) {
	t.Helper()
	dataDir := t.TempDir()
	filename := writeConfig(t, `{"desktopCredentialFile": "desktop-token"}`)
	dir := filepath.Dir(filename)
	if err := os.WriteFile(filepath.Join(dir, "desktop-token"), []byte("desktop-fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFile(filename, func(name string) (string, bool) {
		return dataDir, name == "PERSONA_DATA_DIR"
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	ctx := t.Context()
	cfg, dir := backupFixture(t)
	seedLegacyCredentials(t, cfg.DataDir)
	store, err := OpenCredentials(ctx, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	service := Credential{Ref: Ref{Scope: "service", Name: "sample"}, Secret: []byte("sample-service-fixture")}
	if err := errors.Join(store.Put(ctx, service), store.Close()); err != nil {
		t.Fatal(err)
	}
	originalKey, err := os.ReadFile(filepath.Join(cfg.DataDir, credentialKey))
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(cfg.DataDir, "work", "test bot")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "report.txt"), []byte("private fixture data"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.DataDir, "work", "current-report")
	linkErr := os.Symlink(filepath.Join("test bot", "report.txt"), link)
	if linkErr != nil && (runtime.GOOS != "windows" || !errors.Is(linkErr, os.ErrPermission) && !strings.Contains(linkErr.Error(), "privilege")) {
		t.Fatal(linkErr)
	}
	archive := filepath.Join(dir, "before.tar.gz.age")
	if err := BackupInstallation(ctx, cfg, archive, testPassphrase); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("age-encryption.org/v1\n")) ||
		bytes.Contains(raw, []byte("private fixture data")) || bytes.Contains(raw, originalKey) {
		t.Fatal("backup should be a standard age archive without plaintext data")
	}

	dest := filepath.Join(dir, "restored")
	t.Setenv("PERSONA_DATA_DIR", cfg.DataDir)
	if err := RestoreInstallation(ctx, archive, dest, testPassphrase); err != nil {
		t.Fatal(err)
	}
	restored, err := loadConfigFile(filepath.Join(dest, configName), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if restored.DataDir != filepath.Join(dest, "data") || restored.DesktopCredentialFile != filepath.Join(dest, "secrets", "desktop-token") {
		t.Fatalf("restored config paths still point outside the profile: %+v", restored)
	}
	for file, expected := range map[string][]byte{
		filepath.Join(restored.DataDir, "work", "test bot", "report.txt"): []byte("private fixture data"),
		filepath.Join(restored.DataDir, credentialKey):                    originalKey,
		restored.DesktopCredentialFile:                                    []byte("desktop-fixture-token"),
	} {
		got, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("wrong restored contents at %s: %v", file, err)
		}
	}
	if _, err := os.Stat(filepath.Join(restored.DataDir, "runtime.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime lease must not be restored: %v", err)
	}
	if linkErr == nil {
		restoredLink := filepath.Join(restored.DataDir, "work", "current-report")
		if got, err := os.Readlink(restoredLink); err != nil || filepath.ToSlash(got) != "test bot/report.txt" {
			t.Fatalf("local workspace link was not restored: %v", err)
		}
	}
	reopened, err := OpenCredentials(ctx, restored.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	for _, fixture := range legacyFixtures {
		recovered, err := reopened.Get(ctx, legacyRef(fixture.id))
		if err != nil || string(recovered.Secret) != fixture.plaintext {
			t.Fatalf("legacy credential not restored: %v", err)
		}
	}
	recovered, err := reopened.Get(ctx, service.Ref)
	if err != nil || !bytes.Equal(recovered.Secret, service.Secret) {
		t.Fatalf("Go credential not restored: %v", err)
	}
	if err := RestoreInstallation(ctx, archive, dest, testPassphrase); !errors.Is(err, os.ErrExist) {
		t.Fatalf("restore should refuse to overwrite an installation: %v", err)
	}
	if err := BackupInstallation(ctx, cfg, archive, testPassphrase); !errors.Is(err, os.ErrExist) {
		t.Fatalf("backup should refuse to overwrite an archive: %v", err)
	}

	t.Run("wrong passphrase", func(t *testing.T) {
		assertRestoreRejected(t, archive, dir, "incorrect fixture passphrase")
	})
	t.Run("tampered final chunk", func(t *testing.T) {
		raw[len(raw)-1] ^= 1
		tampered := filepath.Join(dir, "tampered.tar.gz.age")
		if err := os.WriteFile(tampered, raw, 0600); err != nil {
			t.Fatal(err)
		}
		assertRestoreRejected(t, tampered, dir, testPassphrase)
	})
}

func assertRestoreRejected(t *testing.T, archive, dir, passphrase string) {
	t.Helper()
	dest := filepath.Join(dir, "rejected")
	if err := RestoreInstallation(t.Context(), archive, dest, passphrase); err == nil {
		t.Fatal("invalid backup was restored")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore published an unverified profile: %v", err)
	}
	leftover, err := filepath.Glob(filepath.Join(dir, ".persona-restore-*"))
	if err != nil || len(leftover) != 0 {
		t.Fatalf("restore left plaintext in staging: %v", err)
	}
}

func TestExtractTarRejectsUnsafeEntries(t *testing.T) {
	for name, head := range map[string]*tar.Header{
		"parent":    {Name: "data/../outside", Typeflag: tar.TypeReg, Mode: 0600},
		"absolute":  {Name: "/outside", Typeflag: tar.TypeReg, Mode: 0600},
		"backslash": {Name: `data\outside`, Typeflag: tar.TypeReg, Mode: 0600},
		"link":      {Name: "data/link", Typeflag: tar.TypeSymlink, Linkname: "../outside"},
		"missing":   {Name: "data/link", Typeflag: tar.TypeSymlink, Linkname: "not-present"},
		"hard link": {Name: "data/link", Typeflag: tar.TypeLink, Linkname: "data/file"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			writer := tar.NewWriter(&buf)
			if err := errors.Join(writer.WriteHeader(head), writer.Close()); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if err := extractTar(t.Context(), root, tar.NewReader(&buf)); err == nil {
				t.Fatal("unsafe entry accepted")
			}
		})
	}
}

func TestBackupRefusesMissingOrMismatchedCredentialKey(t *testing.T) {
	cfg, dir := backupFixture(t)
	seedLegacyCredentials(t, cfg.DataDir)
	filename := filepath.Join(cfg.DataDir, credentialKey)
	if err := os.WriteFile(filename, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "backup.age")
	if err := BackupInstallation(t.Context(), cfg, out, testPassphrase); err == nil || !strings.Contains(err.Error(), "does not authenticate") {
		t.Fatalf("wrong key not detected: %v", err)
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if err := BackupInstallation(t.Context(), cfg, out, testPassphrase); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key not detected: %v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid backup was published: %v", err)
	}
}

func TestUpgradeDoesNotInstallWithoutABackup(t *testing.T) {
	cfg, dir := backupFixture(t)
	seedLegacyCredentials(t, cfg.DataDir)
	if err := os.Remove(filepath.Join(cfg.DataDir, credentialKey)); err != nil {
		t.Fatal(err)
	}
	err := UpgradeInstallation(t.Context(), cfg, filepath.Join(dir, "recovery.age"), testPassphrase, UpgradeSteps{
		StopWrites: func(context.Context) error { return nil },
		Install:    func(context.Context) error { t.Fatal("install ran without a recovery backup"); return nil },
		Verify:     func(context.Context) error { t.Fatal("verify ran without a recovery backup"); return nil },
	})
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "recovery backup") {
		t.Fatalf("expected backup error, got %v", err)
	}
}

func TestUpgradeStopsWritesThenBacksUpInstallsAndVerifies(t *testing.T) {
	cfg, dir := backupFixture(t)
	ctx := t.Context()
	lease, err := AcquireDataLease(ctx, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease() }()
	archive := filepath.Join(dir, "recovery.age")
	if err := BackupInstallation(ctx, cfg, archive, testPassphrase); err == nil {
		t.Fatal("backup should require the existing writer to release its lease")
	}
	installFile := filepath.Join(cfg.DataDir, "state")
	verificationFailure := errors.New("fixture verification failure")
	var order []string
	err = UpgradeInstallation(ctx, cfg, archive, testPassphrase, UpgradeSteps{
		StopWrites: func(context.Context) error {
			order = append(order, "stop")
			return errors.Join(os.WriteFile(installFile, []byte("before install"), 0600), lease())
		},
		Install: func(ctx context.Context) error {
			order = append(order, "install")
			if _, err := os.Stat(archive); err != nil {
				return err
			}
			if release, err := AcquireDataLease(ctx, cfg.DataDir); err == nil {
				_ = release()
				return errors.New("fixture: install ran without a maintenance lease")
			}
			return os.WriteFile(installFile, []byte("after install"), 0600)
		},
		Verify: func(context.Context) error {
			order = append(order, "verify")
			raw, err := os.ReadFile(installFile)
			if err != nil || string(raw) != "after install" {
				t.Fatalf("verifier ran before installation: %v", err)
			}
			return verificationFailure
		},
	})
	if !errors.Is(err, verificationFailure) || !reflect.DeepEqual(order, []string{"stop", "install", "verify"}) {
		t.Fatalf("wrong upgrade execution: %v, %v", order, err)
	}
	release, err := AcquireDataLease(ctx, cfg.DataDir)
	if err != nil {
		t.Fatalf("failed upgrade did not release lease: %v", err)
	}
	defer func() { _ = release() }()
	restored := filepath.Join(dir, "recovery")
	if err := RestoreInstallation(ctx, archive, restored, testPassphrase); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(restored, "data", "state")); err != nil || string(raw) != "before install" {
		t.Fatalf("recovery archive is not the stopped preinstall state: %v", err)
	}
}
