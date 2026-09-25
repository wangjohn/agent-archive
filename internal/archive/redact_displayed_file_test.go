package archive

import (
	"strings"
	"testing"
)

// A private key is most often seen through a tool that displays the file:
// the Claude Code Read tool numbers every line (`     2→`), as do `cat -n`
// and `grep -n`; a diff marks lines with `+`; source code quotes them. Each
// is redacted whole. (Filter 11 as first written read only bare lines, so a
// key file read by Claude Code was not redacted at all; filter 10 took
// everything between BEGIN and END.)
func TestPrivateKeyInADisplayedFileIsRedacted(t *testing.T) {
	// Split so secret scanners do not take this file for a key.
	body := []string{"MIIEpAIBAAKCAQEA7syntheticAbCdEfGh12345678", "QkFTRTY0c3ludGhldGljYm9keWxpbmV0d28K", "c3ludGhldGljdGhpcmRsaW5l=="}
	begin, end := "-----BEGIN RSA "+"PRIVATE KEY-----", "-----END RSA "+"PRIVATE KEY-----"
	lines := append(append([]string{begin}, body...), end)
	decorate := func(format func(int, string) string, sep string) string {
		var out []string
		for i, line := range lines {
			out = append(out, format(i+1, line))
		}
		return strings.Join(out, sep)
	}
	cases := map[string]string{
		"read tool arrow":  decorate(func(i int, l string) string { return strings.Repeat(" ", 5) + itoa(i) + "→" + l }, "\n"),
		"cat -n":           decorate(func(i int, l string) string { return strings.Repeat(" ", 5) + itoa(i) + "\t" + l }, "\n"),
		"grep -n":          decorate(func(i int, l string) string { return itoa(i) + ":" + l }, "\n"),
		"diff added":       decorate(func(_ int, l string) string { return "+" + l }, "\n"),
		"quoted in email":  decorate(func(_ int, l string) string { return "> " + l }, "\n"),
		"indented yaml":    "key: |\n" + decorate(func(_ int, l string) string { return "    " + l }, "\n"),
		"go string concat": decorate(func(_ int, l string) string { return "\t\"" + l + "\\n\" +" }, "\n"),
		"json array":       "[\n" + decorate(func(_ int, l string) string { return "  \"" + l + "\"," }, "\n") + "\n]",
		"crlf numbered":    decorate(func(i int, l string) string { return "  " + itoa(i) + "→" + l }, "\r\n"),
		"unknown decoration, long lines": decorate(func(_ int, l string) string {
			return "|| " + l + strings.Repeat("A", 40) + " ||"
		}, "\n"),
	}
	for name, in := range cases {
		out, hit := redactSensitive(in)
		for _, line := range body {
			if strings.Contains(out, line[:20]) {
				t.Errorf("%s: key body survived (hit=%v):\n%s", name, hit, out)
				break
			}
		}
	}
	// Truncated (no END line) and numbered: the body is still taken, and
	// what follows it is kept.
	truncated := "     1→" + begin + "\n     2→" + body[0] + "\n     3→" + body[1] + "\n     4→func next() {}"
	out, _ := redactSensitive(truncated)
	if strings.Contains(out, body[0][:20]) || strings.Contains(out, body[1][:20]) || !strings.Contains(out, "func next() {}") {
		t.Errorf("truncated numbered key: %q", out)
	}
	// Code that names both armor lines, with ordinary code between, is kept.
	src := "const header = \"" + begin + "\"\n\nfunc IsKey(b []byte) bool {\n\treturn bytes.HasPrefix(b, []byte(header))\n}\n\nconst footer = \"" + end + "\""
	if out, hit := redactSensitive(src); hit || out != src {
		t.Errorf("code naming the armor lines was redacted: %q", out)
	}
	numbered := "  1→const header = \"" + begin + "\"\n  2→func IsKey() {}\n  3→const footer = \"" + end + "\""
	if out, hit := redactSensitive(numbered); hit || out != numbered {
		t.Errorf("numbered code naming the armor lines was redacted: %q", out)
	}
}
