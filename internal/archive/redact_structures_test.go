package archive

import (
	"encoding/json"
	"strings"
	"testing"
)

// Shapes next to filter 11's that it first missed: a credential whose value
// is a structure, a name/value entry (Kubernetes env, HAR headers), a YAML
// block in a file shown with line numbers, names written with spaces, and
// a few more credential words. Each secret is redacted and the text around
// it kept.
func TestRedactsCredentialStructuresAndEntries(t *testing.T) {
	const secret = "Zq8WvK3pLmN5xR2t"
	cases := []struct {
		name string
		in   string
		want string
	}{
		// A credential-named object or array: its string values, not its keys.
		{"json object", `{"secret": {"value": "` + secret + `"}, "next": 1}`, `{"secret": {"value": "[REDACTED]"}, "next": 1}`},
		{"json array", `"passwords": ["` + secret + `", "` + secret + `"]`, `"passwords": ["[REDACTED]", "[REDACTED]"]`},
		{"python dict", `'credentials': {'github': '` + secret + `'}`, `'credentials': {'github': '[REDACTED]'}`},
		{"js object", `apiKey: {value: "` + secret + `"}`, `apiKey: {value: "[REDACTED]"}`},
		{"escaped json", `{\"apiKey\": {\"value\": \"` + secret + `\"}}`, `{\"apiKey\": {\"value\": \"[REDACTED]\"}}`},
		{"descriptive keys kept", `"credentials": {"type": "service_account", "key": "` + secret + `"}`, `"credentials": {"type": "service_account", "key": "[REDACTED]"}`},
		{"numbered pretty json", "     1\t{\n     2\t  \"secrets\": {\n     3\t    \"github\": \"" + secret + "\"\n     4\t  }\n     5\t}", "     1\t{\n     2\t  \"secrets\": {\n     3\t    \"github\": \"[REDACTED]\"\n     4\t  }\n     5\t}"},
		// Name/value entries.
		{"k8s env", "env:\n  - name: DB_PASSWORD\n    value: " + secret + "\n  - name: HOST\n    value: db", "env:\n  - name: DB_PASSWORD\n    value: [REDACTED]\n  - name: HOST\n    value: db"},
		{"k8s env quoted", "- name: API_TOKEN\n  value: \"" + secret + "\"", "- name: API_TOKEN\n  value: \"[REDACTED]\""},
		{"k8s env numbered", "  7→        - name: STRIPE_SECRET_KEY\n  8→          value: " + secret, "  7→        - name: STRIPE_SECRET_KEY\n  8→          value: [REDACTED]"},
		{"har header one line", `{"name": "Authorization", "value": "Basic ` + secret + `"}, {"name": "Accept", "value": "*/*"}`, `{"name": "Authorization", "value": "[REDACTED]"}, {"name": "Accept", "value": "*/*"}`},
		{"har cookie", `{"name":"Cookie","value":"sid=` + secret + `"}`, `{"name":"Cookie","value":"[REDACTED]"}`},
		// A YAML block value read through a line-numbering tool.
		{"numbered yaml block", "  12→  password: |\n  13→    " + secret + "\n  14→  user: bob", "  12→  password: |\n  13→    [REDACTED]\n  14→  user: bob"},
		{"cat -n yaml next line", "     3\tdb_password:\n     4\t  " + secret, "     3\tdb_password:\n     4\t  [REDACTED]"},
		// A YAML mapping or sequence under a credential key: its values,
		// at any depth, but not its keys, comments, or descriptive values.
		{"yaml mapping under password", "password:\n  value: " + secret + "\nuser: bob", "password:\n  value: [REDACTED]\nuser: bob"},
		{"yaml secrets mapping", "secrets:\n  db: " + secret + "\n  api: \"" + secret + "\"  # prod\nhost: x", "secrets:\n  db: [REDACTED]\n  api: \"[REDACTED]\"  # prod\nhost: x"},
		{"yaml sequence at the key's column", "passwords:\n- " + secret + "\n- " + secret + "\nnext: 1", "passwords:\n- [REDACTED]\n- [REDACTED]\nnext: 1"},
		{"yaml nested", "credentials:\n  github:\n    token: " + secret + "\n    type: pat\n  other: |\n    " + secret + "\nnext: 1", "credentials:\n  github:\n    token: [REDACTED]\n    type: pat\n  other: |\n    [REDACTED]\nnext: 1"},
		{"yaml mapping numbered", "  1→secrets:\n  2→  db: " + secret + "\n  3→other: x", "  1→secrets:\n  2→  db: [REDACTED]\n  3→other: x"},
		{"yaml mapping in a list", "- credentials:\n    user: " + secret + "\n- name: x", "- credentials:\n    user: [REDACTED]\n- name: x"},
		// URL-encoded assignments in a nested query string or form body.
		{"url-encoded equals", "https://x.test/login?next=%2Fapp%3Fpassword%3D" + secret + "%26user%3Dbob&x=1", "https://x.test/login?next=%2Fapp%3Fpassword%3D[REDACTED]%26user%3Dbob&x=1"},
		{"url-encoded colon", "body=api_token%3A" + secret + "%2Cnext", "body=api_token%3A[REDACTED]%2Cnext"},
		{"url-encoded json", "q=%7B%22password%22%3A%22" + secret + "%22%7D", "q=%7B%22password%22%3A[REDACTED]"},
		// Names as a person writes them, and words filter 11 did not know.
		{"api key with a space", "API Key: " + secret, "API Key: [REDACTED]"},
		{"secret key with a space", "Secret Key = " + secret, "Secret Key = [REDACTED]"},
		{"full-width equals", "PASSWORD＝" + secret, "PASSWORD＝[REDACTED]"},
		{"value suffix", "CLIENT_SECRET_VALUE=" + secret, "CLIENT_SECRET_VALUE=[REDACTED]"},
		{"creds", "creds=" + secret, "creds=[REDACTED]"},
		{"auth key", "AUTH_KEY=" + secret, "AUTH_KEY=[REDACTED]"},
		{"azure subscription key", "Ocp-Apim-Subscription-Key: " + secret, "Ocp-Apim-Subscription-Key: [REDACTED]"},
		{"flag at the start of a line", "mysql \\\n--password " + secret, "mysql \\\n--password [REDACTED]"},
	}
	for _, tc := range cases {
		out, hit := redactSensitive(tc.in)
		if !hit || out != tc.want {
			t.Errorf("%s: redactSensitive(%q)\n got  %q (hit=%v)\n want %q", tc.name, tc.in, out, hit, tc.want)
		}
		if again, _ := redactSensitive(out); again != out {
			t.Errorf("%s: not idempotent: %q -> %q", tc.name, out, again)
		}
	}

	benign := []string{
		"- name: HOST\n  value: db.internal",
		"- name: DB_PASSWORD\n  valueFrom:\n    secretKeyRef:\n      name: db\n      key: password",
		`"secretKeyRef": {"name": "db", "key": "password"}`,
		"password: [required, min 8]",
		"scope=user%3Aemail%20repo%3Astatus&state=%2Fhome",
		`"credentials": {"type": "service_account"}`,
		"The API key is stored in the keychain.",
		"Rotate your secrets regularly.",
		`{"name": "Accept", "value": "application/json"}`,
	}
	for _, in := range benign {
		if out, hit := redactSensitive(in); hit || out != in {
			t.Errorf("benign text was redacted: %q -> %q", in, out)
		}
	}
}

// The structured rules know the same shapes: a header listed by name
// (`{"name": "Authorization", "value": …}`) or as a pair, and the plural
// credential words, drop their value in a tool's arguments or a JSON string.
func TestStructuredCredentialLabelsAndPairs(t *testing.T) {
	const secret = "Zq8WvK3pLmN5xR2t"
	for _, in := range []any{
		map[string]any{"headers": []any{map[string]any{"name": "Authorization", "value": "Basic " + secret}}},
		map[string]any{"headers": []any{map[string]any{"name": "Cookie", "value": "sid=" + secret}}},
		map[string]any{"headers": []any{map[string]any{"key": "X-Api-Key", "value": secret}}},
		map[string]any{"headers": []any{[]any{"X-Api-Key", secret}}},
		map[string]any{"secrets": map[string]any{"github": secret}},
		map[string]any{"passwords": []any{secret}},
	} {
		data, _ := json.Marshal(in)
		state := sanitizeState{addGap: func(string, int, string) {}}
		out, _ := sanitizeValue(string(data), &state)
		if s, _ := out.(string); strings.Contains(s, secret) {
			t.Errorf("%s: secret survived: %s", data, s)
		}
	}
	// JSON text that names a key twice decodes to the last value only, so
	// the earlier one was never sanitized: the text is re-encoded from what
	// was, never kept byte for byte.
	for _, in := range []string{
		`{"note":"` + secret + `","note":"ok"}`,
		`[{"a":{"b":"` + secret + `","b":1}}]`,
	} {
		state := sanitizeState{addGap: func(string, int, string) {}}
		if out, _ := sanitizeValue(in, &state); strings.Contains(out.(string), secret) {
			t.Errorf("%s: the duplicate key's first value survived: %v", in, out)
		}
	}
	for in, want := range map[string]bool{
		`{"a":1,"a":2}`: true, `{"a":{"b":1},"c":[{"b":2,"b":3}]}`: true, `[{"a":1},{"a":2}]`: false,
		`{"a":{"a":1}}`: false, `{"a":[1,{"b":2}],"b":3}`: false, `"x"`: false,
	} {
		if got := jsonHasDuplicateKeys(in); got != want {
			t.Errorf("jsonHasDuplicateKeys(%s) = %v", in, got)
		}
	}
	// A label that is not a credential keeps its value.
	state := sanitizeState{addGap: func(string, int, string) {}}
	keep := `{"headers":[{"name":"Accept","value":"application/json"}],"pair":["Content-Type","text/plain"]}`
	if out, _ := sanitizeValue(keep, &state); out != keep {
		t.Errorf("a plain header was changed: %v", out)
	}
}
