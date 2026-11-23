// Package skills reads managed Agent Skills and serves the skill_read product tool.
//
// Skill metadata lives in the session-scoped "persona.skills" document and packages
// are immutable ZIP files named by content hash, both matching the TypeScript backend
// (packages/agent/src/work/skills.ts and skill-package.ts), so imported installations
// keep their skills.
package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/Personed/Persona/internal/computer"
	"github.com/Personed/Persona/internal/store"
)

// Limits shared with the TypeScript backend (packages/agent/src/work/limits.ts).
const (
	// PackageBytes bounds the unpacked files of one skill package.
	PackageBytes = 8 * 1024 * 1024
	// PackageFiles bounds the files of one skill package.
	PackageFiles = 256
	// archiveBytes bounds the stored ZIP, as the TypeScript bound on its base64 form.
	archiveBytes = PackageBytes / 2 * 3
	// archiveEntries bounds ZIP entries, directories included.
	archiveEntries = 512
)

// DocKind matches the pi-durable defineDoc kind.
const DocKind = "persona.skills"

// Skill is one managed skill. Content is its SKILL.md; Hash names its package.
type Skill struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Content     string    `json:"content"`
	Revision    int       `json:"revision"`
	EnabledBots []int64   `json:"enabledBots"`
	Source      string    `json:"source"`
	RequestID   string    `json:"requestId"`
	Hash        string    `json:"hash,omitempty"`
	Versions    []Version `json:"versions,omitempty"`
}

// Version is one saved package of a skill.
type Version struct {
	Hash    string `json:"hash"`
	Version int    `json:"version"`
}

// Loaded is an enabled skill as skill_read returns it. Path is its SKILL.md on the
// Bot's work computer when the package was materialized there.
type Loaded struct {
	Skill
	Path string `json:"path,omitempty"`
}

// ErrorCode classifies an Error like the TypeScript WorkError.
type ErrorCode string

// Error codes.
const (
	NotFound   ErrorCode = "NOT_FOUND"
	Conflict   ErrorCode = "CONFLICT"
	BadRequest ErrorCode = "BAD_REQUEST"
)

// Error is a failure the caller can correct.
type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string { return e.Message }

// Workspace resolves a Bot's bound work computer and the work directory on it.
// The Target's WriteFile must create missing parent directories.
type Workspace func(ctx context.Context, botID int64) (target computer.Target, workDir string, err error)

// Service reads skills through a store.Store.
type Service struct {
	store     store.Store
	packages  fs.FS
	workspace Workspace
	// packageDir, when set, lets Save store new package zips there.
	packageDir string
	// mu serializes read-modify-write cycles; store.Store has no conditional writes.
	mu sync.Mutex
}

// NewService returns a Service. packages holds "<hash>.zip" files (the "skills"
// directory of the Files root). With nil packages or workspace, Read returns skill
// bodies without materializing supporting files.
func NewService(records store.Store, packages fs.FS, workspace Workspace) *Service {
	return &Service{store: records, packages: packages, workspace: workspace}
}

// UsePackageDir lets the service read and store package zips under dir, as
// SkillPackages over the Files "skills" directory.
func (s *Service) UsePackageDir(dir string) { s.packageDir = dir }

// readPackage returns a stored "<hash>.zip", preferring the package directory.
func (s *Service) readPackage(hash string) ([]byte, error) {
	if s.packageDir != "" {
		return os.ReadFile(filepath.Join(s.packageDir, hash+".zip"))
	}
	if s.packages == nil {
		return nil, fmt.Errorf("skills: package store unavailable")
	}
	file, err := s.packages.Open(hash + ".zip")
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return data, err
}

// storePackage writes a bundle's zip under "<hash>.zip", as SkillPackages.store:
// the write fails silently when the name already exists (packages are immutable).
func (s *Service) storePackage(bundle Bundle) error {
	if s.packageDir == "" {
		return nil
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range bundle.Paths {
		file, err := writer.Create(name)
		if err != nil {
			return err
		}
		if _, err := file.Write(bundle.Files[name]); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.packageDir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(s.packageDir, bundle.Hash+".zip"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := file.Write(archive.Bytes()); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (s *Service) list(ctx context.Context) ([]Skill, error) {
	meta, err := s.store.FindDocument(ctx, DocKind, store.Scope{Kind: "session"}, "", store.Current)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value, err := s.store.Document(ctx, meta.ID, store.Current)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Items []Skill `json:"items"`
	}
	if err := json.Unmarshal(value.Value, &doc); err != nil {
		return nil, fmt.Errorf("skills: decode %s: %w", DocKind, err)
	}
	return doc.Items, nil
}

// Read returns a skill enabled for the Bot and writes its package files under
// .persona/skills/<hash>/<name>/ in the Bot's work directory.
func (s *Service) Read(ctx context.Context, botID int64, id string) (Loaded, error) {
	items, err := s.list(ctx)
	if err != nil {
		return Loaded{}, err
	}
	index := slices.IndexFunc(items, func(skill Skill) bool {
		return skill.ID == id && slices.Contains(skill.EnabledBots, botID)
	})
	if index < 0 {
		return Loaded{}, &Error{NotFound, "技能已删除或未为此Bot启用。"}
	}
	loaded := Loaded{Skill: items[index]}
	if loaded.Hash == "" || s.packages == nil || s.workspace == nil {
		return loaded, nil
	}
	loaded.Path, err = s.materialize(ctx, botID, loaded.Skill)
	return loaded, err
}

var (
	hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

func (s *Service) materialize(ctx context.Context, botID int64, skill Skill) (string, error) {
	if !hashPattern.MatchString(skill.Hash) {
		return "", &Error{BadRequest, "版本哈希无效。"}
	}
	// The name is a directory on the work computer. The document holds the name
	// parsed from this package's SKILL.md when it was saved.
	if len(skill.Name) > 64 || !namePattern.MatchString(skill.Name) {
		return "", &Error{BadRequest, "技能名须为1至64位小写字母、数字或单连字符。"}
	}
	files, err := s.unpack(skill.Hash)
	if err != nil {
		return "", err
	}
	target, workDir, err := s.workspace(ctx, botID)
	if err != nil {
		return "", err
	}
	info, err := target.Info(ctx)
	if err != nil {
		return "", err
	}
	root := path.Join(".persona/skills", skill.Hash, skill.Name)
	for _, file := range files {
		if err := target.WriteFile(ctx, workPath(info.OS, workDir, path.Join(root, file.path)), bytes.NewReader(file.data)); err != nil {
			return "", fmt.Errorf("skills: materialize %s: %w", file.path, err)
		}
	}
	return workPath(info.OS, workDir, path.Join(root, "SKILL.md")), nil
}

// workPath joins a slash-separated relative path to a directory on the work computer.
func workPath(goos, dir, relative string) string {
	separator := "/"
	if goos == "windows" {
		separator = `\`
	}
	return strings.TrimRight(dir, `/\`) + separator + strings.ReplaceAll(relative, "/", separator)
}

type packageFile struct {
	path string
	data []byte
}

func (s *Service) unpack(hash string) ([]packageFile, error) {
	archive, err := s.readPackage(hash)
	if err != nil {
		return nil, fmt.Errorf("skills: package %s: %w", hash, err)
	}
	if len(archive) > archiveBytes {
		return nil, &Error{BadRequest, "技能包超过 8 MiB。"}
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("skills: package %s: %w", hash, err)
	}
	if len(reader.File) > archiveEntries {
		return nil, &Error{BadRequest, "技能文件过大。"}
	}
	var files []packageFile
	remaining := int64(PackageBytes)
	hasSkill := false
	for _, entry := range reader.File {
		if strings.HasSuffix(entry.Name, "/") {
			continue
		}
		if !validPath(entry.Name) {
			return nil, &Error{BadRequest, "技能路径无效。"}
		}
		data, err := readEntry(entry, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			return nil, &Error{BadRequest, "技能包超过 8 MiB。"}
		}
		hasSkill = hasSkill || entry.Name == "SKILL.md"
		files = append(files, packageFile{entry.Name, data})
	}
	if !hasSkill {
		return nil, &Error{BadRequest, "缺少 SKILL.md。"}
	}
	if len(files) > PackageFiles {
		return nil, &Error{BadRequest, "技能包最多包含 256 个文件。"}
	}
	return files, nil
}

// readEntry reads at most limit+1 bytes, so a header that understates the size cannot exceed the package bound.
func readEntry(entry *zip.File, limit int64) ([]byte, error) {
	content, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("skills: package entry %s: %w", entry.Name, err)
	}
	data, err := io.ReadAll(io.LimitReader(content, limit+1))
	if closeErr := content.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("skills: package entry %s: %w", entry.Name, err)
	}
	return data, nil
}

// validPath accepts a normalized relative slash path that is also safe on Windows.
func validPath(name string) bool {
	if strings.ContainsAny(name, `\:`) || strings.ContainsFunc(name, func(char rune) bool { return char < 32 }) || path.Clean(name) != name {
		return false
	}
	return !slices.ContainsFunc(strings.Split(name, "/"), func(part string) bool {
		return part == "" || part == "." || part == ".."
	})
}
