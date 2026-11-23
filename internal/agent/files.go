package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// MaxFileBytes is MAX_FILE_BYTES.
const MaxFileBytes = 10 << 20

// Attachment is a stored Bot file, the metadata.json of its artifact.
type Attachment struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Size       int             `json:"size"`
	Path       string          `json:"path"`
	MimeType   string          `json:"mimeType,omitempty"`
	ComputerID string          `json:"computerId,omitempty"`
	Source     json.RawMessage `json:"source,omitempty"`
	UploadID   string          `json:"uploadId,omitempty"`
}

var (
	fileID   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	uploadID = regexp.MustCompile(`(?i)^[a-f0-9-]{36}$`)
)

// Files stores Bot files in the layout of packages/agent/src/files.ts:
// <root>/<botId>/workspace holds uploads, and <root>/<botId>/artifacts/<id>
// holds each published snapshot as content and metadata.json. The root is the
// TypeScript database path plus ".files". Without a work computer target the
// workspace is local.
type Files struct {
	root  string
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

// NewFiles returns the file store at root.
func NewFiles(root string) *Files { return &Files{root: root, locks: map[int64]*sync.Mutex{}} }

// Workspace returns the Bot's local working directory, creating it.
func (f *Files) Workspace(botID int64) (string, error) {
	dir := filepath.Join(f.root, strconv.FormatInt(botID, 10), "workspace")
	return dir, os.MkdirAll(dir, 0o700)
}

// Exclusive runs fn after every earlier Exclusive call of the Bot.
func (f *Files) Exclusive(botID int64, fn func() error) error {
	f.mu.Lock()
	lock, ok := f.locks[botID]
	if !ok {
		lock = &sync.Mutex{}
		f.locks[botID] = lock
	}
	f.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func (f *Files) directory(botID int64, id string) (string, error) {
	if !fileID.MatchString(id) {
		return "", errors.New("invalid file ID")
	}
	return filepath.Join(f.root, strconv.FormatInt(botID, 10), "artifacts", id), nil
}

func (f *Files) store(botID int64, path string, data []byte, computerID, upload string) (Attachment, error) {
	if len(data) > MaxFileBytes {
		return Attachment{}, errors.New("file exceeds 10 MiB")
	}
	hash := sha256.New()
	hash.Write([]byte(path))
	hash.Write([]byte{0})
	hash.Write(data)
	hash.Write([]byte(computerID))
	file := Attachment{
		ID: hex.EncodeToString(hash.Sum(nil)), Name: pathBase(path), Size: len(data), Path: path,
		MimeType: sniff(data), ComputerID: computerID, UploadID: upload,
	}
	dir, err := f.directory(botID, file.ID)
	if err != nil {
		return Attachment{}, err
	}
	metadata, err := json.Marshal(file)
	if err != nil {
		return Attachment{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Attachment{}, err
	}
	if err := writeAtomic(filepath.Join(dir, "content"), data); err != nil {
		return Attachment{}, err
	}
	return file, writeAtomic(filepath.Join(dir, "metadata.json"), metadata)
}

func writeAtomic(path string, data []byte) error {
	temporary := filepath.Join(filepath.Dir(path), newUUID())
	defer func() { _ = os.Remove(temporary) }()
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func pathBase(path string) string {
	path = strings.TrimRight(path, `/\`)
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		path = path[i+1:]
	}
	return path
}

// Upload stores a file the user sent: it is written into the workspace at
// uploads/<uploadId>/<sha256>/<name> and published as an artifact.
func (f *Files) Upload(botID int64, name string, data []byte, upload string) (Attachment, error) {
	filename := trimJS(pathBase(name))
	if filename == "" || filename == "." || filename == ".." || strings.ContainsFunc(filename, func(r rune) bool { return r < 32 || r == ':' }) {
		return Attachment{}, &WorkError{Code: "BAD_REQUEST", Message: name + "：文件名无效。"}
	}
	if len(data) > MaxFileBytes {
		return Attachment{}, &WorkError{Code: "BAD_REQUEST", Message: filename + "：文件大小超过 10 MiB。"}
	}
	if upload == "" {
		upload = newUUID()
	}
	if !uploadID.MatchString(upload) {
		return Attachment{}, &WorkError{Code: "BAD_REQUEST", Message: "上传 ID 无效。"}
	}
	if kind := sniff(data); strings.HasPrefix(kind, "image/") && !slices.Contains(supportedImages, kind) {
		return Attachment{}, &WorkError{Code: "BAD_REQUEST", Message: fmt.Sprintf("%s：不支持 %s。请使用 PNG、JPEG、WebP 或 GIF。", filename, kind)}
	}
	digest := sha256.Sum256(data)
	filePath := "uploads/" + upload + "/" + hex.EncodeToString(digest[:]) + "/" + filename
	workspace, err := f.Workspace(botID)
	if err != nil {
		return Attachment{}, err
	}
	target := filepath.Join(workspace, filepath.FromSlash(filePath))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return Attachment{}, err
	}
	if err := writeAtomic(target, data); err != nil {
		return Attachment{}, err
	}
	file, err := f.store(botID, filePath, data, "", upload)
	if err != nil {
		_ = os.Remove(target)
	}
	return file, err
}

// Metadata reads a stored file's metadata, filling a missing MIME type.
func (f *Files) Metadata(botID int64, id string) (Attachment, error) {
	dir, err := f.directory(botID, id)
	if err != nil {
		return Attachment{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return Attachment{}, err
	}
	var file Attachment
	if err := json.Unmarshal(data, &file); err != nil {
		return Attachment{}, err
	}
	if file.MimeType == "" {
		content, err := os.ReadFile(filepath.Join(dir, "content"))
		if err != nil {
			return Attachment{}, err
		}
		file.MimeType = sniff(content)
		if data, err = json.Marshal(file); err == nil {
			err = os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0o600)
		}
		if err != nil {
			return Attachment{}, err
		}
	}
	return file, nil
}

// Download reads a stored file and its content.
func (f *Files) Download(botID int64, id string) (Attachment, []byte, error) {
	file, err := f.Metadata(botID, id)
	if err != nil {
		return Attachment{}, nil, err
	}
	dir, _ := f.directory(botID, id)
	data, err := os.ReadFile(filepath.Join(dir, "content"))
	return file, data, err
}

// Discard deletes an uploaded file and its workspace copy. Files that were not
// uploaded, and missing files, are left alone.
func (f *Files) Discard(botID int64, id string) error {
	file, err := f.Metadata(botID, id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if file.UploadID == "" {
		return nil
	}
	workspace, err := f.Workspace(botID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(workspace, filepath.FromSlash(file.Path))); err != nil {
		return err
	}
	dir, _ := f.directory(botID, id)
	return os.RemoveAll(dir)
}

// Verify checks that attachments belong to the Bot's work computer. Without a
// work computer target every attachment is local.
func (f *Files) Verify(int64, []Attachment) error { return nil }

var supportedImages = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// sniff detects a MIME type from magic numbers, a subset of the file-type
// package: common images, PDF and archives. Anything else, including text, is
// application/octet-stream, as file-type reports no type for it.
func sniff(data []byte) string {
	has := func(offset int, magic string) bool {
		return len(data) >= offset+len(magic) && string(data[offset:offset+len(magic)]) == magic
	}
	switch {
	case has(0, "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case has(0, "\xff\xd8\xff"):
		return "image/jpeg"
	case has(0, "GIF87a"), has(0, "GIF89a"):
		return "image/gif"
	case has(0, "RIFF") && has(8, "WEBP"):
		return "image/webp"
	case has(0, "BM") && len(data) > 14:
		return "image/bmp"
	case has(0, "II*\x00"), has(0, "MM\x00*"):
		return "image/tiff"
	case has(0, "\x00\x00\x01\x00"):
		return "image/x-icon"
	case has(4, "ftyp"):
		switch {
		case has(8, "avif"), has(8, "avis"):
			return "image/avif"
		case has(8, "heic"), has(8, "heix"), has(8, "mif1"), has(8, "msf1"):
			return "image/heic"
		}
		return "video/mp4"
	case has(0, "%PDF"):
		return "application/pdf"
	case has(0, "PK\x03\x04"):
		return "application/zip"
	case has(0, "\x1f\x8b"):
		return "application/gzip"
	case bytes.HasPrefix(data, []byte("7z\xbc\xaf\x27\x1c")):
		return "application/x-7z-compressed"
	}
	return "application/octet-stream"
}
