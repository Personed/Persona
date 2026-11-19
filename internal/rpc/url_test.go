package rpc

import "testing"

// Expectations recorded from z.url() in Zod 4.6.7 on Node.
func TestCanParseURL(t *testing.T) {
	for input, want := range map[string]bool{
		" https://example.com ":      true,
		"https://":                   false,
		"https://example.com:99999":  false,
		"https://example.com:065535": true,
		"http:example.com":           true,
		"https://exa mple.com":       false,
		"https://256.0.0.1":          false,
		"https://1.2.3":              true,
		"https://[::1]:80/":          true,
		"https://[::1":               false,
		"mailto:x":                   true,
		"a b:c":                      false,
		"not a url":                  false,
		"https://ex\tample.com":      true,
		"foo://":                     true,
		"foo://a b":                  false,
		"file:":                      true,
		"http://a@b@c":               true,
		"https://%41.com":            true,
		"https://%zz.com":            false,
		"https://ü.com":              true,
		"https://a..b":               true,
		"https://0x7f.1":             true,
		"https://08.0.0.1":           false,
		"javascript:alert(1)":        true,
		`https:\\x.com`:              true,
		"file://[::1":                false,
		"file://[::1]/x":             true,
		"file://a:1/":                false,
		"file://exa mple/":           false,
		"file:///x":                  true,
		"file://localhost/x":         true,
		"file:x":                     true,
		`file:\\[::1`:                false,
		"file://a@b/":                false,
		"file:/[::1":                 true,
		"file://":                    true,
	} {
		if got := canParseURL(input); got != want {
			t.Errorf("canParseURL(%q) = %v, want %v", input, got, want)
		}
	}
}
