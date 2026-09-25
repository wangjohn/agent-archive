package archive

import "testing"

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
