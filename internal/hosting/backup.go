package hosting

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"filippo.io/age"
)

const (
	configName     = "persona.config.json"
	manifestName   = "manifest.json"
	backupVersion  = 1
	backupWork     = 18 // age scrypt uses 256 MiB at this work factor.
	passwordLength = 12
)

type backupManifest struct {
	Version int `json:"version"`
}

// BackupInstallation captures the effective config, local persistent data,
// and local credential files explicitly referenced by the config. It refuses
// to run concurrently with a Persona data writer or overwrite an archive.
func BackupInstallation(ctx context.Context, cfg Config, archive, passphrase string) (err error) {
	target, err := backupDestination(cfg, archive, passphrase)
	if err != nil {
		return err
	}
	release, err := AcquireDataLease(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return writeBackup(ctx, cfg, target, passphrase)
}

func backupDestination(cfg Config, archive, passphrase string) (string, error) {
	if utf8.RuneCountInString(passphrase) < passwordLength {
		return "", errors.New("hosting: backup passphrase must be at least 12 characters")
	}
	if !filepath.IsAbs(cfg.DataDir) || !filepath.IsAbs(cfg.ConfigFile) {
		return "", errors.New("hosting: backup requires a resolved config and data directory")
	}
	data, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(data)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("hosting: backup data path must be a directory")
	}
	target, err := newDestination(archive)
	if err != nil {
		return "", err
	}
	if _, ok := within(data, target); ok {
		return "", errors.New("hosting: backup archive must be outside the data directory")
	}
	return target, nil
}

func newDestination(name string) (string, error) {
	if name == "" {
		return "", errors.New("hosting: destination path is required")
	}
	name, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(name))
	if err != nil {
		return "", err
	}
	name = filepath.Join(parent, filepath.Base(name))
	return name, requireAbsent(name)
}

func requireAbsent(name string) error {
	if _, err := os.Lstat(name); err == nil {
		return fmt.Errorf("hosting: destination %q already exists: %w", name, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func within(directory, file string) (string, bool) {
	rel, err := filepath.Rel(directory, file)
	return rel, err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeBackup(ctx context.Context, cfg Config, target, passphrase string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyCredentialDatabase(ctx, cfg.DataDir); err != nil {
		return err
	}
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return err
	}
	recipient.SetWorkFactor(backupWork)
	temp, err := os.CreateTemp(filepath.Dir(target), ".persona-backup-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	encrypted, err := age.Encrypt(temp, recipient)
	if err != nil {
		return errors.Join(err, temp.Close())
	}
	zipped := gzip.NewWriter(encrypted)
	tarred := tar.NewWriter(zipped)
	if err := backupContents(ctx, tarred, cfg); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := errors.Join(tarred.Close(), zipped.Close(), encrypted.Close(), temp.Sync(), temp.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The temporary file is complete and encrypted before it becomes visible.
	if err := os.Link(temp.Name(), target); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil // Windows does not support flushing directory handles.
	}
	parent, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func backupContents(ctx context.Context, archive *tar.Writer, cfg Config) error {
	if cfg.Computer.SSH != nil {
		ssh := *cfg.Computer.SSH
		cfg.Computer.SSH = &ssh
	}
	if cfg.WorkComputer != nil {
		work := *cfg.WorkComputer
		if work.SSH != nil {
			ssh := *work.SSH
			work.SSH = &ssh
		}
		cfg.WorkComputer = &work
	}
	originalData := cfg.DataDir
	cfg.DataDir = "data"
	if rel, ok := within(originalData, cfg.StaticDir); ok {
		cfg.StaticDir = path.Join("data", filepath.ToSlash(rel))
	}

	// Copy only fields owned by the installation, not arbitrary host or Docker
	// directories. Repoint them at their private copies in the restored profile.
	refs := configCredentialPaths(&cfg)
	for _, ref := range refs {
		if *ref.file == "" {
			continue
		}
		full := *ref.file
		*ref.file = path.Join("secrets", ref.name)
		if rel, ok := within(originalData, full); ok {
			info, err := os.Lstat(full)
			if err != nil || !info.Mode().IsRegular() {
				return errors.Join(errors.New("hosting: config references a missing or invalid credential file"), err)
			}
			*ref.file = path.Join("data", filepath.ToSlash(rel))
			continue
		}
		file, err := os.OpenRoot(filepath.Dir(full))
		if err != nil {
			return err
		}
		if err := errors.Join(tarFile(ctx, archive, file, filepath.Base(full), *ref.file), file.Close()); err != nil {
			return err
		}
	}
	if err := tarJSON(archive, configName, cfg); err != nil {
		return err
	}
	if err := tarJSON(archive, manifestName, backupManifest{Version: backupVersion}); err != nil {
		return err
	}

	directory, err := filepath.EvalSymlinks(originalData)
	if err != nil {
		return err
	}
	data, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() { _ = data.Close() }()
	return fs.WalkDir(data.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if skipBackupFile(name) {
			return nil
		}
		destination := path.Join("data", name)
		if !validArchivePath(destination) {
			return errors.New("hosting: data contains a non-portable path")
		}
		if entry.IsDir() {
			return archive.WriteHeader(&tar.Header{Name: destination, Mode: 0700, Typeflag: tar.TypeDir})
		}
		local, err := filepath.Localize(name)
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return tarDataLink(archive, data, directory, local, destination)
		}
		return tarFile(ctx, archive, data, local, destination)
	})
}

func skipBackupFile(name string) bool {
	switch name {
	case "runtime.sqlite", "runtime.sqlite-shm", "runtime.sqlite-wal", "runtime.sqlite-journal",
		"hosting.sqlite-shm", "agent.sqlite-shm":
		return true
	}
	return false
}

func tarDataLink(archive *tar.Writer, root *os.Root, directory, file, name string) error {
	target, err := root.Readlink(file)
	if err != nil || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return errors.Join(errors.New("hosting: backup requires relative data symlinks"), err)
	}
	// Resolve the actual target before calculating a portable relative link.
	// Do not clean the input: "link/../file" can traverse a symlink first.
	full, err := filepath.EvalSymlinks(filepath.Dir(filepath.Join(directory, file)) + string(filepath.Separator) + target)
	if err != nil {
		return fmt.Errorf("hosting: invalid data symlink %q: %w", name, err)
	}
	rel, ok := within(directory, full)
	if !ok || skipBackupFile(filepath.ToSlash(rel)) {
		return fmt.Errorf("hosting: data symlink %q points outside the snapshot", name)
	}
	link, err := filepath.Rel(filepath.Dir(file), rel)
	if err != nil {
		return err
	}
	link = filepath.ToSlash(link)
	if _, err := dataLinkTarget(name, link); err != nil {
		return err
	}
	info, err := root.Stat(rel)
	if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		return errors.Join(errors.New("hosting: data symlink must target a file or directory"), err)
	}
	return archive.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: link})
}

func dataLinkTarget(name, link string) (string, error) {
	target := path.Join(path.Dir(name), link)
	if !strings.HasPrefix(name, "data/") || !strings.HasPrefix(target, "data/") ||
		!validArchivePath(target) || path.IsAbs(link) || path.Clean(link) != link ||
		target == name || strings.HasPrefix(name, target+"/") {
		return "", errors.New("hosting: unsafe data symlink in backup")
	}
	return target, nil
}

type configCredentialPath struct {
	name string
	file *string
}

func configCredentialPaths(cfg *Config) []configCredentialPath {
	refs := []configCredentialPath{{"desktop-token", &cfg.DesktopCredentialFile}}
	for _, item := range []struct {
		prefix   string
		computer *ComputerConfig
	}{{"computer", &cfg.Computer}, {"work-computer", cfg.WorkComputer}} {
		if item.computer == nil {
			continue
		}
		if computer := item.computer; computer.Kind == "controller" {
			refs = append(refs, configCredentialPath{item.prefix + "-controller-token", &computer.CredentialFile})
		} else if computer.SSH != nil {
			refs = append(refs,
				configCredentialPath{item.prefix + "-known-hosts", &computer.SSH.KnownHostsFile},
				configCredentialPath{item.prefix + "-identity", &computer.SSH.IdentityFile},
				configCredentialPath{item.prefix + "-ssh-config", &computer.SSH.ConfigFile})
		}
	}
	return refs
}

func tarJSON(archive *tar.Writer, name string, value any) error {
	content, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(content))}); err != nil {
		return err
	}
	_, err = archive.Write(content)
	return err
}

func tarFile(ctx context.Context, archive *tar.Writer, root *os.Root, file, name string) error {
	if !validArchivePath(name) {
		return errors.New("hosting: configuration or data contains a non-portable path")
	}
	info, err := root.Lstat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("hosting: cannot archive non-regular file %q", name)
	}
	input, err := root.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	current, err := input.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("hosting: backup file %q changed identity", name)
	}
	mode := int64(0600)
	if strings.HasPrefix(name, "data/") {
		mode |= int64(info.Mode().Perm() & 0100)
	}
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(archive, contextReader{ctx, input})
	return err
}

func validArchivePath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsAny(name, "\\:\x00")
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(b)
}

// RestoreInstallation authenticates and verifies a backup in private staging
// before publishing it to a new directory. Runtime environment overrides are
// deliberately not applied to the staged configuration.
func RestoreInstallation(ctx context.Context, archive, destination, passphrase string) error {
	target, err := newDestination(destination)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".persona-restore-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := extractBackup(ctx, archive, stage, passphrase); err != nil {
		return err
	}
	if err := verifyBackup(ctx, stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := requireAbsent(target); err != nil {
		return err
	}
	return os.Rename(stage, target)
}

func extractBackup(ctx context.Context, name, directory, passphrase string) error {
	input, err := os.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return err
	}
	identity.SetMaxWorkFactor(backupWork)
	decrypted, err := age.Decrypt(contextReader{ctx, input}, identity)
	if err != nil {
		return fmt.Errorf("hosting: cannot unlock backup: %w", err)
	}
	buffer := bufio.NewReader(decrypted)
	zipped, err := gzip.NewReader(buffer)
	if err != nil {
		return err
	}
	defer func() { _ = zipped.Close() }()
	zipped.Multistream(false)
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := extractTar(ctx, root, tar.NewReader(zipped)); err != nil {
		return err
	}
	// A tar end marker is not an authentication boundary. Read through the
	// gzip checksum and encrypted final chunk before publishing anything.
	if n, err := io.Copy(io.Discard, zipped); err != nil || n != 0 {
		return errors.Join(errors.New("hosting: incomplete backup or trailing tar data"), err)
	}
	if _, err := buffer.ReadByte(); err != io.EOF {
		return errors.Join(errors.New("hosting: incomplete backup or trailing encrypted data"), err)
	}
	return nil
}

func extractTar(ctx context.Context, root *os.Root, archive *tar.Reader) error {
	seen := make(map[string]bool)
	type link struct{ name, target string }
	var links []link
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := header.Name
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		top, _, child := strings.Cut(name, "/")
		if !validArchivePath(name) || seen[name] {
			return errors.New("hosting: duplicate or invalid backup entry")
		}
		seen[name] = true
		local, err := filepath.Localize(name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if top != "data" && top != "secrets" {
				return errors.New("hosting: unexpected directory in backup")
			}
			if err := root.MkdirAll(local, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			if (!child || (top != "data" && top != "secrets")) && name != configName && name != manifestName {
				return errors.New("hosting: unexpected file in backup")
			}
			if err := root.MkdirAll(filepath.Dir(local), 0700); err != nil {
				return err
			}
			mode := fs.FileMode(0600)
			if top == "data" {
				mode |= fs.FileMode(header.Mode) & 0100
			}
			output, err := root.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, contextReader{ctx, archive})
			if err := errors.Join(copyErr, output.Close()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target, err := dataLinkTarget(name, header.Linkname)
			if err != nil {
				return err
			}
			links = append(links, link{local, target})
		default:
			return errors.New("hosting: backups may not contain hard links or special files")
		}
	}
	// Check all targets before creating any links, so no target or parent can
	// rely on another archive link to resolve. Rebuild the relative paths.
	targets := make([]string, len(links))
	for i, link := range links {
		target, err := filepath.Localize(link.target)
		if err != nil {
			return err
		}
		if _, err := root.Stat(target); err != nil {
			return fmt.Errorf("hosting: missing data symlink target: %w", err)
		}
		parent := filepath.Dir(link.name)
		if info, err := root.Stat(parent); err != nil || !info.IsDir() {
			return errors.Join(errors.New("hosting: invalid data symlink parent"), err)
		}
		targets[i], err = filepath.Rel(parent, target)
		if err != nil {
			return err
		}
	}
	for i, link := range links {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Symlink(targets[i], link.name); err != nil {
			return err
		}
	}
	return nil
}

func verifyBackup(ctx context.Context, directory string) error {
	content, err := os.ReadFile(filepath.Join(directory, manifestName))
	if err != nil {
		return err
	}
	var manifest backupManifest
	if json.Unmarshal(content, &manifest) != nil || manifest.Version != backupVersion {
		return errors.New("hosting: unsupported backup version")
	}
	cfg, err := loadConfigFile(filepath.Join(directory, configName), func(string) (string, bool) { return "", false })
	if err != nil {
		return err
	}
	data := filepath.Join(directory, "data")
	if cfg.DataDir != data {
		return errors.New("hosting: backup config must refer to the bundled data directory")
	}
	if info, err := os.Stat(data); err != nil || !info.IsDir() {
		return errors.Join(errors.New("hosting: backup is missing its data directory"), err)
	}
	for _, ref := range configCredentialPaths(&cfg) {
		if *ref.file == "" {
			continue
		}
		_, inData := within(data, *ref.file)
		_, inSecrets := within(filepath.Join(directory, "secrets"), *ref.file)
		if !inData && !inSecrets {
			return errors.New("hosting: backup config references an unbundled credential file")
		}
		if info, err := os.Stat(*ref.file); err != nil || !info.Mode().IsRegular() {
			return errors.New("hosting: backup config references a missing or invalid credential file")
		}
	}
	return verifyCredentialDatabase(ctx, data)
}
