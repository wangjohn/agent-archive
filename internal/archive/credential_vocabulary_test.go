package archive

import (
	"encoding/json"
	"strings"
	"testing"
)

// P-24: argument names are matched by the same vocabulary as redaction,
// word by word, so every credential spelling is covered and plurals such as
// max_tokens are not.
func TestIsCredentialKey(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"password", "passwd", "pass", "db_pass", "dbPass", "X-Api-Key", "api_key", "apikey", "ApiKey",
		"private_key", "privateKey", "private_key_id", "auth", "X-Auth", "basic_auth", "authorization",
		"token", "accessToken", "access_token", "refresh_token", "tokenValue", "secret", "client_secret",
		"secretKey", "credentials", "credential", "cookie", "cookies", "Set-Cookie", "passphrase",
		"password_confirmation", "new_password", "db_pwd", "pgpassword", "PGPASS", "AccountKey",
		"SharedAccessKey", "bearer", "encryption_key", "signing-key", "master.key",
	} {
		if !isCredentialKey(key) {
			t.Errorf("%q is a credential key", key)
		}
	}
	for _, key := range []string{
		"max_tokens", "input_tokens", "tokens", "author", "oauth_scopes", "bypass",
		"passage", "compass", "pwd", "cwd", "key", "keys", "name", "value", "session", "secretary_name",
		"keyboard", "path", "url", "text",
	} {
		if isCredentialKey(key) {
			t.Errorf("%q is not a credential key", key)
		}
	}
}

// Hardening for P-24: every word of the vocabulary marks both a key and, in
// text, an assignment; and every credential name blockedKeys drops outright
// is one the vocabulary knows, so the two lists cannot drift apart again.
func TestCredentialVocabularyCoversBothLayers(t *testing.T) {
	t.Parallel()
	for _, term := range credentialVocabulary {
		name := strings.ReplaceAll(term.words, " ", "_")
		if term.form == termSeparated {
			name = "db_" + name
		}
		if !isCredentialKey(name) {
			t.Errorf("vocabulary term %q: key %q is not denied", term.words, name)
		}
		if term.form == termKeyOnly {
			continue
		}
		in := strings.ToUpper(name) + "=Zq8WvK3pLmN5xR2t"
		if out, _ := redactSensitive(in); strings.Contains(out, "Zq8WvK3pLmN5xR2t") {
			t.Errorf("vocabulary term %q: %q -> %q", term.words, in, out)
		}
	}
	nonCredential := map[string]bool{"system": true, "developer": true, "instructions": true, "reasoning": true, "analysis": true,
		"encrypted_content": true, "image": true, "images": true, "audio": true, "binary": true, "attachment": true}
	for key := range blockedKeys {
		if !nonCredential[key] && !isCredentialKey(key) {
			t.Errorf("blockedKeys has %q, which the credential vocabulary does not know", key)
		}
	}
}

// An argument vector's secret values are redacted by their position, which
// the text patterns cannot see one element at a time.
func TestFilterV11RedactsArgumentVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   []any
		want []any
	}{
		{[]any{"mysql", "-uroot", "-pS3cret", "app"}, []any{"mysql", "-uroot", "-p[REDACTED]", "app"}},
		{[]any{"/usr/bin/curl", "-u", "admin:S3cret", "https://x.test"}, []any{"/usr/bin/curl", "-u", "admin:[REDACTED]", "https://x.test"}},
		{[]any{"sshpass", "-p", "S3cret", "ssh", "-p", "22", "host"}, []any{"sshpass", "-p", "[REDACTED]", "ssh", "-p", "22", "host"}},
		{[]any{"gh", "auth", "login", "--token", "S3cret"}, []any{"gh", "auth", "login", "--token", "[REDACTED]"}},
		{[]any{"docker", "login", "-u", "me", "--password", "S3cret"}, []any{"docker", "login", "-u", "me", "--password", "[REDACTED]"}},
	}
	for _, tc := range cases {
		got, hit := redactArgv(tc.in)
		if !hit || !jsonEqual(got, tc.want) {
			t.Errorf("redactArgv(%v) = %v (hit=%v), want %v", tc.in, got, hit, tc.want)
		}
	}
	for _, in := range [][]any{
		{"ssh", "-p", "22", "host"},
		{"mkdir", "-p", "a/b"},
		{"mysql", "-h", "db", "-p", "app"},
		{"curl", "-u", "justauser", "https://x.test"},
		{"echo", 1, "-p", "x"},
	} {
		if got, hit := redactArgv(in); hit || !jsonEqual(got, in) {
			t.Errorf("redactArgv(%v) = %v, want unchanged", in, got)
		}
	}
}
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
