package archive

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzRedactSensitive checks the invariants every redaction must keep,
// whatever the input: redacting twice changes nothing the first pass did not
// (so a republished snapshot is stable), and valid UTF-8 stays valid. The
// seed corpus in testdata/fuzz/FuzzRedactSensitive holds the credential
// shapes of TestFilterV9RedactsCredentialAssignments and the ordinary code
// of TestFilterV9LeavesNonCredentialNamesUnchanged.
func FuzzRedactSensitive(f *testing.F) {
	f.Add("DB_PASSWORD=hunter2")
	f.Fuzz(func(t *testing.T, in string) {
		once, _ := redactSensitive(in)
		twice, _ := redactSensitive(once)
		if twice != once {
			t.Fatalf("not idempotent:\n in   %q\n once %q\n twice %q", in, once, twice)
		}
		if utf8.ValidString(in) && !utf8.ValidString(once) {
			t.Fatalf("redaction broke UTF-8: %q -> %q", in, once)
		}
	})
}

// credentialTemplates are credential shapes with the secret left as %s.
var credentialTemplates = []string{
	"DB_PASSWORD=%s",
	"export AWS_SECRET_ACCESS_KEY=%s",
	"OPENAI_API_KEY=%s",
	"GITHUB_TOKEN=%s\nOTHER=1",
	`{"password": "%s"}`,
	`{"api_key":"%s","user":"me"}`,
	`{\"password\":\"%s\"}`,
	`{'api_key': '%s'}`,
	"password: %s",
	"  db_password: \"%s\"",
	"aws_secret_access_key = %s",
	`password = "%s"`,
	"spring.datasource.password=%s",
	"const dbPassword = '%s';",
	"Authorization: Bearer %s",
	"curl -H 'x-api-key: %s' https://x.test",
	"mysql --password=%s -u root",
	"gh auth login --token %s",
	"MYSQL_PWD=%s",
	"DB_PASSWORD_1=%s",
	"SECRET_KEY_BASE=%s",
	"//registry.npmjs.org/:_auth=%s",
	"ENCRYPTION_KEY=%s",
	"password=[%s]",
	"token={%s}",
	"password=[REDACTED]%s",
}

// plainSecret is the shape of a secret the templates are fuzzed with: long
// enough to be unmistakable in the output, free of quotes, separators,
// and whitespace, which would end the value early by design, and not
// starting with `-`, which after a flag is the next flag.
var plainSecret = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+/_.-]{7,63}$`)

// FuzzRedactCredentialTemplates puts fuzzed secrets into every credential
// shape and checks that none survives redaction.
func FuzzRedactCredentialTemplates(f *testing.F) {
	f.Add(uint(0), "hunter2hunter2")
	f.Fuzz(func(t *testing.T, index uint, secret string) {
		if !plainSecret.MatchString(secret) || strings.Contains(secret, "REDACTED") {
			t.Skip()
		}
		template := credentialTemplates[index%uint(len(credentialTemplates))]
		in := strings.Replace(template, "%s", secret, 1)
		out, hit := redactSensitive(in)
		if !hit || strings.Contains(out, secret) {
			t.Fatalf("secret survived: %q -> %q", in, out)
		}
	})
}
