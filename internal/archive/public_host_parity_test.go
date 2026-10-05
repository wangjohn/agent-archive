package archive

import (
	"net"
	"strings"
	"testing"
)

// Pure host classification must retain the pre-extraction IP refusal policy.
func TestPublicHostMatchesHistoricalIPPolicy(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"", "localhost", "LOCALHOST", "git.example.com", "example.com.", "single", "127.0.0.1", "0.0.0.0", "255.255.255.255", "001.002.003.004", "127.1", "256.1.1.1", "::1", "::ffff:192.0.2.1", "2001:db8::1", "fe80::1%en0", "[::1]", "[::ffff:192.0.2.1]", "127.0.0.1.example.com", "github.com:443", "user@github.com", "example.invalid/path", "a-b.example", "a_b.example"} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			want := host != "" && hostPattern.MatchString(host) && !strings.EqualFold(host, "localhost") && net.ParseIP(host) == nil && strings.Contains(host, ".")
			if got := publicHost(host); got != want {
				t.Fatalf("%q got=%v historical=%v", host, got, want)
			}
		})
	}
}
