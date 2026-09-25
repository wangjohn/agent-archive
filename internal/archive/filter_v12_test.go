package archive

import (
	"strings"
	"testing"
)

// Filter 12: a private key encoded in base64 whole (kubeconfig's
// client-key-data) and Docker's registry login shown as text passed
// through filter 11. The synthetic values are split so secret scanners do
// not take this file for a leak.
func TestFilterV12RedactsEncodedKeysAndDockerLogins(t *testing.T) {
	t.Parallel()
	key := "LS0tLS1" + "CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFb3dJQkFBS0NBUUVBc3ludGhldGljCi0tLS0tRU5EIFJTQSBQUklWQVRFIEtFWS0tLS0tCg=="
	cert := "LS0tLS1" + "CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSXN5bnRoZXRpYwotLS0tLUVORCBDRVJUSUZJQ0FURS0tLS0tCg=="
	login := "Ym9iOmdo" + "cF9ub3RhcmVhbHRva2VuMDAwMDAwMDAwMA=="
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"kubeconfig client key", "    client-key-data: " + key, "    client-key-data: [REDACTED]"},
		{"kubeconfig in a numbered read", "    12→    client-key-data: " + key, "    12→    client-key-data: [REDACTED]"},
		{"encoded certificate", "    certificate-authority-data: " + cert, "    certificate-authority-data: [REDACTED]"},
		{"docker auth", `      "auth": "` + login + `"`, `      "auth": "[REDACTED]"`},
		{"docker auth in a numbered read", "     4→      \"auth\": \"" + login + "\"", "     4→      \"auth\": \"[REDACTED]\""},
		{"docker auth after command output", `Exit code 0` + "\n" + `{"auths":{"ghcr.io":{"auth":"` + login + `"}}}`, `Exit code 0` + "\n" + `{"auths":{"ghcr.io":{"auth":"[REDACTED]"}}}`},
		{"docker auth escaped in a string", `{\"auth\": \"` + login + `\"}`, `{\"auth\": \"[REDACTED]\"}`},
		{"docker identity token", `"identitytoken": "` + login + `"`, `"identitytoken": "[REDACTED]"`},
	}
	for _, tc := range cases {
		out, hit := redactSensitive(tc.in)
		if !hit || out != tc.want {
			t.Errorf("%s: redactSensitive(%q)\n got  %q (hit=%v)\n want %q", tc.name, tc.in, out, hit, tc.want)
		}
	}
}

// The new patterns leave ordinary text that shares their words alone.
func TestFilterV12LeavesBenignTextUnchanged(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`"auth": "required"`,
		`{"auth": true, "author": "Ada Lovelace"}`,
		`"author": "c2hvdWxkIG5vdCBtYXRjaA=="`,
		"LS0tLS1CRUdJTi",
		"the auth flow uses OAuth",
	} {
		if out, hit := redactSensitive(in); hit || out != in {
			t.Errorf("redactSensitive(%q) = %q (hit=%v), want it unchanged", in, out, hit)
		}
	}
}

// Filter 12 also redacts Groq and xAI keys by their prefix, access key IDs
// by name, a .pgpass file's passwords when the string names the file, and
// wallet seed phrases. Each passed through before. The synthetic values
// are split so secret scanners do not take this file for a leak.
//
// Regression: 2026-09 review of filter 12.
func TestFilterV12RedactsKeyIDsPgpassAndSeedPhrases(t *testing.T) {
	t.Parallel()
	groq := "gs" + "k_" + strings.Repeat("Ab3", 17) + "Z"
	xai := "xa" + "i-" + strings.Repeat("Qx7z", 20)
	hex32 := strings.Repeat("0f3a9c1e", 4)
	words := func(n int) string {
		list := []string{"abandon", "ability", "able", "about", "above", "absent", "absorb", "abstract"}
		out := make([]string, n)
		for i := range out {
			out[i] = list[i%len(list)]
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"groq key, plain name", "GROQ=" + groq, "GROQ=[REDACTED]"},
		{"groq key in prose", "use " + groq + " for now", "use [REDACTED] for now"},
		{"xai key, plain name", "XAI=" + xai, "XAI=[REDACTED]"},
		{"xai key in json", `{"key": "` + xai + `"}`, `{"key": "[REDACTED]"}`},
		{"r2 access key id", "R2_ACCESS_KEY_ID=" + hex32, "R2_ACCESS_KEY_ID=[REDACTED]"},
		{"aws access key id", "export AWS_ACCESS_KEY_ID=" + hex32, "export AWS_ACCESS_KEY_ID=[REDACTED]"},
		{"access key id in camel case json", `"accessKeyId": "` + hex32 + `"`, `"accessKeyId": "[REDACTED]"`},
		{"access key id as the dashboard shows it", "Access Key ID: " + hex32, "Access Key ID: [REDACTED]"},
		{"pgpass after cat", "$ cat ~/.pgpass\nlocalhost:5432:app:alice:" + "S3cr3t-pw", "$ cat ~/.pgpass\nlocalhost:5432:app:alice:[REDACTED]"},
		{"pgpass wildcard port, escaped colon", "cat .pgpass\n*:*:*:postgres:" + `pa\:ss w0rd`, "cat .pgpass\n*:*:*:postgres:[REDACTED]"},
		{"pgpass backup through grep -n", "/home/me/.pgpass.bak:3:db.internal:6432:app:bob:" + "hunter2", "/home/me/.pgpass.bak:3:db.internal:6432:app:bob:[REDACTED]"},
		{"pgpass by heredoc", "cat > ~/.pgpass <<EOF\ndb.internal:6432:app:bob:" + "hunter2\nEOF", "cat > ~/.pgpass <<EOF\ndb.internal:6432:app:bob:[REDACTED]\nEOF"},
		{"pgpass through head", "==> /home/me/.pgpass <==\n" + `\:\:1:5432:app:bob:` + "hunter2", "==> /home/me/.pgpass <==\n" + `\:\:1:5432:app:bob:[REDACTED]`},
		{"pgpass by echo", `echo "db:5432:app:alice:` + `S3cret" >> ~/.pgpass`, `echo "db:5432:app:alice:[REDACTED]" >> ~/.pgpass`},
		{"pgpass numbered read", "PGPASSFILE=/etc/app/pgpass\n     1\tdb:5432:app:alice:" + "S3cret\n     2\tdb:5432:app:bob:" + "0ther", "PGPASSFILE=/etc/app/pgpass\n     1\tdb:5432:app:alice:[REDACTED]\n     2\tdb:5432:app:bob:[REDACTED]"},
		{"mnemonic env, quoted", `MNEMONIC="` + words(12) + `"`, `MNEMONIC="[REDACTED]"`},
		{"mnemonic yaml, 24 words", "  mnemonic: " + words(24), "  mnemonic: [REDACTED]"},
		{"wallet mnemonic in json", `{"wallet_mnemonic": "` + words(12) + `", "n": 1}`, `{"wallet_mnemonic": "[REDACTED]", "n": 1}`},
		{"camel-case mnemonic", `walletMnemonic: '` + words(15) + `'`, `walletMnemonic: '[REDACTED]'`},
		{"seed phrase env", "SEED_PHRASE=" + words(12), "SEED_PHRASE=[REDACTED]"},
		{"seed phrase in prose", "seed phrase: " + words(24), "seed phrase: [REDACTED]"},
		{"secret recovery phrase", "Secret Recovery Phrase = " + words(12), "Secret Recovery Phrase = [REDACTED]"},
	}
	for _, tc := range cases {
		out, hit := redactSensitive(tc.in)
		if !hit || out != tc.want {
			t.Errorf("%s: redactSensitive(%q)\n got  %q (hit=%v)\n want %q", tc.name, tc.in, out, hit, tc.want)
		}
	}
}

// The new shapes leave alone what merely resembles them: short prefixed
// identifiers, other names ending in `_id`, five colon-separated fields
// with no .pgpass in sight, the .pgpass format as documentation writes it,
// and `mnemonic` as an assembler or prose uses it.
func TestFilterV12KeyIDsPgpassAndSeedPhrasesLeaveBenignTextUnchanged(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"model: xai-grok-4",
		"gsk_config and xai-sdk",
		"KMS_KEY_ID=alias/app",
		"access_key_id_length: 20",
		"12:30:45:00:01",
		"localhost:5432:app:alice:S3cret",
		"Each line of ~/.pgpass is hostname:port:database:username:password.",
		"instr.mnemonic = \"mov\"",
		"mnemonic: a short word list you learn by heart to recall a longer one",
		"MNEMONIC=" + strings.Repeat("word ", 10) + "word",
	} {
		if out, hit := redactSensitive(in); hit || out != in {
			t.Errorf("redactSensitive(%q) = %q (hit=%v), want it unchanged", in, out, hit)
		}
	}
}
