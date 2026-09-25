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

// A key cut off or split has no END line, or no BEGIN line: a file read in
// two parts, a tool result in chunks, `cat -A` or `grep -rn` output cut
// short, a one-line key cut short. Each part's key lines are redacted.
// Certificates and public keys, next to a key or alone, are kept.
func TestPartialPrivateKeyIsRedacted(t *testing.T) {
	b64 := func(n int, seed string) string { return strings.Repeat(seed, n/len(seed)+1)[:n] }
	l1, l2, l3 := b64(64, "MIIEpAIBAAKCAQEA7syn"), b64(64, "QkFTRTY0c3ludGhldGlj"), b64(40, "c3ludGhldGljdGhpcmRs")+"=="
	// Split so secret scanners do not take this file for a key.
	begin, end := "-----BEGIN RSA "+"PRIVATE KEY-----", "-----END RSA "+"PRIVATE KEY-----"
	cases := map[string]string{
		"cat -A cut short":    begin + "$\n" + l1 + "$\n" + l2 + "$\n",
		"grep -rn cut short":  "key.pem:1:" + begin + "\nkey.pem:2:" + l1 + "\nkey.pem:3:" + l2,
		"one line cut short":  `"` + begin + " " + l1 + " " + l2,
		"second part":         l2 + "\n" + l3 + "\n" + end + "\n",
		"second part read":    "    27→" + l1 + "\n    28→" + l2 + "\n    29→" + l3 + "\n    30→" + end + "\n",
		"second part escaped": `{"output": "` + l1 + `\n` + l2 + `\n` + end + `\n"}`,
		"second part crlf":    "  " + l1 + "\r\n  " + l2 + "\r\n  " + end + "\r\n",
	}
	for name, in := range cases {
		out, hit := redactSensitive(in)
		if !hit || strings.Contains(out, l1[:24]) || strings.Contains(out, l2[:24]) {
			t.Errorf("%s: key body survived (hit=%v): %q", name, hit, out)
		}
		if again, _ := redactSensitive(out); again != out {
			t.Errorf("%s: not idempotent: %q then %q", name, out, again)
		}
	}
	// A second part keeps the text before the key and its display decoration.
	if out, _ := redactSensitive("    27→" + l1 + "\n    28→" + end); out != "    27→[REDACTED]" {
		t.Errorf("second part read: %q", out)
	}
	cert := "-----BEGIN CERTIFICATE-----\n" + l1 + "\n" + l2 + "\n-----END CERTIFICATE-----"
	public := "-----BEGIN PUBLIC KEY-----\n" + l1 + "\n" + l2 + "\n-----END PUBLIC KEY-----"
	for name, in := range map[string]string{
		"certificate": cert,
		"public key":  public,
		"ssh public":  "ssh-rsa " + l1 + l2 + " me@host",
		// A line of code above an END line is not a key's body.
		"code tail": "func footer() string {\n\treturn `\n" + end + "`\n}",
	} {
		if out, hit := redactSensitive(in); hit || out != in {
			t.Errorf("%s was redacted: %q", name, out)
		}
	}
	// A key cut short, then a certificate, then another key: both keys go,
	// the certificate stays.
	in := begin + "\n" + l1 + "\n" + cert + "\n" + begin + "\n" + l3 + "\n" + end
	out, _ := redactSensitive(in)
	if strings.Contains(out, l3[:24]) || !strings.Contains(out, cert) {
		t.Errorf("key, certificate, key: %q", out)
	}
	if rest := strings.Replace(out, cert, "", 1); strings.Contains(rest, l1[:24]) {
		t.Errorf("key cut short before a certificate survived: %q", out)
	}
}
