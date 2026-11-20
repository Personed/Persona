package delivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Personed/Persona/internal/computer"
)

// MaxFileBytes bounds one delivered file.
const MaxFileBytes = 10 * 1024 * 1024

// Attachment describes a durable file snapshot. It matches the metadata the
// TypeScript service stores, so existing artifacts stay readable.
type Attachment struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Size       int64           `json:"size"`
	Path       string          `json:"path"`
	MimeType   string          `json:"mimeType,omitempty"`
	ComputerID string          `json:"computerId,omitempty"`
	Source     json.RawMessage `json:"source,omitempty"`
	UploadID   string          `json:"uploadId,omitempty"`
}

var fileID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s *Service) directory(botID int64, id string) (string, error) {
	if !fileID.MatchString(id) {
		return "", &Error{"Invalid file ID"}
	}
	return filepath.Join(s.options.Root, strconv.FormatInt(botID, 10), "artifacts", id), nil
}

func mimeType(content []byte) string {
	detected, _, err := mime.ParseMediaType(http.DetectContentType(content))
	// Sniffed text stays opaque so a delivered file is never served as a page.
	if err != nil || strings.HasPrefix(detected, "text/") {
		return "application/octet-stream"
	}
	return detected
}

// Metadata reads a saved file's description.
func (s *Service) Metadata(botID int64, id string) (Attachment, error) {
	directory, err := s.directory(botID, id)
	if err != nil {
		return Attachment{}, err
	}
	raw, err := os.ReadFile(filepath.Join(directory, "metadata.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Attachment{}, &Error{"No saved file has this id."}
	}
	if err != nil {
		return Attachment{}, err
	}
	var file Attachment
	if err := json.Unmarshal(raw, &file); err != nil {
		return Attachment{}, err
	}
	if file.MimeType == "" {
		content, err := os.ReadFile(filepath.Join(directory, "content"))
		if err != nil {
			return Attachment{}, err
		}
		file.MimeType = mimeType(content)
	}
	return file, nil
}

// save stores a content-addressed snapshot. Content lands before metadata, so a
// readable description always has its bytes.
func (s *Service) save(botID int64, name string, content []byte, computerID string) (Attachment, error) {
	if len(content) > MaxFileBytes {
		return Attachment{}, &Error{"File exceeds 10 MiB"}
	}
	hash := sha256.New()
	hash.Write([]byte(name))
	hash.Write([]byte{0})
	hash.Write(content)
	hash.Write([]byte(computerID))
	file := Attachment{
		ID:         hex.EncodeToString(hash.Sum(nil)),
		Name:       path.Base(name),
		Size:       int64(len(content)),
		Path:       name,
		MimeType:   mimeType(content),
		ComputerID: computerID,
	}
	directory, err := s.directory(botID, file.ID)
	if err != nil {
		return Attachment{}, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Attachment{}, err
	}
	metadata, err := json.Marshal(file)
	if err != nil {
		return Attachment{}, err
	}
	for _, part := range []struct {
		name string
		data []byte
	}{{"content", content}, {"metadata.json", metadata}} {
		if err := replace(directory, part.name, part.data); err != nil {
			return Attachment{}, err
		}
	}
	return file, nil
}

func replace(directory, name string, data []byte) error {
	temporary, err := os.CreateTemp(directory, "write-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), filepath.Join(directory, name))
}

var windowsDrive = regexp.MustCompile(`^[A-Za-z]:/`)

// resolve maps a tool path to an absolute path on the work computer and to its
// slash-separated path inside the workspace. It is lexical: Publish checks links
// and the Target owns permission checks.
func resolve(goos, dir, input string) (absolute, relative string, err error) {
	equal := func(a, b string) bool { return a == b }
	if goos == "windows" {
		dir, input = strings.ReplaceAll(dir, `\`, "/"), strings.ReplaceAll(input, `\`, "/")
		equal = strings.EqualFold
	}
	dir = path.Clean(dir)
	absolute = path.Clean(input)
	if !path.IsAbs(input) && (goos != "windows" || !windowsDrive.MatchString(input)) {
		absolute = path.Join(dir, input)
	}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	if len(absolute) <= len(prefix) || !equal(absolute[:len(prefix)], prefix) {
		return "", "", &Error{"Path is outside this Bot's workspace"}
	}
	relative = absolute[len(prefix):]
	// A colon or a trailing dot or space names another file or stream on Windows.
	for segment := range strings.SplitSeq(relative, "/") {
		if strings.Contains(segment, ":") || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
			return "", "", &Error{"Path is outside this Bot's workspace"}
		}
	}
	return absolute, relative, nil
}

// stat describes a workspace file. It refuses a path that passes through a symbolic
// link and a file with more than one hard link, because both can name content
// outside the workspace. The TypeScript workspace applies the same rule.
func stat(ctx context.Context, target computer.Target, absolute, relative string) (computer.FileInfo, error) {
	root := absolute[:len(absolute)-len(relative)]
	paths := []string{root}
	// A filesystem or drive root keeps its slash.
	if directory := strings.TrimSuffix(root, "/"); directory != "" && !strings.HasSuffix(directory, ":") {
		paths[0] = directory
	}
	for i, character := range relative {
		if character == '/' {
			paths = append(paths, root+relative[:i])
		}
	}
	var info computer.FileInfo
	for _, current := range append(paths, absolute) {
		var err error
		info, err = target.Stat(ctx, current)
		if errors.Is(err, fs.ErrNotExist) {
			return info, &Error{"File not found: " + relative}
		}
		if err != nil {
			return info, err
		}
		// Without a link count the Target has not said whether the path is linked.
		if info.Links == 0 {
			return info, &Error{"The work computer did not report link information for this path"}
		}
		if info.Symlink || (!info.Dir && info.Links > 1) {
			return info, &Error{"Linked paths are outside the workspace capability"}
		}
	}
	return info, nil
}

// Publish snapshots a workspace file from the Bot's work computer.
func (s *Service) Publish(ctx context.Context, botID int64, name string) (Attachment, error) {
	workspace, err := s.options.Workspace(ctx, botID)
	if err != nil {
		return Attachment{}, err
	}
	computer, err := workspace.Target.Info(ctx)
	if err != nil {
		return Attachment{}, err
	}
	absolute, relative, err := resolve(computer.OS, workspace.Dir, name)
	if err != nil {
		return Attachment{}, err
	}
	info, err := stat(ctx, workspace.Target, absolute, relative)
	if err != nil {
		return Attachment{}, err
	}
	if info.Dir {
		return Attachment{}, &Error{"Only files can be attached"}
	}
	if info.Size > MaxFileBytes {
		return Attachment{}, &Error{"File exceeds 10 MiB"}
	}
	reader, err := workspace.Target.ReadFile(ctx, absolute)
	if err != nil {
		return Attachment{}, err
	}
	content, err := io.ReadAll(io.LimitReader(reader, MaxFileBytes+1))
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Attachment{}, err
	}
	return s.save(botID, relative, content, computer.ID)
}

// verify refuses a saved file recorded on another work computer. Files saved before
// computers were recorded belong to the legacy attachment computer.
func (s *Service) verify(ctx context.Context, botID int64, file Attachment) error {
	workspace, err := s.options.Workspace(ctx, botID)
	if err != nil {
		return err
	}
	computer, err := workspace.Target.Info(ctx)
	if err != nil {
		return err
	}
	owner := file.ComputerID
	if owner == "" {
		var computers struct {
			AttachmentComputerID string `json:"attachmentComputerId"`
		}
		if _, err := s.document(ctx, computersDocKind, sessionScope, &computers); err != nil {
			return err
		}
		owner = computers.AttachmentComputerID
	}
	if owner != computer.ID {
		return &Error{"附件属于另一台电脑。请在当前电脑上重新上传。"}
	}
	return nil
}
