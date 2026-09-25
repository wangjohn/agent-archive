package archive

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// credentialTerm is one entry of the credential vocabulary: a word, or words
// (`api key`), that mark a name as holding a credential.
type credentialTerm struct {
	// words are the term's words, lower case, separated by single spaces. In
	// a name they may be joined by `_`, `.`, `-`, or nothing (`api_key`,
	// `api-key`, `apikey`, `ApiKey`).
	words string
	// form says how the term may sit at the end of a name in text.
	form credentialTermForm
}

// credentialTermForm says what may come before a term in a credential name.
type credentialTermForm int

const (
	// termGlued may have anything glued on before it: DB_PASSWORD,
	// accessToken, PGPASSWORD, spring.datasource.password.
	termGlued credentialTermForm = iota
	// termStandalone must stand alone or follow a separator or a camelCase
	// boundary: `pass`, DB_PASS, redis.pass, dbPass, but not bypass or
	// compass.
	termStandalone
	// termSeparated must follow a separator or a camelCase boundary:
	// MYSQL_PWD, dbPwd, but not a bare PWD or OLDPWD, the shell's working
	// directory.
	termSeparated
	// termKeyOnly marks a key name in structured data only. In free text the
	// word is far too common (`auth: required`), so the text pattern leaves
	// it out; a tool argument or JSON key named `auth` is still dropped.
	termKeyOnly
)

// credentialVocabulary is the single list of words that mark a name as
// holding a credential. Both layers of the filter use it: the text
// redaction patterns (credentialName, from which credentialAssignment and
// credentialFlag are built) and the key-name deny list for tool arguments and
// decoded JSON (isCredentialKey). Filter 10 and earlier kept two lists, and a
// tool argument named X-Api-Key, passwd, pass, private_key, or auth passed
// the key list although the text list knew most of them. A word added here
// is covered by both; TestCredentialVocabularyCoversBothLayers checks it.
var credentialVocabulary = []credentialTerm{
	{words: "api key"}, {words: "access key"}, {words: "private key"}, {words: "private key id"},
	{words: "encryption key"}, {words: "signing key"}, {words: "master key"}, {words: "account key"},
	{words: "shared access key"}, {words: "shared access signature"}, {words: "dockerconfigjson"},
	{words: "secret"}, {words: "password"}, {words: "passwd"}, {words: "passphrase"},
	{words: "token"}, {words: "authorization"}, {words: "bearer"}, {words: "credential"},
	{words: "credentials"}, {words: "cookie"}, {words: "cookies"}, {words: "pgpass"},
	{words: "pass", form: termStandalone},
	{words: "pwd", form: termSeparated},
	{words: "auth", form: termKeyOnly},
}

// termPattern is a term's words as a case-insensitive regexp fragment, the
// words joined by an optional separator.
func termPattern(term credentialTerm) string {
	return strings.Join(strings.Fields(term.words), `[_.-]?`)
}

// camelTermPattern is a term as it follows a camelCase boundary: a lower
// case letter or digit, then the term with its first letter upper case and
// the rest in either case (`dbPass`, `dbPwd`, but not `OLDPWD` or `BYPASS`).
func camelTermPattern(term credentialTerm) string {
	first, rest := term.words[:1], term.words[1:]
	return `(?-i:[a-z0-9]` + strings.ToUpper(first) + `)` + strings.ReplaceAll(rest, " ", `[_.-]?`)
}

// credentialNamePattern builds the text pattern for a credential name from
// credentialVocabulary. A name ends in a term (and the optional
// credentialSuffix); what may precede the term depends on its form.
func credentialNamePattern() string {
	var glued, alternatives []string
	for _, term := range credentialVocabulary {
		switch term.form {
		case termGlued:
			glued = append(glued, termPattern(term))
		case termStandalone:
			alternatives = append(alternatives,
				`(?:[a-z0-9_.-]*[_.-])?`+termPattern(term),
				`[a-z0-9_.-]*`+camelTermPattern(term))
		case termSeparated:
			alternatives = append(alternatives,
				`[a-z0-9_.-]*[_.-]`+termPattern(term),
				`[a-z0-9_.-]*`+camelTermPattern(term))
		case termKeyOnly:
		}
	}
	// npm's `_auth` (`:_auth=`, `npm_config__auth=`) is the one text form of
	// the key-only `auth`: a leading underscore makes it unambiguous.
	alternatives = append(alternatives, `(?:[a-z0-9_.-]*_)?_auth`)
	return `(?:[a-z0-9_.-]*?(?:` + strings.Join(glued, "|") + `)|` + strings.Join(alternatives, "|") + `)`
}

// credentialKeyName matches a whole key name that the text pattern would
// take as a credential name, such as a glued `apikey` or `pgpassword`.
var credentialKeyName = regexp.MustCompile(`(?i)^` + credentialName + `$`)

// isCredentialKey reports whether a key name (a tool argument, or a key of
// decoded JSON) names a credential. The key is split into words at `_`, `.`,
// `-`, spaces, and camelCase boundaries (`X-Api-Key` → x api key,
// `accessTokenURL` → access token url), and it is a credential key when the
// words contain a vocabulary term anywhere (`password_confirmation`,
// `tokenValue`), or when the whole key is a credential name as the text
// pattern reads one (`apikey`, `pgpassword`). `pwd` counts only after
// another word (`db_pwd`, not a working directory's `pwd`); plurals are
// different words, so `max_tokens` and `input_tokens` are not credentials.
func isCredentialKey(key string) bool {
	if credentialKeyName.MatchString(key) {
		return true
	}
	words := splitNameWords(key)
	for _, term := range credentialVocabulary {
		want := strings.Fields(term.words)
		for i := 0; i+len(want) <= len(words); i++ {
			if term.form == termSeparated && i == 0 {
				continue
			}
			if wordsEqual(words[i:i+len(want)], want) {
				return true
			}
		}
		// A term written as one glued word (`apikey`, `accesstoken` inside
		// `x_accesstoken`) is a word of its own.
		if len(want) > 1 {
			if slices.Contains(words, strings.Join(want, "")) {
				return true
			}
		}
	}
	return false
}

func wordsEqual(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitNameWords splits an identifier or label into lower-case words at
// every non-alphanumeric character and at camelCase boundaries, keeping an
// acronym together (`APIKey` → api key, `XMLHttpRequest` → xml http request).
func splitNameWords(name string) []string {
	var words []string
	var current []rune
	runes := []rune(name)
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) && len(current) > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		current = append(current, r)
	}
	flush()
	return words
}

// sensitiveLabel matches a label, name, or placeholder that says the value
// beside it is a secret a person typed: a password, PIN, one-time code, card
// number, security code, or social security number. It is matched against
// splitNameWords output joined by spaces, so `cardNumber`, `DB_PASSWORD`,
// and "Card number" all read as words and `spinner` or `shipping` do not
// contain `pin`.
var sensitiveLabel = regexp.MustCompile(`(?:^| )(?:pass|password|passwd|passcode|passphrase|pwd|pin|otp|totp|mfa|2fa|secret|token|cvv|cvc|csc|cvv2|card|ssn|iban|credential|credentials|api key|apikey|security code|verification code|one time|social security|current password|new password)(?: |$)`)

// sensitiveLabelKeys are the keys whose value labels the value beside it: a
// form field's name, label, or placeholder, a browser tool's element
// description ("Password field"), an input's type (`"type": "password"`) or
// autocomplete hint, or an environment entry's name (`{"name":
// "DB_PASSWORD", "value": …}`). Prose fields (description, title) are not
// labels: a title that mentions a password does not make the text beside it
// one.
var sensitiveLabelKeys = map[string]bool{
	"name": true, "label": true, "element": true, "placeholder": true,
	"field": true, "fieldname": true, "field_name": true, "selector": true,
	"aria_label": true, "arialabel": true, "aria-label": true, "id": true, "key": true,
	"type": true, "autocomplete": true, "for": true,
}

// hasSensitiveLabel reports whether an object carries a label that marks
// its value as a typed secret (see sensitiveLabel).
func hasSensitiveLabel(in map[string]any) bool {
	for key, value := range in {
		if !sensitiveLabelKeys[strings.ToLower(key)] {
			continue
		}
		label, ok := value.(string)
		if !ok || label == "" || len(label) > 256 {
			continue
		}
		if sensitiveLabel.MatchString(strings.Join(splitNameWords(label), " ")) {
			return true
		}
	}
	return false
}
