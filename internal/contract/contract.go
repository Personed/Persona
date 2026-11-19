// Package contract loads the frozen API contract in contracts/api, which records the
// TypeScript oRPC API that the Go service must keep serving unchanged (D2).
package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Procedure is one oRPC procedure. Kind is "unary" or "stream" (SSE event iterator).
// Input is JSON Schema 2020-12, nil when the procedure takes no input. Errors lists
// every oRPC error code the handler can return.
type Procedure struct {
	Path   string          `json:"path"`
	Kind   string          `json:"kind"`
	Input  json.RawMessage `json:"input"`
	Errors []string        `json:"errors"`
}

// HTTPPath is the request path of the procedure, e.g. /rpc/bots/create.
func (p Procedure) HTTPPath(prefix string) string {
	return prefix + "/" + strings.ReplaceAll(p.Path, ".", "/")
}

// Contract is contracts/api/procedures.json.
type Contract struct {
	Transport  string      `json:"transport"`
	Prefix     string      `json:"prefix"`
	Procedures []Procedure `json:"procedures"`
}

// Procedure returns the procedure at path.
func (c Contract) Procedure(path string) (Procedure, bool) {
	index := sort.Search(len(c.Procedures), func(i int) bool { return c.Procedures[i].Path >= path })
	if index < len(c.Procedures) && c.Procedures[index].Path == path {
		return c.Procedures[index], true
	}
	return Procedure{}, false
}

// Exchange is one recorded request and response on the oRPC wire. Volatile values
// are normalized to "<uuid>", "<time>" and "<path>".
type Exchange struct {
	Procedure string `json:"procedure"`
	Request   struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	} `json:"request"`
	Response struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"response"`
}

// Load reads procedures.json from dir.
func Load(dir string) (Contract, error) {
	var contract Contract
	data, err := os.ReadFile(filepath.Join(dir, "procedures.json"))
	if err != nil {
		return contract, err
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		return contract, fmt.Errorf("procedures.json: %w", err)
	}
	sort.Slice(contract.Procedures, func(i, j int) bool { return contract.Procedures[i].Path < contract.Procedures[j].Path })
	return contract, nil
}

// Fixtures reads every recorded exchange in dir/fixtures, keyed by file name without extension.
func Fixtures(dir string) (map[string][]Exchange, error) {
	files, err := filepath.Glob(filepath.Join(dir, "fixtures", "*.json"))
	if err != nil {
		return nil, err
	}
	fixtures := make(map[string][]Exchange, len(files))
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var exchanges []Exchange
		if err := json.Unmarshal(data, &exchanges); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(file), err)
		}
		fixtures[strings.TrimSuffix(filepath.Base(file), ".json")] = exchanges
	}
	return fixtures, nil
}
