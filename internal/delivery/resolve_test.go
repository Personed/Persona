package delivery

import "testing"

func TestResolve(t *testing.T) {
	for _, test := range []struct {
		goos, dir, input, absolute, relative string
	}{
		{"linux", "/work/bot", "out/a.txt", "/work/bot/out/a.txt", "out/a.txt"},
		{"linux", "/work/bot/", "./out/../a.txt", "/work/bot/a.txt", "a.txt"},
		{"linux", "/work/bot", "/work/bot/a.txt", "/work/bot/a.txt", "a.txt"},
		{"linux", "/work/bot", "/work/botany/a.txt", "", ""},
		{"linux", "/work/bot", "/work/Bot/a.txt", "", ""},
		{"linux", "/", "etc/hosts", "/etc/hosts", "etc/hosts"},
		{"darwin", "/Users/bot/work", `a\b.txt`, `/Users/bot/work/a\b.txt`, `a\b.txt`},
		{"windows", `C:\Users\bot\work`, `out\a.txt`, "C:/Users/bot/work/out/a.txt", "out/a.txt"},
		{"windows", `C:\Users\bot\work`, `c:\users\BOT\work\a.txt`, "c:/users/BOT/work/a.txt", "a.txt"},
		{"windows", `C:\Users\bot\work`, `D:\a.txt`, "", ""},
		{"windows", `C:\Users\bot\work`, `..\peer\a.txt`, "", ""},
		{"windows", `C:\Users\bot\work`, `a.txt::$DATA`, "", ""},
		{"windows", `C:\Users\bot\work`, `secret `, "", ""},
	} {
		absolute, relative, err := resolve(test.goos, test.dir, test.input)
		if absolute != test.absolute || relative != test.relative || (err == nil) != (test.absolute != "") {
			t.Errorf("resolve(%q, %q, %q) = %q, %q, %v", test.goos, test.dir, test.input, absolute, relative, err)
		}
	}
}
