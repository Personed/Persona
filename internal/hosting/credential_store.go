package hosting

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	// Register the CGO-free SQLite driver for database/sql.
	_ "modernc.org/sqlite"
)

const (
	credentialDatabase = "hosting.sqlite"
	credentialKey      = "credentials.key"
)

// CredentialStore reads and writes the hosting SQLite database. A service using
// it must hold the data directory's runtime lease for its lifetime.
type CredentialStore struct {
	db   *sql.DB
	aead cipher.AEAD
}

var _ Credentials = (*CredentialStore)(nil)

// OpenCredentials opens (or initializes) the encrypted store. If an existing
// database lost its key, it fails without generating an unusable replacement.
func OpenCredentials(ctx context.Context, directory string) (*CredentialStore, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	key, err := openCredentialKey(directory)
	if err != nil {
		return nil, err
	}
	aead, err := credentialCipher(key)
	if err != nil {
		return nil, err
	}
	db, err := openSQLite(filepath.Join(directory, credentialDatabase), "rwc")
	if err != nil {
		return nil, err
	}
	// Keep the legacy table column layout: the TypeScript writer inserts positional values.
	if _, err := db.ExecContext(ctx, `
		PRAGMA journal_mode=WAL;
		CREATE TABLE IF NOT EXISTS credentials (
			provider TEXT PRIMARY KEY, type TEXT NOT NULL, value BLOB NOT NULL
		);
		CREATE TABLE IF NOT EXISTS persona_credentials (
			scope TEXT NOT NULL, owner TEXT NOT NULL, name TEXT NOT NULL,
			legacy_digest BLOB, value BLOB NOT NULL, PRIMARY KEY (scope, owner, name)
		);
	`); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &CredentialStore{db: db, aead: aead}, nil
}

func openSQLite(filename, mode string) (*sql.DB, error) {
	filename, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	path := filepath.ToSlash(filename)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)"}}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func openCredentialKey(directory string) ([]byte, error) {
	filename := filepath.Join(directory, credentialKey)
	key, err := readCredentialKey(filename)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(filepath.Join(directory, credentialDatabase+suffix)); err == nil {
			return nil, errors.New("hosting: credential database exists but credentials.key is missing")
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	key = make([]byte, 32)
	rand.Read(key)
	temp, err := os.CreateTemp(directory, ".credentials-key-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	_, writeErr := temp.Write(key)
	if err := errors.Join(writeErr, temp.Sync(), temp.Close()); err != nil {
		return nil, err
	}
	// Publish the fully written key without ever replacing a concurrently created key.
	if err := os.Link(temp.Name(), filename); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readCredentialKey(filename)
		}
		return nil, err
	}
	return key, nil
}

func readCredentialKey(filename string) ([]byte, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 32 {
		return nil, errors.New("hosting: invalid credentials.key")
	}
	return os.ReadFile(filename)
}

func credentialCipher(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Close releases the SQLite connection.
func (s *CredentialStore) Close() error { return s.db.Close() }

// Provider refs without an owner use the exact TypeScript provider ID.
// Managed MCP rows use their original persona-mcp:… ID with an empty owner.
func legacyRef(id string) Ref {
	scope := "provider"
	if strings.HasPrefix(id, "persona-mcp:") {
		scope = "mcp"
	}
	return Ref{Scope: scope, Name: id}
}

func isLegacyRef(ref Ref) bool { return ref == legacyRef(ref.Name) }

func validRef(ref Ref) bool {
	return ref.Name != "" && utf8.ValidString(ref.Name) && utf8.ValidString(ref.Owner) &&
		(ref.Scope == "provider" || ref.Scope == "service" || ref.Scope == "mcp" || ref.Scope == "device")
}

// The TypeScript wire layout is nonce || auth tag || ciphertext (not Go's
// ciphertext || tag). It authenticates the exact SQLite provider ID as AAD.
func sealCredential(aead cipher.AEAD, plain []byte, identity string) []byte {
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	sealed := aead.Seal(nil, nonce, plain, []byte(identity))
	ciphertext, tag := sealed[:len(plain)], sealed[len(plain):]
	return append(append(nonce, tag...), ciphertext...)
}

func openCredential(aead cipher.AEAD, value []byte, identity string) ([]byte, error) {
	n, end := aead.NonceSize(), aead.NonceSize()+aead.Overhead()
	if len(value) < end {
		return nil, errors.New("hosting: invalid encrypted credential")
	}
	sealed := make([]byte, len(value)-n)
	copy(sealed, value[end:])
	copy(sealed[len(value)-end:], value[n:end])
	return aead.Open(nil, value[:n], sealed, []byte(identity))
}

type credentialData struct {
	Secret    []byte            `json:"secret,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	ExpiresAt time.Time         `json:"expiresAt,omitzero"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type legacyData struct {
	Type    string          `json:"type"`
	Expires json.RawMessage `json:"expires"`
}

func parseLegacy(secret []byte) (legacyData, error) {
	var data legacyData
	if json.Unmarshal(secret, &data) != nil || (data.Type != "oauth" && data.Type != "api_key") {
		return legacyData{}, errors.New("hosting: provider and managed MCP secrets must be legacy credential JSON")
	}
	return data, nil
}

func credentialAAD(ref Ref, digest []byte) string {
	id, _ := json.Marshal(ref)
	return "persona:go-credential:" + string(id) + ":" + string(digest)
}

func (s *CredentialStore) decode(ref Ref, digest, value []byte) (credentialData, error) {
	var data credentialData
	plain, err := openCredential(s.aead, value, credentialAAD(ref, digest))
	if err != nil || json.Unmarshal(plain, &data) != nil {
		return credentialData{}, errors.New("hosting: cannot decrypt credential metadata")
	}
	return data, nil
}

// Put stores credentials. The Secret of a legacy provider or managed MCP ref is
// its full JSON value (including its type); it is not translated or stripped.
func (s *CredentialStore) Put(ctx context.Context, c Credential) error {
	if !validRef(c.Ref) || len(c.Secret) == 0 {
		return errors.New("hosting: invalid credential reference or empty secret")
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now().UTC()
	}
	var digest []byte
	data := credentialData{c.Secret, c.Metadata, c.ExpiresAt, c.UpdatedAt}
	if !isLegacyRef(c.Ref) {
		return s.putData(ctx, s.db, c.Ref, digest, data)
	}

	legacy, err := parseLegacy(c.Secret)
	if err != nil {
		return err
	}
	value := sealCredential(s.aead, c.Secret, c.Ref.Name)
	hash := sha256.Sum256(value)
	data.Secret = nil
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO credentials (provider,type,value) VALUES (?,?,?)
		ON CONFLICT(provider) DO UPDATE SET type=excluded.type,value=excluded.value
	`, c.Ref.Name, legacy.Type, value); err != nil {
		return err
	}
	if err := s.putData(ctx, tx, c.Ref, hash[:], data); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CredentialStore) putData(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, ref Ref, digest []byte, data credentialData) error {
	plain, err := json.Marshal(data)
	if err != nil {
		return errors.New("hosting: invalid credential metadata or timestamp")
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO persona_credentials (scope,owner,name,legacy_digest,value) VALUES (?,?,?,?,?)
		ON CONFLICT(scope,owner,name) DO UPDATE SET legacy_digest=excluded.legacy_digest,value=excluded.value
	`, ref.Scope, ref.Owner, ref.Name, digest, sealCredential(s.aead, plain, credentialAAD(ref, digest)))
	return err
}

func (data credentialData) credential(ref Ref) Credential {
	return Credential{Ref: ref, Secret: data.Secret, Metadata: data.Metadata, UpdatedAt: data.UpdatedAt, ExpiresAt: data.ExpiresAt}
}

// Get retrieves the secret and metadata for ref.
func (s *CredentialStore) Get(ctx context.Context, ref Ref) (Credential, error) {
	if !validRef(ref) {
		return Credential{}, errors.New("hosting: invalid credential reference")
	}
	if isLegacyRef(ref) {
		result, err := s.legacyCredentials(ctx, ref.Name, true)
		if err != nil {
			return Credential{}, err
		}
		if len(result) == 0 {
			return Credential{}, ErrNotFound
		}
		return result[0], nil
	}
	var value []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT value FROM persona_credentials WHERE scope=? AND owner=? AND name=? AND legacy_digest IS NULL
	`, ref.Scope, ref.Owner, ref.Name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, err
	}
	data, err := s.decode(ref, nil, value)
	return data.credential(ref), err
}

func (s *CredentialStore) legacyCredentials(ctx context.Context, id string, secrets bool) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.provider,c.type,c.value,p.legacy_digest,p.value
		FROM credentials c LEFT JOIN persona_credentials p ON p.owner='' AND p.name=c.provider
		AND p.scope=CASE WHEN substr(c.provider,1,12)='persona-mcp:' THEN 'mcp' ELSE 'provider' END
		WHERE (?='' OR c.provider=?) ORDER BY c.provider
	`, id, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Credential
	for rows.Next() {
		var name, kind string
		var value, digest, metadata []byte
		if err := rows.Scan(&name, &kind, &value, &digest, &metadata); err != nil {
			return nil, err
		}
		ref := legacyRef(name)
		var data credentialData
		hash := sha256.Sum256(value)
		// Ignore stale Go metadata after a legacy service updates the row.
		if bytes.Equal(hash[:], digest) {
			data, err = s.decode(ref, digest, metadata)
			if err != nil {
				return nil, err
			}
		}
		if data.Metadata == nil {
			data.Metadata = make(map[string]string, 1)
		}
		data.Metadata["type"] = kind
		if secrets {
			data.Secret, err = openCredential(s.aead, value, name)
			if err != nil {
				return nil, errors.New("hosting: cannot decrypt legacy credential")
			}
			legacy, err := parseLegacy(data.Secret)
			if err != nil || legacy.Type != kind {
				return nil, errors.New("hosting: invalid legacy credential")
			}
			if expires, _ := strconv.ParseInt(string(legacy.Expires), 10, 64); data.ExpiresAt.IsZero() && kind == "oauth" && expires != 0 {
				data.ExpiresAt = time.UnixMilli(expires).UTC()
			}
		}
		result = append(result, data.credential(ref))
	}
	return result, rows.Err()
}

// List enumerates metadata; an empty scope or owner is a wildcard.
func (s *CredentialStore) List(ctx context.Context, scope, owner string) ([]Credential, error) {
	var result []Credential
	if owner == "" && (scope == "" || scope == "provider" || scope == "mcp") {
		rows, err := s.legacyCredentials(ctx, "", false)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if scope == "" || row.Ref.Scope == scope {
				result = append(result, row)
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT scope,owner,name,value FROM persona_credentials WHERE legacy_digest IS NULL
		AND (?='' OR scope=?) AND (?='' OR owner=?) ORDER BY scope,owner,name
	`, scope, scope, owner, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref Ref
		var value []byte
		if err := rows.Scan(&ref.Scope, &ref.Owner, &ref.Name, &value); err != nil {
			return nil, err
		}
		data, err := s.decode(ref, nil, value)
		if err != nil {
			return nil, err
		}
		data.Secret = nil
		result = append(result, data.credential(ref))
	}
	return result, rows.Err()
}

// Delete removes a credential if it exists.
func (s *CredentialStore) Delete(ctx context.Context, ref Ref) error {
	if !validRef(ref) {
		return errors.New("hosting: invalid credential reference")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if isLegacyRef(ref) {
		if _, err := tx.ExecContext(ctx, "DELETE FROM credentials WHERE provider=?", ref.Name); err != nil {
			return fmt.Errorf("delete legacy credential: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM persona_credentials WHERE scope=? AND owner=? AND name=?", ref.Scope, ref.Owner, ref.Name); err != nil {
		return err
	}
	return tx.Commit()
}
