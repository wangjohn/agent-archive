package archive

import (
	"regexp"
	"strings"
)

// injectedInstructionOpen matches the opening tag of a block a harness injects
// into an otherwise ordinary message: Claude Code wraps CLAUDE.md, hook output,
// and memory in <system-reminder>, and Codex writes AGENTS.md inside
// <user_instructions>, machine details inside <environment_context>, and a
// catalog of uninstalled plugins inside <recommended_plugins>. They are
// instructions to the model, not something the user wrote, so they are
// stripped from string content wherever they appear. Untagged instruction text
// is deliberately not guessed at.
var injectedInstructionOpen = regexp.MustCompile(`<(system-reminder|user_instructions|environment_context|recommended_plugins)\b[^>]*>`)

// injectedInstructionOpenByTag finds a further opening tag of one specific
// kind, so a block nested inside a block of the same kind extends the outer
// block instead of ending it early.
var injectedInstructionOpenByTag = map[string]*regexp.Regexp{
	"system-reminder":     regexp.MustCompile(`<system-reminder\b[^>]*>`),
	"user_instructions":   regexp.MustCompile(`<user_instructions\b[^>]*>`),
	"environment_context": regexp.MustCompile(`<environment_context\b[^>]*>`),
	"recommended_plugins": regexp.MustCompile(`<recommended_plugins\b[^>]*>`),
}

// stripInjectedInstructions removes every injected instruction block from one
// string. It reports whether anything was removed and returns the remaining
// text, which is empty when the string held nothing else. A block runs from
// its opening tag to the matching closing tag, counting nested blocks of the
// same kind; an opening tag whose block never closes, which a truncated or
// still-streaming record can produce, drops everything after it rather than
// retaining part of the block.
func stripInjectedInstructions(value string) (bool, string) {
	loc := injectedInstructionOpen.FindStringSubmatchIndex(value)
	if loc == nil {
		return false, value
	}
	var out strings.Builder
	rest := value
	for loc != nil {
		out.WriteString(rest[:loc[0]])
		end := injectedInstructionEnd(rest, loc[1], rest[loc[2]:loc[3]])
		if end < 0 {
			rest = ""
			break
		}
		rest = rest[end:]
		loc = injectedInstructionOpen.FindStringSubmatchIndex(rest)
	}
	out.WriteString(rest)
	return true, strings.TrimSpace(out.String())
}

// injectedInstructionEnd returns the index just past the closing tag which
// matches an opening tag of kind tag that ended at from, or -1 when the block
// never closes.
func injectedInstructionEnd(value string, from int, tag string) int {
	closeTag := "</" + tag + ">"
	open := injectedInstructionOpenByTag[tag]
	depth, pos := 1, from
	for depth > 0 {
		closeAt := strings.Index(value[pos:], closeTag)
		if closeAt < 0 {
			return -1
		}
		if openAt := open.FindStringIndex(value[pos:]); openAt != nil && openAt[0] < closeAt {
			depth++
			pos += openAt[1]
			continue
		}
		depth--
		pos += closeAt + len(closeTag)
	}
	return pos
}

// A credential assignment is a name that says it holds a credential, a
// separator, and a value: `DB_PASSWORD=…`, `export AWS_SECRET_ACCESS_KEY=…`,
// `"api_key": "…"`, `password: …`, `accessToken = "…"`, `--token=…`. Filter 8
// and earlier required the trigger word to stand alone (`\bpassword\b`), and
// since `_` is a word character that missed every snake_case or
// SCREAMING_CASE name, and a quote between the name and the separator missed
// every JSON key. The pattern is built from the named parts below. It is
// case-insensitive throughout. Only the value is replaced; the name and
// separator are kept so a reader can see which credential was there.
const (
	// credentialWords are the words that mark a name as holding a
	// credential, whatever is glued on before them.
	credentialWords = `api[_.-]?key|access[_.-]?key|private[_.-]?key|encryption[_.-]?key|signing[_.-]?key|master[_.-]?key|` +
		`secret|password|passwd|passphrase|token|authorization|bearer|credentials?`
	// credentialSeparatedWords are words that mark a credential only after a
	// separator: `pwd` (MYSQL_PWD, DB_PWD), because a bare PWD or OLDPWD is
	// the shell's working directory, and npm's `_auth` (`:_auth=`,
	// `npm_config__auth=`), because `auth` alone is far too common.
	credentialSeparatedWords = `[a-z0-9_.-]*[_.-]pwd|(?:[a-z0-9_.-]*_)?_auth` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialSuffix may follow the word: `key` or `access_key`, then
	// `base` (SECRET_KEY, AWS_SECRET_ACCESS_KEY, SECRET_KEY_BASE), then a
	// number (DB_PASSWORD_1, PASSWORD2, API_KEY_2).
	credentialSuffix = `(?:[_.-]?(?:access[_.-]?)?key(?:[_.-]?base)?)?(?:[_.-]?[0-9]+)?` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialName is a name ending in a credential word and optionally the
	// suffix. Anything may be glued on before the word (DB_PASSWORD,
	// accessToken, PGPASSWORD, spring.datasource.password, --password), but
	// nothing after it except the suffix, so `tokens`, `max_tokens`,
	// `secretary`, `password_policy`, and `TOKEN_URL` are not credential
	// names.
	credentialName = `(?:[a-z0-9_.-]*?(?:` + credentialWords + `)|` + credentialSeparatedWords + `)` + credentialSuffix
	// credentialLead is what may precede a name: the start of the string or a
	// character that cannot be part of one. It keeps a match from starting in
	// the middle of an identifier.
	credentialLead = `(?:^|[^a-z0-9_.-])` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialQuote is an optional quote around a name, as in JSON, Python,
	// or JSON escaped inside a string once or more (`\"password\"`,
	// `\\\"password\\\"`).
	credentialQuote = `(?:\\*["'])?` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialSeparator is `=`, `:`, `:=`, or `=>`, with spaces or tabs
	// around it but not newlines, so a YAML key with its value on the next
	// line does not swallow the line after it.
	credentialSeparator = `[ \t]*(?::=|=>|=|:)[ \t]*` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialScheme is an HTTP authorization scheme kept before the value
	// (`Authorization: Bearer [REDACTED]`).
	credentialScheme = `(?:(?:bearer|basic|digest|token)[ \t]+)?` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialQuotedValue is a value in quotes, up to its closing quote, or
	// to the end of the line when it has none, and for plain quotes whatever
	// is glued on after the closing quote (credentialQuotedGlue). In order:
	// JSON escaped once inside a string (`\"…\"`, where an escaped quote inside is `\\\"`);
	// JSON escaped more than once (`\\\"…\\\"`, up to the first escaped
	// quote of any depth); double quotes, with backslash escapes (a value
	// cut off after a lone backslash takes it along, so none is left for a
	// second pass to glue onto the marker); single quotes.
	credentialQuotedValue = credentialQuoted + `|(?:` + credentialPlainQuoted + `)` + credentialQuotedGlue
	credentialQuoted      = `\\"(?:\\\\\\"|[^"\\\n]|\\[^"\n])*(?:\\")?|` +
		`\\{2,}"(?:[^"\\\n]|\\+[^"\\\n])*(?:\\+")?`
	credentialPlainQuoted = `"(?:[^"\\\n]|\\.)+\\?"?|'[^'\n]+'?` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialQuotedGlue is what a shell reads as part of the same word
	// after a closing quote: more text, or more closed quoted segments
	// (`PASSWORD="abc"realsecret` is the value `abcrealsecret`). It is
	// taken as part of the value, so it is redacted with it. It stops at
	// whitespace, a separator, a closing bracket (the end of a JSON object
	// or array), or shell punctuation (`&&`, `|`, a redirection).
	credentialQuotedGlue = `(?:"(?:[^"\\\n]|\\.)*"|'[^'\n]*'|[^\s,;"'\]})&|<>])*` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialBracketedValue is a single token in brackets or braces
	// (`[hunter2]`, `{abc123}`, an earlier `[REDACTED]`), with whatever is
	// glued on after it (`[REDACTED]realsecret`, `[REDACTED][a,b]realsecret`:
	// a glued bracket group is taken whole, whatever it holds). A bracket holding
	// whitespace, a comma, a colon, or a quote is a structure
	// (`"credentials": {"type": …}`, `password: [required, min 8]`), whose
	// members are checked on their own, and is not a value: replacing its
	// opening bracket would break the line around it. Taking an earlier
	// [REDACTED] as a value also keeps redacting twice a no-op
	// (`Bearer [REDACTED]` is not read as the value `Bearer`).
	credentialBracketedValue = `(?:\[[^\s,:;"'\[\]{}]+\]|\{[^\s,:;"'\[\]{}]+\})(?:\[[^\]\n]*\]|\{[^}\n]*\}|[^\s,;"'])*` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialValue is a quoted value, a bracketed one, or an unquoted one,
	// which runs up to whitespace, `,`, `;`, or a quote, so a value inside a
	// quoted string (`-H 'x-api-key: abc'`, `["TOKEN=abc"]`) leaves the
	// closing quote. Each may follow extra `=` signs (`PASSWORD==abc`,
	// `PASSWORD=="abc"`, `token: =abc`), but no value begins with
	// whitespace, so `token == nil` is a comparison, not an assignment.
	credentialValue = `=*(?:` + credentialQuotedValue + `|` + credentialBracketedValue + `)|=*[^\s,;"'={\[][^\s,;"']*` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialFlagValue is the value after a space-separated command-line
	// flag (`--token abc`); one beginning with `-` is the next flag.
	credentialFlagValue = credentialQuotedValue + `|[^\s,;"'=-][^\s,;"']*` //nolint:gosec // G101: regex fragment naming credential words, not a credential
)

// credentialAssignment matches `name<sep>value` for a credential name, and
// credentialFlag a `--name value` command-line flag. Both capture the value
// as "value" so redactCredentialValues replaces only it. Their assignment
// form is deliberately broad and is a known false-positive class: ordinary
// code such as `token = parse(x)` matches it. See docs/agent-archive-privacy.md.
var (
	credentialAssignment = regexp.MustCompile(`(?i)` + credentialLead + credentialQuote + credentialName + credentialQuote + credentialSeparator + credentialScheme + `(?P<value>` + credentialValue + `)`)
	credentialFlag       = regexp.MustCompile(`(?i)(?:^|[ \t])--` + credentialName + `[ \t]+(?P<value>` + credentialFlagValue + `)`)
)

// credentialShape matches credentials recognizable by their own structure
// rather than by an assignment around them: a PEM private key block (from
// its BEGIN line through the next END line, or to the end of the string when
// the END line is missing), a JWT (three base64url segments, the first
// beginning with `eyJ`), GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`,
// `ghr_`, `github_pat_`), Slack tokens (`xox[baprs]-`), AWS access key IDs
// (`AKIA…`, and `ASIA…` for temporary STS keys), and Anthropic and OpenAI
// style `sk-` keys.
var credentialShape = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:.*?-----END [A-Z0-9 ]*-----|.*)|\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}|\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b|\bxox[baprs]-[A-Za-z0-9-]{10,}|\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\bsk-[A-Za-z0-9_-]{12,}\b`)

// quotedPrefix matches the quoted part at the start of a quoted value,
// without anything glued on after its closing quote.
var quotedPrefix = regexp.MustCompile(`^(?:` + credentialQuoted + `|` + credentialPlainQuoted + `)`)

// redactCredentialValues replaces the "value" group of every match of pattern
// with [REDACTED], keeping a quoted value's quotes, and reports whether
// anything was replaced.
func redactCredentialValues(pattern *regexp.Regexp, value string) (string, bool) {
	matches := pattern.FindAllStringSubmatchIndex(value, -1)
	if matches == nil {
		return value, false
	}
	group := pattern.SubexpIndex("value")
	var out strings.Builder
	last := 0
	for _, match := range matches {
		start, end := match[2*group], match[2*group+1]
		out.WriteString(value[last:start])
		// Extra `=` signs before the value stay (`PASSWORD==[REDACTED]`), and a
		// quoted value keeps its quotes, with any backslashes escaping them,
		// so the marker is never glued to text a second pass would take as
		// part of the value.
		secret := value[start:end]
		equals := len(secret) - len(strings.TrimLeft(secret, "="))
		out.WriteString(secret[:equals])
		secret, quote := secret[equals:], ""
		if escapes := len(secret) - len(strings.TrimLeft(secret, `\`)); escapes < len(secret) && (secret[escapes] == '"' || secret[escapes] == '\'') {
			quote = secret[:escapes+1]
		}
		out.WriteString(quote + "[REDACTED]")
		// Text glued on after the closing quote is part of the value and
		// goes with it; only the quoted part's closing quote is kept.
		if quote != "" {
			if quoted := quotedPrefix.FindString(secret); quoted != "" {
				secret = quoted
			}
		}
		// The closing quote, with whatever backslashes escape it (they can
		// differ from the opening's in malformed input), stays too.
		if quote != "" && len(secret) > len(quote) && secret[len(secret)-1] == quote[len(quote)-1] {
			body := strings.TrimRight(secret[len(quote):len(secret)-1], `\`)
			out.WriteString(secret[len(quote)+len(body):])
		}
		last = end
	}
	out.WriteString(value[last:])
	return out.String(), true
}

// urlUserinfo matches the userinfo of a URL (`scheme://user:pass@host`, or a
// bare `scheme://user@host`). Only the userinfo is replaced; the scheme and
// host stay so the reference remains legible.
var urlUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@]+@`)

// redactSensitive applies every value-level credential pattern to one string
// and reports whether anything was replaced. The narrow structural patterns
// run before the broad assignment patterns so that, for example, a
// `x-access-token:…@host` URL keeps its host instead of losing everything
// after the word "token".
func redactSensitive(value string) (string, bool) {
	redacted := false
	if urlUserinfo.MatchString(value) {
		value = urlUserinfo.ReplaceAllString(value, "${1}[REDACTED]@")
		redacted = true
	}
	if credentialShape.MatchString(value) {
		value = credentialShape.ReplaceAllString(value, "[REDACTED]")
		redacted = true
	}
	for _, pattern := range []*regexp.Regexp{credentialAssignment, credentialFlag} {
		if replaced, hit := redactCredentialValues(pattern, value); hit {
			value, redacted = replaced, true
		}
	}
	return value, redacted
}

// base64DataURL matches a base64 `data:` URL, the form Codex and browser
// tools use to inline an image or file in a string. The media type is kept;
// the payload is replaced (see sanitizeValue). Short payloads are left alone.
var base64DataURL = regexp.MustCompile(`(?i)\bdata:([a-z0-9.+-]+/[a-z0-9.+-]+)?((?:;[a-z0-9.+-]+=[a-z0-9.+-]+)*);base64,[A-Za-z0-9+/_-]{16,}=*`)

// binaryBlockTypes are content-block types that carry an image, a document,
// or audio rather than text: Claude's `image` and `document` blocks (their
// `source` is base64 data, a URL, or a file ID) and the OpenAI Responses and
// Chat Completions input shapes Codex writes. A block of one of these types
// is dropped whole at any depth, with a binary_content_omitted gap.
var binaryBlockTypes = map[string]bool{
	"image": true, "document": true, "input_image": true, "input_file": true,
	"input_audio": true, "image_url": true,
}

// binaryContentBlock reports whether an object is a content block carrying
// binary content: one of binaryBlockTypes, a `file` block with inline data,
// or any object whose `source` is `{"type": "base64", …}`. The returned
// detail is a fixed string, never taken from the input.
func binaryContentBlock(in map[string]any) (string, bool) {
	kind := strings.ToLower(strings.TrimSpace(firstString(in, "type")))
	if binaryBlockTypes[kind] {
		return kind + " block omitted", true
	}
	if source, ok := in["source"].(map[string]any); ok && strings.EqualFold(strings.TrimSpace(firstString(source, "type")), "base64") {
		return "base64 source block omitted", true
	}
	if kind == "file" {
		for _, key := range []string{"source", "data", "file_data"} {
			if _, present := in[key]; present {
				return "file block omitted", true
			}
		}
	}
	return "", false
}
