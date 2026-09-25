package archive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// Credential assignments in the shapes an agent sees when it reads a .env
// file, a shell profile, a config file, or a command line. Filter 8 required
// the trigger word to stand alone, so every snake_case or SCREAMING_CASE name
// and every quoted JSON key passed through. Only the value is replaced.
func TestFilterV9RedactsCredentialAssignments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// .env and shell
		{"env db password", "DB_PASSWORD=hunter2hunter2", "DB_PASSWORD=[REDACTED]"},
		{"env aws secret", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "AWS_SECRET_ACCESS_KEY=[REDACTED]"},
		{"export aws secret", "export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG", "export AWS_SECRET_ACCESS_KEY=[REDACTED]"},
		{"env aws session token", "AWS_SESSION_TOKEN=IQoJb3JpZ2luX2VjEXAMPLE", "AWS_SESSION_TOKEN=[REDACTED]"},
		{"env openai key", "OPENAI_API_KEY=abcd1234efgh5678", "OPENAI_API_KEY=[REDACTED]"},
		{"env github token", "GITHUB_TOKEN=abc123def456", "GITHUB_TOKEN=[REDACTED]"},
		{"env slack bot token", "SLACK_BOT_TOKEN=T0-synthetic-value", "SLACK_BOT_TOKEN=[REDACTED]"},
		{"env secret key", "SECRET_KEY=django-insecure-synthetic", "SECRET_KEY=[REDACTED]"},
		{"env stripe secret", "STRIPE_SECRET=rk_live_abcdefghijklmnop", "STRIPE_SECRET=[REDACTED]"},
		{"env glued pg", "PGPASSWORD=hunter2", "PGPASSWORD=[REDACTED]"},
		{"env double quoted spaces", `POSTGRES_PASSWORD="correct horse battery staple"`, `POSTGRES_PASSWORD="[REDACTED]"`},
		{"env single quoted", "export API_TOKEN='abc def'", "export API_TOKEN='[REDACTED]'"},
		{"env lines", "HOST=db.internal\nDB_PASSWORD=hunter2\nPORT=5432", "HOST=db.internal\nDB_PASSWORD=[REDACTED]\nPORT=5432"},
		// INI, TOML, properties
		{"aws credentials ini", "[default]\naws_secret_access_key = wJalrXUtnFEMI/K7MDENG", "[default]\naws_secret_access_key = [REDACTED]"},
		{"toml", `password = "hunter2"`, `password = "[REDACTED]"`},
		{"java properties", "spring.datasource.password=hunter2", "spring.datasource.password=[REDACTED]"},
		// YAML
		{"yaml", "password: hunter2hunter2", "password: [REDACTED]"},
		{"yaml nested quoted", "db:\n  db_password: \"hunter2\"\n  host: x", "db:\n  db_password: \"[REDACTED]\"\n  host: x"},
		// JSON, Python, JavaScript, Go, Ruby
		{"json", `{"password": "hunter2hunter2"}`, `{"password": "[REDACTED]"}`},
		{"json compact", `{"api_key":"abcd1234","user":"me"}`, `{"api_key":"[REDACTED]","user":"me"}`},
		{"json camel", `{"clientSecret": "abc", "accessToken": "def.ghi"}`, `{"clientSecret": "[REDACTED]", "accessToken": "[REDACTED]"}`},
		{"json escaped in text", `{\"password\":\"hunter2\"}`, `{\"password\":\"[REDACTED]\"}`},
		{"env in a json array", `"env": ["GITHUB_TOKEN=abc123", "CI=1"]`, `"env": ["GITHUB_TOKEN=[REDACTED]", "CI=1"]`},
		{"python dict", `{'api_key': 'abcd1234'}`, `{'api_key': '[REDACTED]'}`},
		{"javascript const", `const dbPassword = 'hunter2';`, `const dbPassword = '[REDACTED]';`},
		{"go short assignment", `password := "hunter2"`, `password := "[REDACTED]"`},
		{"ruby hash", `:password => 'hunter2'`, `:password => '[REDACTED]'`},
		{"npmrc", "//registry.npmjs.org/:_authToken=npm_synthetic123", "//registry.npmjs.org/:_authToken=[REDACTED]"},
		// HTTP
		{"authorization bearer", "Authorization: Bearer abc.def.ghi", "Authorization: Bearer [REDACTED]"},
		{"authorization header in quotes", `curl -H "Authorization: Bearer abc.def" https://x.test`, `curl -H "Authorization: Bearer [REDACTED]" https://x.test`},
		{"authorization basic", "authorization: Basic dXNlcjpwYXNz", "authorization: Basic [REDACTED]"},
		{"api key header", "curl -H 'x-api-key: abc123' https://api.example.test", "curl -H 'x-api-key: [REDACTED]' https://api.example.test"},
		{"query string", "GET /cb?access_token=abc123 HTTP/1.1", "GET /cb?access_token=[REDACTED] HTTP/1.1"},
		// Command-line flags
		{"flag equals", "mysql --password=hunter2 -u root", "mysql --password=[REDACTED] -u root"},
		{"flag space", "gh auth login --token abc123", "gh auth login --token [REDACTED]"},
		{"flag kebab space", "aws configure set --aws-secret-access-key wJalr -p x", "aws configure set --aws-secret-access-key [REDACTED] -p x"},
		// Names added after review
		{"rails secret key base", "SECRET_KEY_BASE=abc123def", "SECRET_KEY_BASE=[REDACTED]"},
		{"mysql pwd", "MYSQL_PWD=hunter2", "MYSQL_PWD=[REDACTED]"},
		{"db pwd", "export DB_PWD='hunter2'", "export DB_PWD='[REDACTED]'"},
		{"numbered password", "DB_PASSWORD_1=hunter2", "DB_PASSWORD_1=[REDACTED]"},
		{"glued number", "PASSWORD2=hunter2", "PASSWORD2=[REDACTED]"},
		{"numbered api key", "API_KEY_2=abcd1234", "API_KEY_2=[REDACTED]"},
		{"npmrc auth", "//registry.npmjs.org/:_auth=dXNlcjpwYXNz", "//registry.npmjs.org/:_auth=[REDACTED]"},
		{"npm config auth", "npm_config__auth=dXNlcjpwYXNz", "npm_config__auth=[REDACTED]"},
		{"credentials", "credentials=user:hunter2", "credentials=[REDACTED]"},
		{"encryption key", "ENCRYPTION_KEY=0123456789abcdef", "ENCRYPTION_KEY=[REDACTED]"},
		{"signing key", "JWT_SIGNING_KEY=0123456789abcdef", "JWT_SIGNING_KEY=[REDACTED]"},
		{"master key", "RAILS_MASTER_KEY=0123456789abcdef", "RAILS_MASTER_KEY=[REDACTED]"},
		{"sts key id", "aws_access_key_id = ASIAIOSFODNN7EXAMPLE", "aws_access_key_id = [REDACTED]"},
		{"value starting with equals", "PASSWORD==abc", "PASSWORD==[REDACTED]"},
		{"yaml value starting with equals", "token: =abc", "token: =[REDACTED]"},
		{"quoted value after equals", `PASSWORD=="abc"`, `PASSWORD=="[REDACTED]"`},
		// A marker-looking prefix does not shield what is glued to it.
		{"glued after marker", "password=[REDACTED]realsecret", "password=[REDACTED]"},
		{"glued after lowercase marker", "password=[redacted]realsecret", "password=[REDACTED]"},
		{"glued after bearer marker", "Authorization: Bearer [REDACTED]realsecret", "Authorization: Bearer [REDACTED]"},
		{"glued after two markers", "api_key=[REDACTED][REDACTED]realsecret", "api_key=[REDACTED]"},
		{"glued bracket group", "password=[REDACTED][a,b]realsecret", "password=[REDACTED]"},
		{"quoted value cut off after a backslash", `PASSWD=="0\`, `PASSWD=="[REDACTED]`},
		// Filter 10: what a shell glues onto a quoted value is part of it.
		{"quoted value after equals with a glued tail", `.PWD=='0'0`, `.PWD=='[REDACTED]'`},
		{"text glued after a closing quote", `PASSWORD="abc"realsecret`, `PASSWORD="[REDACTED]"`},
		{"quoted segments glued together", `export TOKEN='abc'"def"ghi && make`, `export TOKEN='[REDACTED]' && make`},
		{"glued tail stops at shell punctuation", `API_KEY="abc"secret|tee log`, `API_KEY="[REDACTED]"|tee log`},
		{"closing brace is not glued", `{"password":"abc"}`, `{"password":"[REDACTED]"}`},
		{"closing bracket is not glued", `["TOKEN='abc'x"]`, `["TOKEN='[REDACTED]'"]`},
		{"flag value with a glued tail", `mysql --password 'abc'def -u root`, `mysql --password '[REDACTED]' -u root`},
		// A single bracketed token is a value.
		{"bracketed value", "password=[hunter2]", "password=[REDACTED]"},
		{"braced value", "token={abc123def}", "token=[REDACTED]"},
		{"escaped quote inside escaped json", `{\"password\":\"ab\\\"cd\"}`, `{\"password\":\"[REDACTED]\"}`},
		{"doubly escaped json", `{\\\"password\\\":\\\"hunter2\\\"}`, `{\\\"password\\\":\\\"[REDACTED]\\\"}`},
		// Earlier shapes still redacted
		{"plain assignment", "password=hunter2", "password=[REDACTED]"},
		{"structural key under an assignment", "OPENAI_API_KEY=sk-proj-abcdefghijklmnop", "OPENAI_API_KEY=[REDACTED]"},
	}
	for _, tc := range cases {
		out, hit := redactSensitive(tc.in)
		if !hit || out != tc.want {
			t.Errorf("%s: redactSensitive(%q) = %q (hit=%v), want %q", tc.name, tc.in, out, hit, tc.want)
		}
	}
}

// Names that merely contain a trigger word, comparisons, and ordinary prose
// are not credential assignments. The documented false-positive class (a
// bare credential word assigned code, such as `token = parse(x)`) is pinned
// in TestFilterV3DoesNotRedactOrdinaryCode.
func TestFilterV9LeavesNonCredentialNamesUnchanged(t *testing.T) {
	negative := []string{
		"password_policy = strong",
		"max_tokens: 4096",
		`{"max_tokens": 4096, "input_tokens": 12}`,
		"token_count=12",
		"tokens := lexer.Tokenize(src)",
		"tokenizer: bpe",
		"the secretary filed the report",
		"SECRET_NAME=prod/db",
		"TOKEN_URL=https://auth.example.test/token",
		"passwordless: true",
		"use_token_auth: true",
		"if token == nil { return }",
		"if token != nil { return }",
		"if token.Kind == KindIdent { next() }",
		"func parseToken(t Token) error { return nil }",
		"echo $DB_PASSWORD",
		"PWD=/Users/someone/project",
		"OLDPWD=/Users/someone",
		"cd $PWD && ls",
		"auth: required",
		"oauth_callback=https://example.test/cb",
		"credential_helper=osxkeychain",
		"keyboard: us",
		"primary_key = id",
		`{"credentials": {"type": "service_account"}, "tokens": [1, 2]}`,
		"password: [required, min 8]",
		`if token == "" { return }`,
		"docker login --password-stdin < token.txt",
		"git config credential.helper osxkeychain",
		"gh auth login --with-token --hostname github.com",
		"Enter your password:",
	}
	for _, in := range negative {
		if out, hit := redactSensitive(in); hit || out != in {
			t.Errorf("redactSensitive(%q) = %q (hit=%v), want unchanged", in, out, hit)
		}
	}
}

// Pasted screenshots, PDFs, and images a tool read arrive as content blocks
// whose data is base64. Filter 8 kept them: their key names (type, source,
// data) were all on the allowlist. They are dropped whole, with a gap.
func TestFilterV9DropsBinaryContentBlocks(t *testing.T) {
	const payload = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk"
	lines := []string{
		// A Read of an image file: the tool result's content is an image block.
		`{"type":"user","uuid":"u1","timestamp":"2026-09-24T10:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + payload + `"}}]}]}}`,
		// A pasted screenshot and PDF beside the prompt's text.
		`{"type":"user","uuid":"u2","timestamp":"2026-09-24T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"what is wrong here?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + payload + `"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + payload + `"}}]}}`,
		// A block of any other type with a base64 source.
		`{"type":"user","uuid":"u3","timestamp":"2026-09-24T10:00:02Z","message":{"role":"user","content":[{"type":"text","text":"and this"},{"type":"future_media","source":{"type":"base64","data":"` + payload + `"}}]}}`,
		// A data URL inside ordinary text.
		`{"type":"assistant","uuid":"a1","timestamp":"2026-09-24T10:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"inline: <img src=\"data:image/png;base64,` + payload + `\"> done"}]}}`,
	}
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, payload[:24]) {
		t.Fatalf("base64 payload retained: %s", joined)
	}
	for _, kept := range []string{`"tool_use_id":"t1"`, "what is wrong here?", "and this", `data:image/png;base64,[OMITTED]\"\u003e done`} {
		if !strings.Contains(joined, kept) {
			t.Errorf("%q not retained: %s", kept, joined)
		}
	}
	for _, detail := range []string{"image block omitted", "document block omitted", "base64 source block omitted", "base64 data URL omitted"} {
		found := false
		for _, gap := range filtered.Gaps {
			found = found || (gap.Code == "binary_content_omitted" && gap.Detail == detail)
		}
		if !found {
			t.Errorf("no binary_content_omitted gap %q: %#v", detail, filtered.Gaps)
		}
	}

	// Codex writes pasted images as input_image with a data URL.
	codex := `{"type":"response_item","timestamp":"2026-09-24T10:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,` + payload + `"}]}}`
	filtered, err = (CodexAdapter{}).FilterJSONL(strings.NewReader(codex))
	if err != nil {
		t.Fatal(err)
	}
	joined = string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, payload[:24]) || strings.Contains(joined, "input_image") || !strings.Contains(joined, `"look"`) || !hasGap(filtered.Gaps, "binary_content_omitted") {
		t.Errorf("codex input_image: %s %#v", joined, filtered.Gaps)
	}
}

// `data` is no longer allowed globally: nothing this filter retains needs it,
// and it is where base64 content lives.
func TestFilterV9OmitsDataKey(t *testing.T) {
	line := `{"type":"user","uuid":"u1","timestamp":"2026-09-24T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hi","data":"opaque-synthetic-blob"}]}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "opaque-synthetic-blob") || !strings.Contains(joined, `"hi"`) {
		t.Errorf("data key retained: %s", joined)
	}
}

// A structured Cursor tool result was encoded to one string before the
// sanitizer saw it, so neither the key rules nor binary-block dropping ran:
// {"password": …, "image": …} came through whole. It is now sanitized like a
// tool-argument subtree first.
func TestCursorComposerSanitizesStructuredToolResults(t *testing.T) {
	composer := json.RawMessage(`{"_v":18,"composerId":"c1","createdAt":1758700000000,"status":"completed","fullConversationHeadersOnly":[{"bubbleId":"b1","type":2},{"bubbleId":"b2","type":2}]}`)
	bubble1 := json.RawMessage(`{"_v":3,"bubbleId":"b1","type":2,"toolFormerData":{"status":"completed","toolCallId":"t1","name":"read_file","rawArgs":"{\"path\":\"cfg.json\"}","result":{"contents":"host=db note=DB_PASSWORD=hunter2synthetic","password":"hunter2hunter2","image":"iVBORw0KGgoAAAANSUhEUg","nested":{"clientSecretValue":"abc","ok":1},"blocks":[{"type":"image","source":{"type":"base64","data":"QUJDREVGR0hJSktMTU5PUA"}}]}}}`)
	bubble2 := json.RawMessage(`{"_v":3,"bubbleId":"b2","type":2,"toolFormerData":{"status":"error","toolCallId":"t2","name":"run","rawArgs":"{}","error":{"message":"denied","api_token":"tok-synthetic"}}}`)
	filtered, err := (CursorAdapter{}).FilterComposer(CursorComposer{Composer: composer, Bubbles: []CursorBubble{{ID: "b1", Value: bubble1}, {ID: "b2", Value: bubble2}}})
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, leaked := range []string{"hunter2", "iVBORw0KGgo", "QUJDREVGR0hJSktMTU5PUA", "tok-synthetic", `\"abc\"`} {
		if strings.Contains(joined, leaked) {
			t.Errorf("%q survived a structured tool result: %s", leaked, joined)
		}
	}
	for _, kept := range []string{`host=db note=DB_PASSWORD=[REDACTED]`, `\"ok\":1`, `\"message\":\"denied\"`} {
		if !strings.Contains(joined, kept) {
			t.Errorf("%q not retained: %s", kept, joined)
		}
	}
	for _, code := range []string{"sensitive_or_hidden_field_omitted", "binary_content_omitted", "sensitive_content_redacted"} {
		if !hasGap(filtered.Gaps, code) {
			t.Errorf("no %s gap: %#v", code, filtered.Gaps)
		}
	}
}

// An empty structured result is kept as `{}`. One the sanitizer leaves
// nothing of is named among the omitted keys, not reported as a record
// without allowed fields (it is no record).
func TestCursorComposerStructuredResultEmptyAndFullyDropped(t *testing.T) {
	composer := json.RawMessage(`{"_v":18,"composerId":"c1","createdAt":1758700000000,"status":"completed","fullConversationHeadersOnly":[{"bubbleId":"b1","type":2},{"bubbleId":"b2","type":2}]}`)
	empty := json.RawMessage(`{"_v":3,"bubbleId":"b1","type":2,"toolFormerData":{"status":"completed","toolCallId":"t1","name":"noop","rawArgs":"{}","result":{}}}`)
	dropped := json.RawMessage(`{"_v":3,"bubbleId":"b2","type":2,"toolFormerData":{"status":"completed","toolCallId":"t2","name":"login","rawArgs":"{}","result":{"password":"hunter2hunter2","token":"tok-synthetic"}}}`)
	filtered, err := (CursorAdapter{}).FilterComposer(CursorComposer{Composer: composer, Bubbles: []CursorBubble{{ID: "b1", Value: empty}, {ID: "b2", Value: dropped}}})
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if !strings.Contains(joined, `"content":"{}"`) || strings.Contains(joined, "hunter2") || strings.Contains(joined, "tok-synthetic") {
		t.Fatalf("records: %s", joined)
	}
	if hasGap(filtered.Gaps, "record_without_allowed_fields_omitted") {
		t.Errorf("structured result reported as a record: %#v", filtered.Gaps)
	}
	found := false
	for _, gap := range filtered.Gaps {
		found = found || gap.Code == "unknown_field_omitted" && strings.Contains(gap.Detail, "tool.result")
	}
	if !found {
		t.Errorf("fully dropped result not named: %#v", filtered.Gaps)
	}
}

// The 64 KB string cap cuts on a character boundary, so a retained string is
// always valid UTF-8.
func TestFilterV9TruncatesOnRuneBoundary(t *testing.T) {
	const maxTextBytes = 64 * 1024
	text := strings.Repeat("a", maxTextBytes-1) + "é" + "tail"
	state := sanitizeState{addGap: func(string, int, string) {}}
	out, keep := sanitizeValue(text, &state)
	got, _ := out.(string)
	if !keep || !utf8.ValidString(got) || got != strings.Repeat("a", maxTextBytes-1) {
		t.Fatalf("truncated to %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}
