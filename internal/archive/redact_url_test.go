package archive

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// redactURLUserinfoReference is redactURLUserinfo as filter 11 wrote it,
// which read each URL's token again from its start: quadratic on URLs glued
// into one token, but simple. The linear scan must redact exactly as it
// did.
func redactURLUserinfoReference(s string) (string, bool) {
	if !strings.Contains(s, "://") {
		return s, false
	}
	matches := urlScheme.FindAllStringIndex(s, -1)
	if matches == nil {
		return s, false
	}
	var out strings.Builder
	last, hit := 0, false
	for _, match := range matches {
		start := match[1]
		if start < last {
			continue
		}
		tokenEnd := strings.IndexAny(s[start:], " \t\r\n\"'<>`")
		if tokenEnd < 0 {
			tokenEnd = len(s) - start
		}
		at := userinfoEndReference(s[start : start+tokenEnd])
		if at <= 0 {
			continue
		}
		out.WriteString(s[last:start])
		out.WriteString(redactedMarker)
		last, hit = start+at, true
	}
	if !hit {
		return s, false
	}
	out.WriteString(s[last:])
	return out.String(), true
}

func userinfoEndReference(token string) int {
	authorityEnd := strings.IndexAny(token, "/?#")
	if authorityEnd < 0 {
		authorityEnd = len(token)
	}
	authority := token[:authorityEnd]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		return at
	}
	colon := strings.IndexByte(authority, ':')
	if colon < 0 || isDigits(authority[colon+1:]) {
		return -1
	}
	for at := authorityEnd; at < len(token); at++ {
		if token[at] == '@' && isHostStartReference(token[at+1:]) {
			return at
		}
	}
	return -1
}

func isHostStartReference(rest string) bool {
	end := strings.IndexAny(rest, ":/?#")
	if end < 0 {
		end = len(rest)
	}
	host := rest[:end]
	if host == "" {
		return false
	}
	for i := range len(host) {
		c := host[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune(".-[]_", rune(c)) {
			return false
		}
	}
	return true
}

func checkURLUserinfoMatchesReference(t *testing.T, s string) {
	t.Helper()
	got, gotHit := redactURLUserinfo(s)
	want, wantHit := redactURLUserinfoReference(s)
	if got != want || gotHit != wantHit {
		t.Fatalf("redactURLUserinfo(%q)\n got  %q (hit=%v)\n want %q (hit=%v)", s, got, gotHit, want, wantHit)
	}
}

func urlUserinfoSeeds() []string {
	return []string{
		"postgres://u:p@ss@db/x",
		"postgres://u:pa/ss@db:5432/x",
		"https://medium.com/@user",
		"http://host:8080/?to=a@b",
		"http://a:b/@http://a:b/@",
		"http://xhttp://xhttp://x",
		`"http://x""http://u:p@h"`,
		"see https://u:p/q?r#s@host.example/path and ftp://@h",
		"http://a:b/c@[::1]:80 http://a:b/c@ http://a:b/@-_.x",
		"x-access-token:abc@github.com https://x-access-token:abc@github.com/o/r",
		strings.Repeat("http://a:b/", 20) + "@host",
		strings.Repeat("http://a:b/@", 5) + " " + strings.Repeat("s3://k:v@", 5),
	}
}

// The linear scan redacts exactly what the quadratic one did, on the seeds
// and on random text built from the characters a URL's userinfo scan cares
// about.
func TestURLUserinfoScanMatchesReference(t *testing.T) {
	t.Parallel()
	for _, s := range urlUserinfoSeeds() {
		checkURLUserinfoMatchesReference(t, s)
	}
	pieces := []string{"http://", "://", "a", "b", ":", "80", "@", "/", "?", "#", " ", `"`, "'", "<", "[", "]", "-", "_", ".", "\n", "é"}
	rng := rand.New(rand.NewPCG(12, 34))
	for range 20000 {
		var b strings.Builder
		for range rng.IntN(24) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		checkURLUserinfoMatchesReference(t, b.String())
	}
}

// FuzzURLUserinfoMatchesReference checks the linear scan against the
// quadratic one for any text.
func FuzzURLUserinfoMatchesReference(f *testing.F) {
	for _, s := range urlUserinfoSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 {
			t.Skip()
		}
		checkURLUserinfoMatchesReference(t, s)
	})
}
