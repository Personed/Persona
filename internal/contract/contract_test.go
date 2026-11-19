package contract

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

var dir = filepath.Join("..", "..", "contracts", "api")

func TestContract(t *testing.T) {
	contract, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if contract.Transport != "orpc-rpc-1" || contract.Prefix != "/rpc" || len(contract.Procedures) == 0 {
		t.Fatalf("unexpected contract header %q %q with %d procedures", contract.Transport, contract.Prefix, len(contract.Procedures))
	}
	for _, procedure := range contract.Procedures {
		if procedure.Kind != "unary" && procedure.Kind != "stream" {
			t.Errorf("%s: kind %q", procedure.Path, procedure.Kind)
		}
		if !slices.Contains(procedure.Errors, "INTERNAL_SERVER_ERROR") {
			t.Errorf("%s: errors miss INTERNAL_SERVER_ERROR", procedure.Path)
		}
		if procedure.Input != nil && string(procedure.Input) != "null" && !json.Valid(procedure.Input) {
			t.Errorf("%s: invalid input schema", procedure.Path)
		}
	}
}

func TestFixturesMatchContract(t *testing.T) {
	contract, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := Fixtures(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures recorded")
	}
	for name, exchanges := range fixtures {
		for _, exchange := range exchanges {
			procedure, ok := contract.Procedure(exchange.Procedure)
			if !ok {
				t.Errorf("%s: %s is not in the contract", name, exchange.Procedure)
				continue
			}
			if exchange.Request.Path != procedure.HTTPPath(contract.Prefix) || exchange.Request.Method != "POST" {
				t.Errorf("%s: %s %s", name, exchange.Request.Method, exchange.Request.Path)
			}
			if exchange.Response.Status == 200 {
				continue
			}
			var failure struct {
				Code   string `json:"code"`
				Status int    `json:"status"`
			}
			var body struct {
				JSON json.RawMessage `json:"json"`
			}
			if err := json.Unmarshal(exchange.Response.Body, &body); err != nil || json.Unmarshal(body.JSON, &failure) != nil {
				t.Errorf("%s: %s error body is not an oRPC error", name, exchange.Procedure)
				continue
			}
			if failure.Status != exchange.Response.Status || !slices.Contains(procedure.Errors, failure.Code) {
				t.Errorf("%s: %s returned %d %s outside %v", name, exchange.Procedure, failure.Status, failure.Code, procedure.Errors)
			}
		}
	}
}
