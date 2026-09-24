package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Adapter filters one application's hook-provided JSONL transcript. Adapters
// are deliberately readers only; the collector owns paths, retries, and I/O.
type Adapter interface {
	Name() string
	Version() string
	FilterJSONL(io.Reader) (FilteredTranscript, error)
}

// FilterError means no new source bundle may be made from this input. It is
// intentionally distinct from ParseError, where a filtered source is still
// safe and valuable to retain.
type FilterError struct{ Reason string }

// Error names the refusal's reason.
func (e *FilterError) Error() string { return "unsafe source format: " + e.Reason }

// ErrUnsafeSourceFormat means the input held no record of a type the adapter
// recognizes, so nothing in it is known to be safe to retain.
var ErrUnsafeSourceFormat = &FilterError{Reason: "no recognized safe records"}

// ErrRecordTooLarge means one JSONL record is longer than MaxRecordBytes. It is
// a FilterError like any other refusal, distinct so a caller can record it as
// the capture gap it is rather than a malformed transcript.
var ErrRecordTooLarge = &FilterError{Reason: "record exceeds the record size limit"}

// MaxRecordBytes is the largest single JSONL record the filter reads, and the
// collector's transcript size ceiling is defined from it (see
// collector.DefaultMaxTranscriptBytes), so any record inside a transcript the
// collector accepts can be read. Filter 4 and earlier stopped at 2 MB, which
// refused whole Claude Code sessions whose tool results are a few megabytes
// before filtering (the bulk is in toolUseResult, which the filter drops).
//
// Reading one record costs memory in proportion to its size: the scanner's
// buffer and the decoded JSON value both hold it, several times over at the
// limit. See docs/history/implementation-ledger.md for the measured ceiling.
const MaxRecordBytes = 64 * 1024 * 1024

// maxRecordBytes is MaxRecordBytes, as a variable only so a test can lower it.
var maxRecordBytes = MaxRecordBytes

const adapterVersion = "0.9.0"

// maxOmittedKeyNames bounds how many distinct omitted key names one filtered
// transcript reports, so a pathological source cannot grow the gap list.
const maxOmittedKeyNames = 64

// DefaultParserVersion is the source parser version reported by this bounded
// foundation. The parser is intentionally partial until fixture coverage proves
// a given native format more completely.
const DefaultParserVersion = "0.9.0"

// NewAdapter returns a privacy-first adapter by canonical harness name.
func NewAdapter(name string) (Adapter, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "codex":
		return CodexAdapter{}, nil
	case "claude", "claude-code":
		return ClaudeAdapter{}, nil
	case "cursor":
		return CursorAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported archive adapter %q", name)
	}
}

// CodexAdapter supports the conservative JSONL shapes observed by this
// foundation. Unsupported Codex record types are gaps, never pass-through.
type CodexAdapter struct{}

// Name returns the canonical harness name, "codex".
func (CodexAdapter) Name() string { return "codex" }

// Version returns the adapter version shared by every adapter.
func (CodexAdapter) Version() string { return adapterVersion }

// FilterJSONL keeps only the Codex record types this adapter recognizes,
// through the shared privacy filter, and labels the result codex-jsonl.
func (CodexAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "codex-jsonl", map[string]bool{
		"session_meta": true, "turn_context": true, "response_item": true,
		"event_msg": true, "message": true, "token_usage_record": true,
	})
}

// ClaudeAdapter handles a small, explicit subset of Claude Code JSONL event
// types. It does not claim schema coverage for every installed version.
type ClaudeAdapter struct{}

// Name returns the canonical harness name, "claude".
func (ClaudeAdapter) Name() string { return "claude" }

// Version returns the adapter version shared by every adapter.
func (ClaudeAdapter) Version() string { return adapterVersion }

// FilterJSONL keeps only the Claude Code record types this adapter recognizes,
// through the shared privacy filter, and labels the result claude-jsonl.
func (ClaudeAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "claude-jsonl", map[string]bool{
		"user": true, "assistant": true, "tool_use": true, "tool_result": true,
		"message": true, "summary": true,
	})
}

// CursorAdapter filters hook-provided JSONL records, a hook-provided text
// transcript (FilterText), and a chat read from Cursor's database
// (FilterComposer, in cursor_composer.go). Undocumented formats remain
// unsupported capture gaps upstream.
type CursorAdapter struct{}

// Name returns the canonical harness name, "cursor".
func (CursorAdapter) Name() string { return "cursor" }

// Version returns the adapter version shared by every adapter.
func (CursorAdapter) Version() string { return adapterVersion }

// FilterJSONL keeps only the Cursor record types this adapter recognizes,
// through the shared privacy filter, and labels the result cursor-jsonl.
func (CursorAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "cursor-jsonl", map[string]bool{
		"session": true, "message": true, "tool_call": true, "tool_result": true,
		"event": true, "turn_ended": true,
	})
}

// FilterText retains a hook-provided Cursor text transcript only when the hook
// has established that this is a fresh eligible session. It labels the source
// as text rather than fabricating message events from unstructured content.
//
// The transcript as a whole is bounded by the record size limit (a text
// transcript is one unit, like one JSONL record); a longer one fails with
// ErrRecordTooLarge, which the collector records as a capture gap. Filter 6
// and earlier stopped at 2 MB.
//
// Each visible role section — a `user:`, `assistant:`, or `tool:` line and the
// continuation lines under it — is sanitized on its own, so redaction and the
// 64 KB string cap apply per message, as they do to JSONL records. Filter 6
// sanitized the whole joined text as one string, which truncated any text
// transcript over 64 KB to its first 64 KB. The sanitized sections are joined
// again in their original order, one line per original line, so the result is
// read back by the same section prefixes (see textSectionPrefixes). Hidden
// sections (`system:`, `developer:`, `thinking:`, `analysis:`) and their
// continuation lines are omitted. Each gap is recorded once.
//
// The collector's rewrite guard compares the retained text by prefix across
// passes. Per-section sanitizing keeps every completed section's bytes
// stable, but the last section, if it is still being written, can change
// bytes it already produced once more of it lands (a credential that only
// matches when complete, or an injected block whose stripping trims the
// section's edges). That is a property of sanitizing a growing string, not
// of this function, and a text transcript offers no record boundary to stop
// short of.
func (CursorAdapter) FilterText(r io.Reader, freshStartedAt time.Time) (FilteredTranscript, error) {
	if freshStartedAt.IsZero() {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no reliable fresh-session start"}
	}
	content, err := io.ReadAll(io.LimitReader(r, int64(maxRecordBytes)+1))
	if err != nil {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript cannot be read"}
	}
	if len(content) > maxRecordBytes {
		return FilteredTranscript{}, ErrRecordTooLarge
	}
	result := FilteredTranscript{Format: "cursor-text", FirstEventAt: freshStartedAt.UTC()}
	gapSet := map[CaptureGap]bool{}
	addGap := func(code, detail string) {
		gap := CaptureGap{Code: code, Detail: detail}
		if !gapSet[gap] {
			gapSet[gap] = true
			result.Gaps = append(result.Gaps, gap)
		}
	}
	addGap("text_structure_partial", "Cursor role sections retained without manufactured events")

	// Split into sections, keeping each visible section's lines as they were.
	var sections [][]string
	hidden := false
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "system:") || strings.HasPrefix(lower, "developer:") || strings.HasPrefix(lower, "thinking:") || strings.HasPrefix(lower, "analysis:"):
			hidden = true
			addGap("hidden_instruction_omitted", "text section omitted")
		case strings.HasPrefix(lower, "user:") || strings.HasPrefix(lower, "assistant:") || strings.HasPrefix(lower, "tool:"):
			hidden = false
			sections = append(sections, []string{line})
		case hidden:
			// continuation line of an already-hidden section; omit.
		case len(sections) > 0:
			// continuation line of the current visible section's message body.
			sections[len(sections)-1] = append(sections[len(sections)-1], line)
		default:
			return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has unrecognized role section"}
		}
	}
	if len(sections) == 0 {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no retainable visible sections"}
	}
	state := sanitizeState{addGap: func(code string, _ int, detail string) { addGap(code, detail) }}
	retained := make([]string, 0, len(sections))
	for _, section := range sections {
		safe, keep := sanitizeValue(strings.Join(section, "\n"), &state)
		if !keep {
			continue
		}
		text, ok := safe.(string)
		if !ok {
			return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript is not text"}
		}
		retained = append(retained, text)
	}
	if len(retained) == 0 {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no retainable content"}
	}
	text := strings.Join(retained, "\n")
	result.Text = []string{text}
	result.Boundary.RetainedBytes = len(text)
	return result, nil
}

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
	// to the end of the line when it has none. In order: JSON escaped once
	// inside a string (`\"…\"`, where an escaped quote inside is `\\\"`);
	// JSON escaped more than once (`\\\"…\\\"`, up to the first escaped
	// quote of any depth); double quotes, with backslash escapes (a value
	// cut off after a lone backslash takes it along, so none is left for a
	// second pass to glue onto the marker); single quotes.
	credentialQuotedValue = `\\"(?:\\\\\\"|[^"\\\n]|\\[^"\n])*(?:\\")?|` +
		`\\{2,}"(?:[^"\\\n]|\\+[^"\\\n])*(?:\\+")?|` +
		`"(?:[^"\\\n]|\\.)+\\?"?|'[^'\n]+'?`
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
// code such as `token = parse(x)` matches it. See docs/security/privacy.md.
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

var allowedKeys = map[string]bool{
	"type": true, "id": true, "uuid": true, "session_id": true, "parent_id": true,
	"parent_uuid": true, "parentuuid": true, "timestamp": true, "created_at": true, "updated_at": true,
	"cwd": true, "model": true, "model_provider": true, "role": true, "content": true,
	"channel": true,
	"text":    true, "message": true, "item": true, "event": true, "payload": true,
	"tool_name": true, "tool_input": true, "tool_output": true, "tool_use": true,
	"tool_result": true, "call_id": true, "input": true, "output": true, "result": true, "arguments": true,
	"command": true, "path": true, "query": true, "url": true, "description": true,
	"status": true, "event_name": true, "turn_id": true, "reasoning_effort": true, "name": true, "items": true,
	"sha256": true, "message_id": true, "settings": true, "model_id": true, "discovered": true, "installed": true, "snapshot": true, "source": true,
	"coverage": true, "skills": true, "redacted": true, "observed_at": true,
	"model_params": true, "value": true, "cli_version": true, "agent_id": true,
	// "version" is the per-record Claude Code build stamp; it attributes a
	// published capture to an installed version. Values still pass sanitizeValue.
	"version":     true,
	"uncertainty": true, "scope": true, "original_bytes": true, "event_id": true,
	"truncated": true, "omitted_count": true, "snapshot_omitted_count": true, "inventory_complete": true, "root_status": true,
	"gaps":  true,
	"skill": true,
	// Claude Code marks a subagent's records with is_sidechain. Retaining the
	// flag lets a parent's normalized view exclude any inlined child records
	// from its own counts; the child is archived as its own session.
	"is_sidechain": true, "issidechain": true,
	"file_path":          true,
	"archive_session_id": true, "relationship": true,
	// Filter 3 additions. A tool result can only be joined back to the call it
	// answers through tool_use_id, and a turn's own identity (sessionId,
	// requestId, gitBranch) is what lets a reader place a record in its
	// session and working branch. usage and the Codex *_token_usage subtrees
	// are retained as numbers only (see numericSubtreeKeys).
	"tool_use_id": true, "tooluseid": true, "is_error": true, "stop_reason": true,
	"usage": true, "sessionid": true, "requestid": true, "gitbranch": true,
	// Codex token accounting records.
	"info": true, "total_token_usage": true, "last_token_usage": true,
	"turn_token_usage": true, "thread_token_usage": true, "last_agent_message": true,
	"thread_id": true, "root_turn_id": true, "completed_at_ms": true, "started_at_ms": true,
	// Filter 4: Claude Code marks harness-written user records (an expanded
	// skill or slash command, a local-command caveat) with isMeta. The flag is
	// what tells a parser such a record is not a human prompt; the record's
	// text itself is stripped (see stripMetaRecordText).
	"ismeta": true,
	// Filter 5: after /compact or auto-compaction Claude Code writes a user
	// record carrying a model-written summary of the earlier conversation,
	// marked isCompactSummary (and usually isVisibleInTranscriptOnly). The
	// flags tell a parser it is not a prompt. Unlike an isMeta record, the
	// summary's text is kept: it is model output, useful for a handoff. Both
	// are admitted as booleans only (see booleanFlagKeys).
	"iscompactsummary": true, "isvisibleintranscriptonly": true,
	// Filter 6: Claude Code says who produced a user record. A person's prompt
	// carries origin.kind "human"; a background-task completion carries
	// "task-notification" and promptSource "system". origin is retained only
	// as its kind string (see originKindOnly) and promptSource only as a
	// string, so a parser can tell a notification from a prompt.
	"origin": true, "promptsource": true,
}

// booleanFlagKeys are allowed only as the boolean flag the harness writes. Any
// other value under one of these names is prose the allowlist never retained.
var booleanFlagKeys = map[string]bool{"ismeta": true, "iscompactsummary": true, "isvisibleintranscriptonly": true}

// toolArgumentKeys name the subtrees which carry a tool call's own arguments.
// Filter 2 applied allowedKeys recursively inside them, which dropped every
// Edit old_string/new_string, Agent prompt, Grep pattern, and MCP argument and
// left tool evidence unusable. Inside these subtrees every argument name is
// retained; blockedKeys, value redaction (redactSensitive), the string cap, and the
// hidden role/channel rules all still apply to the values. Codex's
// payload.input is covered by the same "input" entry.
var toolArgumentKeys = map[string]bool{"input": true, "arguments": true, "tool_input": true}

// Retaining tool arguments wholesale has two exceptions, applied at every
// depth of a tool-argument subtree. The argument's key name is recorded in a
// gap; its value is never retained.
//
// typedInputArgumentKeys are the arguments which carry text a tool types or
// submits outward — into a browser field, a terminal, or a device — when the
// tool's name (`name` or `tool_name` beside the subtree) is one of
// typedInputToolNames, ends with `_` followed by one of them (an MCP tool such
// as `mcp__browser__computer`), or ends with one of typedInputToolSuffixes.
// What was typed into a login form is exactly the value a transcript must not
// keep, and the tool's own name is the only signal of that.
//
// credentialKeyFragments drop any argument whose lowercase key contains one
// of them, for every tool. This is broader than blockedKeys (which need an
// exact name) and knowingly catches budgets such as `max_tokens`.
var (
	typedInputArgumentKeys  = map[string]bool{"text": true, "value": true, "values": true}
	typedInputToolNames     = []string{"type", "form_input", "computer", "key", "enter_verification_code", "autofill_credential"}
	typedInputToolSuffixes  = []string{"_type", "_input", "_fill"}
	credentialKeyFragments  = []string{"password", "secret", "token", "credential", "api_key", "apikey", "cookie", "authorization"}
	deniedToolArgumentIntro = "omitted tool argument keys: "
)

func isTypedInputTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	for _, candidate := range typedInputToolNames {
		if lower == candidate || strings.HasSuffix(lower, "_"+candidate) {
			return true
		}
	}
	for _, suffix := range typedInputToolSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// deniedToolArgument reports whether a tool argument's value must be dropped
// even though the subtree otherwise retains every key.
func deniedToolArgument(key, toolName string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range credentialKeyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return typedInputArgumentKeys[lower] && isTypedInputTool(toolName)
}

// keyNameSet collects distinct key names for one summary gap, capped so a
// pathological source cannot grow the gap list. Names only, never values.
type keyNameSet struct {
	names  map[string]bool
	capped bool
}

func (s *keyNameSet) add(key string) {
	if s.names == nil {
		s.names = map[string]bool{}
	}
	if s.names[key] {
		return
	}
	if len(s.names) >= maxOmittedKeyNames {
		s.capped = true
		return
	}
	s.names[key] = true
}

// detail renders the sorted names after intro, noting when the cap was hit,
// or returns "" when nothing was collected.
func (s *keyNameSet) detail(intro string) string {
	if len(s.names) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.names))
	for key := range s.names {
		names = append(names, key)
	}
	sort.Strings(names)
	detail := intro + strings.Join(names, ", ")
	if s.capped {
		detail += "; further key names omitted"
	}
	return detail
}

// numericSubtreeKeys name subtrees retained for their numbers only: token
// accounting carries no prose, so anything in them which is not a number (or a
// nested object or array of numbers) is omitted with its key name recorded.
var numericSubtreeKeys = map[string]bool{
	"usage": true, "total_token_usage": true, "last_token_usage": true,
	"turn_token_usage": true, "thread_token_usage": true,
}

// captureGapKeys are additionally allowed inside a capture_gap evidence
// payload, whose whole content is an archive-authored code and its fixed
// description. They are deliberately not in allowedKeys: `detail` is a
// common free-text field name in native transcripts, and sanitizeObject
// recurses, so allowing it globally would retain arbitrary nested prose.
var captureGapKeys = map[string]bool{"code": true, "detail": true}

var blockedKeys = map[string]bool{
	"api_key": true, "apikey": true, "access_key": true, "secret": true,
	"secret_key": true, "password": true, "authorization": true, "token": true,
	"cookie": true, "set_cookie": true, "system": true, "developer": true,
	"instructions": true, "reasoning": true, "analysis": true, "encrypted_content": true,
	"image": true, "images": true, "audio": true, "binary": true, "attachment": true,
}

func filterJSONL(r io.Reader, format string, knownTypes map[string]bool) (FilteredTranscript, error) {
	result := FilteredTranscript{Format: format, NativeStartComplete: true}
	scanner := bufio.NewScanner(r)
	// Individual native JSONL records can contain tool output. A hard limit keeps
	// filtering bounded; exceeding it is refused rather than silently
	// truncated. The buffer holds a record plus its newline, so a record of
	// exactly maxRecordBytes is still read. bufio.Scanner allows the larger of
	// its maximum and the initial buffer's capacity, so the initial buffer
	// must not exceed the limit either.
	scanner.Buffer(make([]byte, min(64*1024, maxRecordBytes+1)), maxRecordBytes+1)
	lineNo, recognized := 0, 0
	gapSet := map[string]bool{}
	// Filter 2 collapsed every omission into one content-free gap, so a reader
	// could not see what this filter version was unable to keep. Collect the
	// distinct key names — names only, never values — and report them once.
	// Tool arguments dropped by the deny list are reported the same way under
	// their own code.
	var omittedKeys, deniedKeys keyNameSet
	addGap := func(code string, record int, detail string) {
		key := fmt.Sprintf("%s:%s", code, detail)
		if !gapSet[key] {
			gapSet[key] = true
			// Do not retain a source line number: an excluded preceding line must
			// not alter an otherwise identical retained snapshot.
			result.Gaps = append(result.Gaps, CaptureGap{Code: code, Detail: detail})
		}
	}
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		// bytes.TrimSpace, not strings.TrimSpace(string(line)): the same test
		// without copying a record that can be tens of megabytes.
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			result.NativeStartComplete = false
			addGap("incomplete_or_invalid_record", lineNo, "jsonl record omitted")
			continue
		}
		observed := parseNativeTimestamp(raw)
		if result.FirstEventAt.IsZero() {
			result.FirstEventAt = observed
		}
		if observed.IsZero() {
			if recordCarriesConversation(raw) {
				result.NativeStartComplete = false
			}
		} else if result.NativeStartAt.IsZero() || observed.Before(result.NativeStartAt) {
			result.NativeStartAt = observed
		}
		if !observed.IsZero() && (result.NativeEndAt.IsZero() || observed.After(result.NativeEndAt)) {
			result.NativeEndAt = observed
		}
		result.SessionIDs = appendUniqueString(result.SessionIDs, firstString(raw, "session_id", "sessionId"))
		result.AgentIDs = appendUniqueString(result.AgentIDs, firstString(raw, "agent_id", "agentId"))
		kind, _ := raw["type"].(string)
		if format == "claude-jsonl" && isCompactBoundary(raw) {
			recognized++
			encoded, err := json.Marshal(compactBoundaryRecord(raw, omittedKeys.add))
			if err != nil {
				return FilteredTranscript{}, &FilterError{Reason: "safe record cannot be encoded"}
			}
			result.Records = append(result.Records, encoded)
			result.Boundary.RetainedRecords++
			result.Boundary.RetainedBytes += len(encoded)
			continue
		}
		cursorRoleContent := format == "cursor-jsonl" && kind == "" && firstString(raw, "role") != ""
		if !knownTypes[kind] && !cursorRoleContent {
			addGap("unknown_record_type", lineNo, "record omitted")
			continue
		}
		recognized++
		if stripMetaRecordText(raw) {
			addGap("hidden_instruction_omitted", lineNo, "meta record text omitted")
		}
		state := sanitizeState{record: lineNo, addGap: addGap, omittedKey: omittedKeys.add, deniedKey: deniedKeys.add}
		safe, keep := sanitizeObject(raw, &state)
		if !keep {
			continue
		}
		encoded, err := json.Marshal(safe)
		if err != nil {
			return FilteredTranscript{}, &FilterError{Reason: "safe record cannot be encoded"}
		}
		result.Records = append(result.Records, encoded)
		result.Boundary.RetainedRecords++
		result.Boundary.RetainedBytes += len(encoded)
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return FilteredTranscript{}, ErrRecordTooLarge
		}
		return FilteredTranscript{}, &FilterError{Reason: "transcript cannot be read"}
	}
	if lineNo > 0 && recognized == 0 {
		return FilteredTranscript{}, ErrUnsafeSourceFormat
	}
	if detail := omittedKeys.detail("omitted keys: "); detail != "" {
		addGap("unknown_field_omitted", 0, detail)
	}
	if detail := deniedKeys.detail(deniedToolArgumentIntro); detail != "" {
		addGap("sensitive_or_hidden_field_omitted", 0, detail)
	}
	sort.SliceStable(result.Gaps, func(i, j int) bool { return result.Gaps[i].Code < result.Gaps[j].Code })
	return result, nil
}

// isCompactBoundary reports whether a Claude Code record is the marker it
// writes where a conversation was compacted: exactly type "system" with
// subtype "compact_boundary". Every other system record stays hidden.
func isCompactBoundary(record map[string]any) bool {
	kind, _ := record["type"].(string)
	subtype, _ := record["subtype"].(string)
	return kind == "system" && subtype == "compact_boundary"
}

// compactBoundaryIDKeys are the identifiers a compact_boundary record keeps
// beside its type, subtype, and timestamp. logicalParentUuid is how Claude
// Code links the boundary to the last record before the compaction.
var compactBoundaryIDKeys = map[string]bool{"uuid": true, "parentUuid": true, "logicalParentUuid": true, "sessionId": true}

// compactBoundaryRecord rebuilds a compact_boundary record from what filter 5
// retains of it: type, subtype, its ids, its timestamp, and the isSidechain
// flag (so a subagent's compaction inlined in a parent transcript is excluded
// from the parent's counts like the rest of the subagent's records). Nothing
// else of a system record is kept, text above all; each omitted key's name is
// reported through omit. It is built from an allowlist of typed values rather
// than sanitized, because a system record is otherwise hidden whole.
func compactBoundaryRecord(raw map[string]any, omit func(string)) map[string]any {
	out := map[string]any{"type": "system", "subtype": "compact_boundary"}
	for key, value := range raw {
		switch {
		case key == "type" || key == "subtype":
		case key == "timestamp":
			if stamp, ok := value.(string); ok && !parseNativeTimestamp(map[string]any{"timestamp": stamp}).IsZero() {
				out[key] = stamp
			} else {
				omit(key)
			}
		case compactBoundaryIDKeys[key]:
			// An id is kept only when it looks like one (see
			// looksLikeRecordID). A null parent is kept as null.
			if value == nil {
				out[key] = nil
			} else if id, ok := value.(string); ok && looksLikeRecordID(id) {
				out[key] = id
			} else {
				omit(key)
			}
		case key == "isSidechain":
			if flag, ok := value.(bool); ok {
				out[key] = flag
			} else {
				omit(key)
			}
		default:
			omit(key)
		}
	}
	return out
}

// looksLikeRecordID reports whether a string can be kept as a record id on a
// rebuilt compact_boundary record: a short token with no whitespace and no
// tag brackets, which the credential redaction would leave unchanged. The
// boundary bypasses sanitizeObject, so this is what keeps its ids under the
// same value rules as every other retained string.
func looksLikeRecordID(value string) bool {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, " \t\r\n<>") {
		return false
	}
	_, sensitive := redactSensitive(value)
	return !sensitive
}

// isMetaRecord reports whether a native record is one Claude Code marked as
// harness-written with isMeta: true.
func isMetaRecord(record map[string]any) bool {
	flag, ok := record["isMeta"].(bool)
	return ok && flag
}

// stripMetaRecordText removes the text of an isMeta record before it is
// sanitized, and reports whether anything was removed. Such a record carries a
// harness-expanded skill or slash command, or a local-command caveat: injected
// instruction text, like a <system-reminder> block, not something a person
// wrote. The record itself, its ids, its parent link, and the isMeta flag are
// kept so the conversation's parent chain survives; string content and text
// blocks are removed at every depth of the content. Any other block keeps its
// shape and identifiers (a tool result keeps its tool_use_id and is_error),
// loses its own text and nested content text, and is then left to the
// ordinary rules. A message left with nothing but its role still keeps the
// record.
func stripMetaRecordText(record map[string]any) bool {
	if !isMetaRecord(record) {
		return false
	}
	stripped := stripContentText(record)
	if message, ok := record["message"].(map[string]any); ok {
		if stripContentText(message) {
			stripped = true
		}
		if len(message) == 0 {
			delete(record, "message")
		}
	}
	return stripped
}

// stripContentText removes string content and text blocks from one object's
// "content", dropping the key when nothing is left. A block that survives
// (a tool result, for instance) is stripped the same way: its own "text" and
// its nested "content" text go too, so no text of the harness-written record
// remains at any depth.
func stripContentText(holder map[string]any) bool {
	content, present := holder["content"]
	if !present {
		return false
	}
	switch value := content.(type) {
	case string:
		delete(holder, "content")
		return true
	case map[string]any:
		return stripBlockText(value)
	case []any:
		kept := make([]any, 0, len(value))
		stripped := false
		for _, raw := range value {
			switch block := raw.(type) {
			case string:
				stripped = true
			case map[string]any:
				if textBlockTypes[strings.ToLower(strings.TrimSpace(firstString(block, "type")))] {
					stripped = true
					continue
				}
				if stripBlockText(block) {
					stripped = true
				}
				kept = append(kept, block)
			default:
				kept = append(kept, raw)
			}
		}
		if len(kept) == 0 {
			delete(holder, "content")
		} else {
			holder["content"] = kept
		}
		return stripped
	}
	return false
}

// stripBlockText removes a non-text block's own "text" string and the text
// of its nested "content", recursively.
func stripBlockText(block map[string]any) bool {
	stripped := false
	if _, isText := block["text"].(string); isText {
		delete(block, "text")
		stripped = true
	}
	if stripContentText(block) {
		stripped = true
	}
	return stripped
}

func appendUniqueString(values []string, candidate string) []string {
	if candidate == "" {
		return values
	}
	for _, existing := range values {
		if existing == candidate {
			return values
		}
	}
	return append(values, candidate)
}

// conversationRecordTypes are the record types whose start time is part of a
// session's timestamp provenance. Harnesses also write bookkeeping entries
// beside the conversation — Claude Code's `summary` and `file-history-snapshot`
// records are the observed examples — which carry no top-level timestamp and
// no conversational content. Treating those as missing provenance would make
// an otherwise fully timestamped transcript permanently ineligible for child
// capture, so only conversation-bearing records are required to be stamped.
var conversationRecordTypes = map[string]bool{
	"user": true, "assistant": true, "system": true, "message": true,
	"tool_use": true, "tool_result": true, "tool_call": true,
	"session_meta": true, "turn_context": true, "response_item": true,
	"event_msg": true, "session": true, "event": true,
}

func recordCarriesConversation(record map[string]any) bool {
	if _, present := record["message"]; present {
		return true
	}
	if firstString(record, "role") != "" {
		return true
	}
	kind, _ := record["type"].(string)
	return conversationRecordTypes[strings.ToLower(strings.TrimSpace(kind))]
}

func parseNativeTimestamp(record map[string]any) time.Time {
	for _, key := range []string{"timestamp", "created_at"} {
		value, _ := record[key].(string)
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

type sanitizeState struct {
	record int
	addGap func(string, int, string)
	// extraAllowed widens the key allowlist for one archive-authored payload
	// shape. It applies at every depth of that payload, which is safe only
	// because such payloads are flat maps this repository writes itself.
	extraAllowed map[string]bool
	// retainAllKeys is set while sanitizing a tool-argument subtree, where the
	// argument names are the tool's own vocabulary and no allowlist can
	// anticipate them. Value sanitization is unchanged.
	retainAllKeys bool
	// numericOnly is set while sanitizing a token-accounting subtree.
	numericOnly bool
	// toolName is the name of the tool whose argument subtree is being
	// sanitized, read from the `name` or `tool_name` beside that subtree. It
	// decides whether typed-input arguments are denied.
	toolName string
	// omittedKey, when set, receives the name of each key the filter could not
	// keep so the caller can report the distinct names once. Without it an
	// omission falls back to the content-free unknown_field_omitted gap.
	omittedKey func(string)
	// deniedKey, when set, receives the name of each tool argument dropped by
	// the deny list. Without it the drop falls back to the content-free
	// sensitive_or_hidden_field_omitted gap.
	deniedKey func(string)
}

func (s *sanitizeState) omitField(key string) {
	if s.omittedKey != nil {
		s.omittedKey(key)
		return
	}
	s.addGap("unknown_field_omitted", s.record, "field omitted")
}

func (s *sanitizeState) denyArgument(key string) {
	if s.deniedKey != nil {
		s.deniedKey(key)
		return
	}
	s.addGap("sensitive_or_hidden_field_omitted", s.record, "field omitted")
}

func sanitizeObject(in map[string]any, state *sanitizeState) (map[string]any, bool) {
	if role, _ := in["role"].(string); isHiddenRole(role) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	if channel, _ := in["channel"].(string); isHiddenChannel(channel) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	if kind, _ := in["type"].(string); isHiddenRole(kind) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	// Filter 9: a pasted screenshot, a PDF, or an image a tool read arrives
	// as a content block whose data is base64. Its key names pass the
	// allowlist (type, source), so the block is recognized by shape and
	// dropped whole, wherever it sits, rather than kept key by key.
	if detail, binary := binaryContentBlock(in); binary && !state.numericOnly {
		state.addGap("binary_content_omitted", state.record, detail)
		return nil, false
	}
	out := make(map[string]any)
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := in[key]
		lower := strings.ToLower(key)
		if blockedKeys[lower] {
			state.addGap("sensitive_or_hidden_field_omitted", state.record, "field omitted")
			continue
		}
		switch {
		case state.numericOnly:
			if !isNumericSubtreeValue(value) {
				state.omitField(key)
				continue
			}
		case state.retainAllKeys:
			// A tool argument's own name is retained; its value is not trusted,
			// and a typed-input or credential-named argument is dropped whole.
			if deniedToolArgument(key, state.toolName) {
				state.denyArgument(key)
				continue
			}
		case !allowedKeys[lower] && !state.extraAllowed[lower]:
			state.omitField(key)
			continue
		case lower == "origin":
			// Filter 6 admits origin only as {kind: <string>}. Every other
			// member is omitted by name without being inspected further.
			kind, ok := originKindOnly(value, state)
			if !ok {
				state.omitField(key)
				continue
			}
			out[key] = kind
			continue
		case lower == "promptsource":
			if _, isString := value.(string); !isString {
				state.omitField(key)
				continue
			}
		case booleanFlagKeys[lower]:
			// Filter 4 admits isMeta, and filter 5 isCompactSummary and
			// isVisibleInTranscriptOnly, only as the boolean flag Claude Code
			// writes. Any other value under those names is prose the allowlist
			// never retained, and stays omitted.
			if _, isFlag := value.(bool); !isFlag {
				state.omitField(key)
				continue
			}
		}
		retainAll, numericOnly, toolName := state.retainAllKeys, state.numericOnly, state.toolName
		switch {
		case numericSubtreeKeys[lower]:
			state.numericOnly, state.retainAllKeys = true, false
		case toolArgumentKeys[lower] && !state.numericOnly:
			if !state.retainAllKeys {
				state.toolName = firstString(in, "name", "tool_name")
			}
			state.retainAllKeys = true
		}
		safe, keep := sanitizeValue(value, state)
		state.retainAllKeys, state.numericOnly, state.toolName = retainAll, numericOnly, toolName
		if keep {
			out[key] = safe
		}
	}
	if len(out) == 0 {
		state.addGap("record_without_allowed_fields_omitted", state.record, "record omitted")
		return nil, false
	}
	for _, key := range []string{"payload", "message", "item", "event"} {
		if _, had := in[key]; had {
			if _, kept := out[key]; !kept {
				state.addGap("hidden_or_unknown_nested_content_omitted", state.record, "record omitted")
				return nil, false
			}
		}
	}
	return out, true
}

// originKindOnly reduces a Claude Code origin object to {kind: <string>},
// reporting every other member as omitted. It keeps nothing when origin is not
// an object or has no string kind.
func originKindOnly(value any, state *sanitizeState) (map[string]any, bool) {
	origin, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	for key := range origin {
		if key != "kind" {
			state.omitField(key)
		}
	}
	kind, ok := origin["kind"].(string)
	if !ok || strings.TrimSpace(kind) == "" {
		return nil, false
	}
	safe, keep := sanitizeValue(kind, state)
	if !keep {
		return nil, false
	}
	return map[string]any{"kind": safe}, true
}

func isHiddenChannel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "analysis", "reasoning", "thinking", "chain_of_thought":
		return true
	}
	return false
}

// isNumericSubtreeValue reports whether a value may appear in a numbers-only
// subtree. Objects and arrays are admitted so the recursion can prune them;
// everything else, including strings and booleans, is omitted there.
func isNumericSubtreeValue(value any) bool {
	switch value.(type) {
	case float64, map[string]any, []any:
		return true
	default:
		return false
	}
}

func sanitizeValue(value any, state *sanitizeState) (any, bool) {
	switch v := value.(type) {
	case nil, bool, float64:
		return v, true
	case string:
		if injected, stripped := stripInjectedInstructions(v); injected {
			state.addGap("hidden_instruction_omitted", state.record, "injected instruction block omitted")
			if stripped == "" {
				return nil, false
			}
			v = stripped
		}
		if base64DataURL.MatchString(v) {
			state.addGap("binary_content_omitted", state.record, "base64 data URL omitted")
			v = base64DataURL.ReplaceAllString(v, "data:${1}${2};base64,[OMITTED]")
		}
		if redacted, hit := redactSensitive(v); hit {
			state.addGap("sensitive_content_redacted", state.record, "content redacted")
			v = redacted
		}
		const maxTextBytes = 64 * 1024
		if len(v) > maxTextBytes {
			// Cut on a character boundary, so a retained string stays valid
			// UTF-8 (filter 8 could split a multi-byte character).
			state.addGap("content_truncated", state.record, "content truncated")
			v = TruncateUTF8(v, maxTextBytes)
		}
		return v, true
	case map[string]any:
		return sanitizeObject(v, state)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			// An array inside a numbers-only subtree is filtered per element,
			// since only sanitizeObject sees the key that admitted it.
			if state.numericOnly && !isNumericSubtreeValue(item) {
				state.addGap("unsupported_value_omitted", state.record, "value omitted")
				continue
			}
			safe, keep := sanitizeValue(item, state)
			if keep {
				out = append(out, safe)
			}
		}
		if len(v) > 0 && len(out) == 0 {
			state.addGap("hidden_or_unknown_nested_content_omitted", state.record, "field omitted")
			return nil, false
		}
		return out, true
	default:
		state.addGap("unsupported_value_omitted", state.record, "value omitted")
		return nil, false
	}
}

func isHiddenRole(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "system", "developer", "reasoning", "analysis", "thinking", "chain_of_thought", "agent_reasoning", "agent_reasoning_delta", "raw_agent_reasoning":
		return true
	default:
		return false
	}
}

// IsFilterError supports callers which need to retain a previous source bundle
// when a new native format cannot be safely filtered.
func IsFilterError(err error) bool {
	var target *FilterError
	return errors.As(err, &target)
}
