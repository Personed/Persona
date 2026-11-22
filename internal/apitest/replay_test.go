package apitest

import "testing"

func TestNormalizeJSON(t *testing.T) {
	if got, err := normalizeJSON([]byte("{\"json\":[]}\n")); err != nil || got != `{"json":[]}` {
		t.Errorf("normalizeJSON = %q, %v", got, err)
	}
	if _, err := normalizeJSON([]byte(`{"json":[]}lixo`)); err == nil {
		t.Error("trailing data accepted")
	}
}
