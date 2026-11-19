package rpc

import (
	"net"
	"strconv"
	"strings"
)

// canParseURL reports whether the WHATWG URL parser (URL.canParse, no base),
// which z.url() uses, accepts s. It checks what can fail in that parser: the
// scheme, the authority of special and hierarchical URLs, ports, and IPv4 and
// IPv6 hosts. Paths, queries and fragments are percent-encoded there and never
// fail. Non-ASCII domains are accepted without IDNA validation.
func canParseURL(s string) bool {
	s = stripTabAndNewline(strings.Trim(s, c0ControlOrSpace))
	colon := strings.IndexByte(s, ':')
	if colon < 1 || !validScheme(s[:colon]) {
		return false
	}
	scheme, rest := strings.ToLower(s[:colon]), s[colon+1:]
	switch scheme {
	case "file":
		return true
	case "http", "https", "ws", "wss", "ftp":
		rest = strings.TrimLeft(rest, `/\`)
		if end := strings.IndexAny(rest, `/\?#`); end >= 0 {
			rest = rest[:end]
		}
		return validAuthority(rest, true)
	}
	if !strings.HasPrefix(rest, "//") {
		return true
	}
	rest = rest[2:]
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	return validAuthority(rest, false)
}

const c0ControlOrSpace = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f" +
	"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

// stripTabAndNewline removes ASCII tab, LF and CR, as the URL parser and z.url() do.
func stripTabAndNewline(s string) string {
	return strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)
}

func urlScheme(s string) string {
	scheme, _, _ := strings.Cut(s, ":")
	return strings.ToLower(scheme)
}

func validScheme(scheme string) bool {
	for i, c := range []byte(scheme) {
		alpha := c|0x20 >= 'a' && c|0x20 <= 'z'
		other := c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
		if !alpha && (i == 0 || !other) {
			return false
		}
	}
	return true
}

func validAuthority(authority string, special bool) bool {
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	host, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 || !strings.Contains(authority[1:end], ":") || net.ParseIP(authority[1:end]) == nil {
			return false
		}
		rest := authority[end+1:]
		if rest != "" && rest[0] != ':' {
			return false
		}
		return validPort(strings.TrimPrefix(rest, ":"))
	}
	if i := strings.IndexByte(authority, ':'); i >= 0 {
		host, port = authority[:i], authority[i+1:]
	}
	if !validPort(port) {
		return false
	}
	if !special {
		return !strings.ContainsAny(host, "\x00\t\n\r #/:<>?@[\\]^|")
	}
	if host == "" {
		return false
	}
	host = percentDecode(host)
	for _, c := range []byte(host) {
		if c < 0x20 || c == 0x7f || strings.IndexByte(" #%/:<>?@[\\]^|", c) >= 0 {
			return false
		}
	}
	return validIPv4(host)
}

func validPort(port string) bool {
	if port == "" {
		return true
	}
	for _, c := range []byte(port) {
		if c < '0' || c > '9' {
			return false
		}
	}
	trimmed := strings.TrimLeft(port, "0")
	n, err := strconv.Atoi("0" + trimmed)
	return err == nil && len(trimmed) <= 5 && n <= 65535
}

func percentDecode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// validIPv4 applies the WHATWG IPv4 parser to a host whose last label is a
// number; other hosts are domains and pass.
func validIPv4(host string) bool {
	labels := strings.Split(host, ".")
	if len(labels) > 1 && labels[len(labels)-1] == "" {
		labels = labels[:len(labels)-1]
	}
	last := labels[len(labels)-1]
	hex := len(last) >= 2 && last[0] == '0' && last[1]|0x20 == 'x' && strings.Trim(last[2:], "0123456789abcdefABCDEF") == ""
	if !hex && (last == "" || strings.Trim(last, "0123456789") != "") {
		return true
	}
	if len(labels) > 4 {
		return false
	}
	for i, label := range labels {
		n, ok := ipv4Number(label)
		limit := uint64(255)
		if i == len(labels)-1 {
			limit = 1<<(8*(5-len(labels))) - 1
		}
		if !ok || n > limit {
			return false
		}
	}
	return true
}

func ipv4Number(label string) (uint64, bool) {
	if label == "" {
		return 0, false
	}
	base := 10
	switch {
	case len(label) >= 2 && label[0] == '0' && label[1]|0x20 == 'x':
		label, base = label[2:], 16
		if label == "" {
			return 0, true
		}
	case len(label) >= 2 && label[0] == '0':
		label, base = label[1:], 8
	}
	n, err := strconv.ParseUint(label, base, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
