package tui

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// servers is the saved list of Persona services and the device token paired with each.
// It lives in the user config directory with owner-only permissions.
type servers struct {
	path    string
	Current string            `json:"current,omitempty"`
	Tokens  map[string]string `json:"tokens,omitempty"`
}

func serversPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "persona", "tui.json"), nil
}

func loadServers(path string) (*servers, error) {
	s := &servers{path: path, Tokens: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	if s.Tokens == nil {
		s.Tokens = map[string]string{}
	}
	return s, nil
}

func (s *servers) save() error {
	if s == nil || s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// remember records token for server and makes server the default for the next start.
func (s *servers) remember(server, token string) error {
	if s == nil {
		return nil
	}
	s.Current = server
	if token == "" {
		delete(s.Tokens, server)
	} else {
		s.Tokens[server] = token
	}
	return s.save()
}
