package hosting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func verifyCredentialDatabase(ctx context.Context, directory string) (err error) {
	file := filepath.Join(directory, credentialDatabase)
	if _, err := os.Lstat(file); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(file + suffix)
		if err != nil && (suffix == "" || !errors.Is(err, fs.ErrNotExist)) {
			return err
		}
		if info != nil && !info.Mode().IsRegular() {
			return errors.New("hosting: credential database and its sidecars must be regular files")
		}
	}
	key, err := readCredentialKey(filepath.Join(directory, credentialKey))
	if err != nil {
		return fmt.Errorf("hosting: cannot verify the credential database without its key: %w", err)
	}
	aead, err := credentialCipher(key)
	if err != nil {
		return err
	}
	db, err := openSQLite(file, "ro")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return errors.Join(errors.New("hosting: credential database failed its integrity check"), err)
	}
	store := &CredentialStore{db: db, aead: aead}
	if err := verifyLegacyRows(ctx, store); err != nil {
		return err
	}
	var goTable int
	if err := db.QueryRowContext(ctx, "SELECT 1 FROM sqlite_schema WHERE type='table' AND name='persona_credentials'").Scan(&goTable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // A pure TypeScript database does not have this table.
		}
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT scope,owner,name,legacy_digest,value FROM persona_credentials")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref Ref
		var digest, value []byte
		if err := rows.Scan(&ref.Scope, &ref.Owner, &ref.Name, &digest, &value); err != nil {
			return err
		}
		if _, err := store.decode(ref, digest, value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func verifyLegacyRows(ctx context.Context, store *CredentialStore) error {
	rows, err := store.db.QueryContext(ctx, "SELECT provider,type,value FROM credentials")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, kind string
		var value []byte
		if err := rows.Scan(&id, &kind, &value); err != nil {
			return err
		}
		plain, err := openCredential(store.aead, value, id)
		if err != nil {
			return errors.New("hosting: credential key does not authenticate all rows")
		}
		legacy, err := parseLegacy(plain)
		if err != nil || legacy.Type != kind {
			return errors.New("hosting: invalid credential in backup")
		}
	}
	return rows.Err()
}
