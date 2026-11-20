package rpc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Personed/Persona/internal/contract"
)

func loadContract(t *testing.T) contract.Contract {
	t.Helper()
	c, err := contract.Load(filepath.Join("..", "..", "contracts", "api"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestZodParity replays inputs recorded against the router's Zod schemas by
// apps/api/contract/zod-issues.ts.
func TestZodParity(t *testing.T) {
	c := loadContract(t)
	data, err := os.ReadFile(filepath.Join("testdata", "zod-issues.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Cases []struct {
			Path   string          `json:"path"`
			Input  json.RawMessage `json:"input"`
			Output json.RawMessage `json:"output"`
			Issues json.RawMessage `json:"issues"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	schemas := map[string]*Schema{}
	failures := 0
	for i, tc := range recorded.Cases {
		schema, ok := schemas[tc.Path]
		if !ok {
			p, _ := c.Procedure(tc.Path)
			if schema, err = CompileSchema(p.Input); err != nil {
				t.Fatalf("%s: %v", tc.Path, err)
			}
			schemas[tc.Path] = schema
		}
		input := undefined
		if tc.Input != nil {
			if input, err = decode(tc.Input); err != nil {
				t.Fatal(err)
			}
		}
		output, issues := schema.Validate(input)
		var got, want []byte
		if tc.Issues != nil {
			got, _ = json.Marshal(issues)
			want = canonical(t, tc.Issues)
		} else {
			if len(issues) > 0 {
				got, _ = json.Marshal(issues)
			} else if _, missing := output.(undefinedValue); missing {
				got = []byte("null")
			} else {
				got, _ = json.Marshal(output)
			}
			want = canonical(t, tc.Output)
		}
		if string(got) != string(want) {
			failures++
			if failures <= 12 {
				in := canonical(t, tc.Input)
				t.Errorf("case %d %s input %.150s:\n got %.400s\nwant %.400s", i, tc.Path, in, got, want)
			}
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d cases differ", failures, len(recorded.Cases))
	}
}

func canonical(t *testing.T, data json.RawMessage) []byte {
	t.Helper()
	value, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
