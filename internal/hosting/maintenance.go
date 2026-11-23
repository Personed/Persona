package hosting

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// AcquireDataLease excludes another Persona runtime or maintenance operation.
// It uses the same SQLite file and lock as the TypeScript runtime. The caller
// must close all data writers before releasing the lease.
func AcquireDataLease(ctx context.Context, dataDir string) (release func() error, err error) {
	if dataDir == "" {
		return nil, errors.New("hosting: data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	db, err := openSQLite(filepath.Join(dataDir, "runtime.sqlite"), "rwc")
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	closeLease := func() error {
		return errors.Join(conn.Close(), db.Close())
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0; PRAGMA journal_mode=DELETE; BEGIN EXCLUSIVE`); err != nil {
		return nil, errors.Join(fmt.Errorf("hosting: acquire runtime lease (stop other Persona writers first): %w", err), closeLease())
	}
	// SQLite releases the exclusive transaction when its connection closes.
	return sync.OnceValue(closeLease), nil
}

// UpgradeSteps are supplied by the installation supervisor. StopWrites must
// stop/drain Persona writers and release their leases. Install and Verify run
// offline and must not acquire another data lease, replace its lock file, or
// start writers.
type UpgradeSteps struct {
	StopWrites func(context.Context) error
	Install    func(context.Context) error
	Verify     func(context.Context) error
}

// UpgradeInstallation stops writes, creates a recovery archive, installs the
// update, and verifies it before releasing the data lease. An install or
// verification failure leaves the archive in place for recovery.
func UpgradeInstallation(ctx context.Context, cfg Config, archive, passphrase string, steps UpgradeSteps) (err error) {
	if steps.StopWrites == nil || steps.Install == nil || steps.Verify == nil {
		return errors.New("hosting: stop-writes, install, and verify steps are required")
	}
	target, err := backupDestination(cfg, archive, passphrase)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := steps.StopWrites(ctx); err != nil {
		return fmt.Errorf("hosting: stop writes: %w", err)
	}
	release, err := AcquireDataLease(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	if err := writeBackup(ctx, cfg, target, passphrase); err != nil {
		return fmt.Errorf("hosting: create recovery backup: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := steps.Install(ctx); err != nil {
		return fmt.Errorf("hosting: install: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := steps.Verify(ctx); err != nil {
		return fmt.Errorf("hosting: verify installation: %w", err)
	}
	return ctx.Err()
}
