package archive

import (
	"regexp"
	"strings"
	"testing"
)

// FuzzNormalizeRemoteURL checks what every input must satisfy: no panic, a
// key that is empty or the documented shape, and a normalized form with no
// edge slash.
func FuzzNormalizeRemoteURL(f *testing.F) {
	for _, seed := range []string{
		"", "https://user:token@github.com/acme/widget.git", "git@github.com:acme/widget.git",
		"ssh://git@github.com:2222/acme/widget", "file:///srv/widget", "C:\\src\\widget",
		"https://[::1]:8080/a/b", "://", "a:b:c", "https://h/%zz", "\x00", "git@:", "@@@:@@",
	} {
		f.Add(seed)
	}
	shape := regexp.MustCompile(`^repo-[0-9a-f]{16}$`)
	f.Fuzz(func(t *testing.T, in string) {
		if key := RepoKey(in); key != "" && !shape.MatchString(key) {
			t.Fatalf("RepoKey(%q) = %q, not repo- and 16 hex digits", in, key)
		}
		normalized := NormalizeRemoteURL(in)
		if normalized != "" && (normalized[0] == '/' || normalized[len(normalized)-1] == '/') {
			t.Fatalf("NormalizeRemoteURL(%q) = %q, has an edge slash", in, normalized)
		}
		if strings.ContainsFunc(normalized, isControl) || strings.HasSuffix(strings.ToLower(normalized), ".git") {
			t.Fatalf("NormalizeRemoteURL(%q) = %q, has a control character or a .git suffix", in, normalized)
		}
	})
}
