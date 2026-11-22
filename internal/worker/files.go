package worker

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// FileInfo is the lstat result. Kind is file, directory, symlink, or other.
type FileInfo struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	MtimeSec  int64  `json:"mtimeSec"`
	MtimeNsec int64  `json:"mtimeNsec"`
	Dev       uint64 `json:"dev,omitempty"`
	Ino       uint64 `json:"ino,omitempty"`
	Links     uint64 `json:"links,omitempty"`
}

func describe(path string, info fs.FileInfo) FileInfo {
	kind := "other"
	switch mode := info.Mode(); {
	case mode.IsRegular():
		kind = "file"
	case mode.IsDir():
		kind = "directory"
	case mode&fs.ModeSymlink != 0:
		kind = "symlink"
	}
	mtime := info.ModTime()
	dev, ino, links := fileIdentity(path, info)
	return FileInfo{
		Name: filepath.Base(path), Kind: kind, Size: info.Size(),
		MtimeSec: mtime.Unix(), MtimeNsec: int64(mtime.Nanosecond()), Dev: dev, Ino: ino, Links: links,
	}
}

type pathRequest struct {
	Path      string `json:"path"`
	To        string `json:"to,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	Force     bool   `json:"force,omitempty"`
	Append    bool   `json:"append,omitempty"`
	Parents   *bool  `json:"parents,omitempty"`
	Keep      bool   `json:"keep,omitempty"`
	NoFollow  bool   `json:"noFollow,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

// handles holds files and directories opened by open and opendir until close.
type handles struct {
	mu   sync.Mutex
	next uint32
	open map[uint32]*os.File
}

const maxHandles = 4096

func (h *handles) add(file *os.File) (uint32, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.open) >= maxHandles {
		_ = file.Close()
		return 0, codeError("EMFILE", "too many open handles", file.Name())
	}
	h.next++
	h.open[h.next] = file
	return h.next, nil
}

func (h *handles) get(id uint32) (*os.File, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if file, ok := h.open[id]; ok {
		return file, nil
	}
	return nil, codeError("EBADF", "unknown handle", "")
}

func (h *handles) close(id uint32) error {
	h.mu.Lock()
	file, ok := h.open[id]
	delete(h.open, id)
	h.mu.Unlock()
	if !ok {
		return codeError("EBADF", "unknown handle", "")
	}
	return file.Close()
}

func (h *handles) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, file := range h.open {
		_ = file.Close()
		delete(h.open, id)
	}
}

func lstat(path string) (FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return FileInfo{}, err
	}
	return describe(path, info), nil
}

func writeFile(h *handles, request pathRequest, data []byte) (any, error) {
	if request.Parents == nil || *request.Parents {
		if err := mkdirAll(filepath.Dir(request.Path)); err != nil {
			return nil, err
		}
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if request.Append {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	file, err := os.OpenFile(request.Path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return nil, err
	}
	if !request.Keep {
		return struct{}{}, file.Close()
	}
	id, err := h.add(file)
	return map[string]uint32{"handle": id}, err
}

// mkdirAll creates missing parents. A file in the way fails with ENOTDIR, an
// existing directory succeeds.
func mkdirAll(path string) error {
	err := os.MkdirAll(path, 0o755)
	if err != nil && errors.Is(err, fs.ErrExist) {
		return codeError("ENOTDIR", "not a directory", path)
	}
	return err
}

func mkdir(request pathRequest) error {
	if !request.Recursive {
		return os.Mkdir(request.Path, 0o755)
	}
	if info, err := os.Lstat(request.Path); err == nil && !info.IsDir() {
		return codeError("EEXIST", "file already exists", request.Path)
	}
	return mkdirAll(request.Path)
}

func remove(request pathRequest) error {
	info, err := os.Lstat(request.Path)
	if err != nil {
		if request.Force && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.IsDir() && !request.Recursive {
		return codeError("ERR_FS_EISDIR", "path is a directory", request.Path)
	}
	if request.Recursive {
		return os.RemoveAll(request.Path)
	}
	return os.Remove(request.Path)
}

type handleRequest struct {
	Handle uint32 `json:"handle"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	Max    int    `json:"max"`
}

func openFile(h *handles, request pathRequest) (any, error) {
	if request.NoFollow {
		info, err := os.Lstat(request.Path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, codeError("EINVAL", "not a regular file", request.Path)
		}
	}
	file, err := os.Open(request.Path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.IsDir() {
		_ = file.Close()
		return nil, codeError("EISDIR", "illegal operation on a directory", request.Path)
	}
	id, err := h.add(file)
	return map[string]any{"handle": id, "info": describe(request.Path, info)}, err
}

const maxRead = 8 << 20

func pread(h *handles, request handleRequest) ([]byte, error) {
	file, err := h.get(request.Handle)
	if err != nil {
		return nil, err
	}
	length := min(max(request.Length, 0), maxRead)
	buffer := make([]byte, length)
	n, err := file.ReadAt(buffer, request.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buffer[:n], nil
}

func writeChunk(h *handles, request handleRequest, data []byte) error {
	file, err := h.get(request.Handle)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	return err
}

type dirEntry struct {
	Name  string    `json:"name"`
	Info  *FileInfo `json:"info,omitempty"`
	Error *Error    `json:"error,omitempty"`
}

func openDir(h *handles, path string) (any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.IsDir() {
		err = codeError("ENOTDIR", "not a directory", path)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	id, err := h.add(file)
	return map[string]uint32{"handle": id}, err
}

// readDir returns up to max entries in directory order. Each entry carries its
// lstat result or the error that prevented it.
func readDir(h *handles, request handleRequest) (any, error) {
	file, err := h.get(request.Handle)
	if err != nil {
		return nil, err
	}
	count := request.Max
	if count <= 0 || count > 1024 {
		count = 1024
	}
	names, err := file.Readdirnames(count)
	done := errors.Is(err, io.EOF)
	if err != nil && !done {
		return nil, err
	}
	entries := make([]dirEntry, 0, len(names))
	for _, name := range names {
		entry := dirEntry{Name: name}
		if info, err := lstat(filepath.Join(file.Name(), name)); err != nil {
			entry.Error = toError(err)
		} else {
			entry.Info = &info
		}
		entries = append(entries, entry)
	}
	return map[string]any{"entries": entries, "done": done || len(names) < count}, nil
}

func decode[T any](raw json.RawMessage) (T, error) {
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, codeError("EINVAL", "invalid request: "+err.Error(), "")
	}
	return value, nil
}
