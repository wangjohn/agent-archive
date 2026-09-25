package archive

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// A credential assignment is a name that says it holds a credential, a
// separator, and a value: `DB_PASSWORD=…`, `export AWS_SECRET_ACCESS_KEY=…`,
// `"api_key": "…"`, `password: …`, `accessToken = "…"`, `--token=…`. The
// pattern is built from the named parts below and the words of
// credentialVocabulary. It is case-insensitive throughout. Only the value is
// replaced; the name and separator are kept so a reader can see which
// credential was there.
const (
	// credentialSuffix may follow the word: `key` or `access_key`, then
	// `base` (SECRET_KEY, AWS_SECRET_ACCESS_KEY, SECRET_KEY_BASE, and as a
	// person writes it, `Secret Key`), then a number (DB_PASSWORD_1,
	// PASSWORD2, API_KEY_2), then `value` (SECRET_VALUE, tokenValue).
	credentialSuffix = `(?:[_. -]?(?:access[_. -]?)?key(?:[_.-]?base)?)?(?:[_.-]?[0-9]+)?(?:[_.-]?value)?` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialLead is what may precede a name: the start of the string or a
	// character that cannot be part of one. It keeps a match from starting in
	// the middle of an identifier.
	credentialLead = `(?:^|[^a-z0-9_.-])` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialQuote is an optional quote around a name, as in JSON, Python,
	// or JSON escaped inside a string once or more (`\"password\"`,
	// `\\\"password\\\"`).
	credentialQuote = `(?:\\*["'])?` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialSeparator is `=`, `:`, `:=`, `=>`, or a full-width colon or
	// equals sign, with spaces or tabs around it but not newlines, so a YAML key with its
	// value on the next line does not swallow the line after it (see
	// redactYAMLBlockValues for that shape); or a tab alone, as in a
	// tab-separated `api_key<TAB>value` listing.
	credentialSeparator = `(?:[ \t]*(?::=|=>|=|:|：|＝)[ \t]*|[ \t]*\t[ \t]*)` //nolint:gosec // G101: regex fragment naming credential words, not a credential
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
	// credentialValue is a quoted value, a bracketed one, or an unquoted one.
	// The pattern takes an unquoted value only up to whitespace, `,`, `;`,
	// or a quote; redactAssignments then extends it to the end of the line
	// (see unquotedValueEnd). Each may follow extra `=` signs
	// (`PASSWORD==abc`, `PASSWORD=="abc"`, `token: =abc`), but no value
	// begins with whitespace, so `token == nil` is a comparison, not an
	// assignment.
	credentialValue = `=*(?:` + credentialQuotedValue + `|` + credentialBracketedValue + `)|=*[^\s,;"'={\[][^\s,;"']*` //nolint:gosec // G101: regex fragment naming credential words, not a credential
	// credentialFlagValue is the value after a space-separated command-line
	// flag (`--token abc`); one beginning with `-` is the next flag, and one
	// beginning with shell punctuation is not a value.
	credentialFlagValue = credentialQuotedValue + `|[^\s,;"'=|&<>()-][^\s,;"']*` //nolint:gosec // G101: regex fragment naming credential words, not a credential
)

// credentialName is a name ending in a credentialVocabulary term and
// optionally the suffix. Anything may be glued on before most terms
// (DB_PASSWORD, accessToken, PGPASSWORD, spring.datasource.password,
// --password), but nothing after it except the suffix, so `tokens`,
// `max_tokens`, `secretary`, `password_policy`, and `TOKEN_URL` are not
// credential names.
var credentialName = credentialNamePattern() + credentialSuffix

// credentialAssignment matches `name<sep>value` for a credential name, and
// credentialFlag a `--name value` (or `-name value`) command-line flag. Both
// capture the value as "value" so only it is replaced; the assignment also
// captures "name". Their assignment form is deliberately broad and is a
// known false-positive class: ordinary code such as `token = parse(x)`
// matches it. See dev/specs/privacy-filter.md.
var (
	credentialAssignment = regexp.MustCompile(`(?i)` + credentialLead + credentialQuote + `(?P<name>` + credentialName + `)` + credentialQuote + credentialSeparator + credentialScheme + `(?P<value>` + credentialValue + `)`)
	credentialFlag       = regexp.MustCompile(`(?im)(?:^|[ \t])-{1,2}` + credentialName + `[ \t]+(?P<value>` + credentialFlagValue + `)`)
	// credentialArgvFlag is a credential flag as one element of an argument
	// vector (`["gh", "auth", "login", "--token", "abc"]`).
	credentialArgvFlag = regexp.MustCompile(`(?i)^-{1,2}` + credentialName + `$`)
)

// credentialShapeTable lists the credentials recognizable by their own
// structure rather than by an assignment around them: a JWT (three
// base64url segments, the first beginning with `eyJ`) and the prefixed
// tokens of common services. Each needs enough characters after its prefix
// that a word or identifier which merely starts the same way is left alone.
// Each lists its needles: lower-case literals one of which every match
// holds (see linePattern); TestCredentialShapeNeedles checks each is in its
// pattern. PEM private keys are matched by redactPrivateKeyBlocks.
var credentialShapeTable = []struct {
	pattern string
	needles []string
}{
	{`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`, []string{"eyj"}},
	// GitHub.
	{`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`, []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}},
	{`\bgithub_pat_[A-Za-z0-9_]{20,}\b`, []string{"github_pat_"}},
	// Slack.
	{`\bxox[abposre]-[A-Za-z0-9-]{10,}`, []string{"xox"}},
	{`\bxapp-[0-9]+-[A-Za-z0-9-]{10,}`, []string{"xapp-"}},
	// AWS access key IDs, and temporary STS keys.
	{`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, []string{"akia", "asia"}},
	// Anthropic (`sk-ant-…`) and OpenAI (`sk-…`, `sk-proj-…`).
	{`\bsk-[A-Za-z0-9_-]{12,}`, []string{"sk-"}},
	// Stripe secret and restricted keys, and webhook signing secrets.
	{`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{10,}`, []string{"_live_", "_test_"}},
	{`\bwhsec_[A-Za-z0-9]{20,}`, []string{"whsec_"}},
	// GitLab personal, deploy, runner, trigger, and other tokens.
	{`\bgl(?:pat|dt|rt|ptt|cbt|soat|oas|imt|ft|agent|ffct)-[A-Za-z0-9_-]{20,}`, []string{"glpat-", "gldt-", "glrt-", "glptt-", "glcbt-", "glsoat-", "gloas-", "glimt-", "glft-", "glagent-", "glffct-"}},
	// Google API keys, OAuth client secrets, and OAuth access tokens.
	{`\bAIza[0-9A-Za-z_-]{30,}`, []string{"aiza"}},
	{`\bGOCSPX-[A-Za-z0-9_-]{20,}`, []string{"gocspx-"}},
	{`\bya29\.[0-9A-Za-z_-]{20,}`, []string{"ya29."}},
	// Hugging Face, npm, PyPI.
	{`\bhf_[A-Za-z0-9]{30,}`, []string{"hf_"}},
	{`\bapi_org_[A-Za-z0-9]{30,}`, []string{"api_org_"}},
	{`\bnpm_[A-Za-z0-9]{36,}`, []string{"npm_"}},
	{`\bpypi-AgE[A-Za-z0-9_-]{50,}`, []string{"pypi-age"}},
	// SendGrid, Shopify, DigitalOcean, HashiCorp Vault, Databricks, Linear.
	{`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`, []string{"sg."}},
	{`\bshp(?:at|ca|pa|ss)_[a-fA-F0-9]{32}`, []string{"shpat_", "shpca_", "shppa_", "shpss_"}},
	{`\bdo[por]_v1_[a-f0-9]{64}`, []string{"_v1_"}},
	{`\bhv[sbr]\.[A-Za-z0-9_-]{24,}`, []string{"hvs.", "hvb.", "hvr."}},
	{`\bdapi[a-f0-9]{32}`, []string{"dapi"}},
	{`\blin_(?:api|oauth)_[A-Za-z0-9]{32,}`, []string{"lin_api_", "lin_oauth_"}},
	// Grafana, Postman, New Relic, Sentry, Atlassian, Figma, Doppler.
	{`\bglsa_[A-Za-z0-9_]{32,}`, []string{"glsa_"}},
	{`\bglc_[A-Za-z0-9+/=_-]{32,}`, []string{"glc_"}},
	{`\bPMAK-[a-f0-9]{24}-[a-f0-9]{34}`, []string{"pmak-"}},
	{`\bNRAK-[A-Z0-9]{27}`, []string{"nrak-"}},
	{`\bsntr[ysu]_[A-Za-z0-9+/=_-]{30,}`, []string{"sntry_", "sntrs_", "sntru_"}},
	{`\bATATT3[A-Za-z0-9_=-]{30,}`, []string{"atatt3"}},
	{`\bfigd_[A-Za-z0-9_-]{30,}`, []string{"figd_"}},
	{`\bdp\.(?:pt|st|sa|ct|scim|audit)\.[A-Za-z0-9]{30,}`, []string{"dp."}},
	// age secret keys, Mailgun keys, Telegram bot tokens, Azure AD client
	// secrets.
	{`\b(?i:age-secret-key-1)[0-9A-Za-z]{40,}`, []string{"age-secret-key-1"}},
	{`\bkey-[0-9a-f]{32}\b`, []string{"key-"}},
	{`\b[0-9]{8,10}:AA[A-Za-z0-9_-]{33}\b`, []string{":aa"}},
	{`\b[A-Za-z0-9_~.-]{3}[78]Q~[A-Za-z0-9_~.-]{31,34}`, []string{"7q~", "8q~"}},
	// Groq (`gsk_` and 52 characters; 48 in some reports) and xAI (`xai-`
	// and 80). The prefix alone is too short to mean a key: xAI's model
	// names begin `xai-` too.
	{`\bgsk_[A-Za-z0-9]{48,}`, []string{"gsk_"}},
	{`\bxai-[A-Za-z0-9]{70,}`, []string{"xai-"}},
	// A PEM block encoded in base64 whole, as kubeconfig's client-key-data
	// holds a private key: every one begins with base64 of "-----BEGIN".
	// Certificates encoded the same way are redacted too.
	{`\bLS0tLS1CRUdJTi[A-Za-z0-9+/]{20,}={0,2}`, []string{"ls0tls1crudjti"}},
}

// The parts of a seed phrase (see credentialContextPatterns): a word of the
// BIP-39 list's lengths; what separates two words (spaces or a comma), and
// in an array a closing quote, a comma, and an opening quote; and what may
// follow the phrase: a closing quote or bracket, or the end of the line,
// possibly after a comment. A comma does not end a phrase, so a list of
// more words than a seed phrase has is not cut to one.
const (
	seedWord           = `[a-z]{3,8}`
	seedSeparator      = `(?:[ \t]*,[ \t]*|[ \t]+)`
	seedArraySeparator = `(?:[ \t]*,[ \t]*|[ \t]+|\\*["'][ \t]*,[ \t]*\\*["'])`
	seedEnd            = `(?:\\*["']|[ \t]*[;})\]]|[ \t]*(?:#[^\n]*)?\r?$)`
)

// credentialShape is credentialShapeTable as one pattern, gated by all of
// its needles.
var credentialShape = func() linePattern {
	patterns := make([]string, len(credentialShapeTable))
	var needles []string
	for i, shape := range credentialShapeTable {
		patterns[i] = shape.pattern
		needles = append(needles, shape.needles...)
	}
	return linePattern{re: regexp.MustCompile(strings.Join(patterns, "|")), needles: needles}
}()

// commandSecretFlag describes a command-line flag that carries a secret for
// particular programs only: `-p` is a password to mysql (glued, `-pS3cret`)
// and sshpass, but a port to ssh and a parent flag to mkdir. The one table
// drives both the text pattern for a command line (commandFlagPatterns) and
// the check of an argument vector (redactArgv).
type commandSecretFlag struct {
	// programs are the program names (a path before them is allowed).
	programs []string
	// subcommand, when set, must follow the program (`docker login`).
	subcommand string
	// flags are the flag spellings, case-sensitive (`-P` is not `-p`).
	flags []string
	// spaced: the value may follow as the next word (`-p value`, `--body value`).
	spaced bool
	// glued: the value may be glued to the flag (`-pvalue`).
	glued bool
	// userpass: the value is `user:secret`; only what follows the first
	// colon is the secret, and a value with no colon is only a user name.
	userpass bool
}

var commandSecretFlags = []commandSecretFlag{
	{programs: []string{"mysql", "mysqldump", "mysqladmin", "mysqlimport", "mysqlcheck", "mysqlsh", "mysqlpump", "mariadb", "mariadb-dump", "mariadb-admin"}, flags: []string{"-p"}, glued: true},
	{programs: []string{"sshpass"}, flags: []string{"-p"}, spaced: true, glued: true},
	{programs: []string{"docker", "podman", "nerdctl", "buildah", "skopeo", "helm", "oras", "crane", "finch"}, subcommand: "login", flags: []string{"-p"}, spaced: true, glued: true},
	{programs: []string{"az"}, subcommand: "login", flags: []string{"-p"}, spaced: true},
	{programs: []string{"redis-cli", "valkey-cli", "keydb-cli"}, flags: []string{"-a"}, spaced: true},
	{programs: []string{"mongo", "mongosh", "mongodump", "mongorestore", "mongoexport", "mongoimport", "mongostat", "mongotop", "mongofiles"}, flags: []string{"-p"}, spaced: true},
	{programs: []string{"sqlcmd", "bcp", "osql", "isql"}, flags: []string{"-P"}, spaced: true},
	{programs: []string{"ldapsearch", "ldapadd", "ldapmodify", "ldapdelete", "ldapwhoami", "ldappasswd", "ldapcompare", "ldapmodrdn"}, flags: []string{"-w"}, spaced: true},
	{programs: []string{"zip", "unzip", "zipcloak"}, flags: []string{"-P"}, spaced: true},
	{programs: []string{"7z", "7za", "7zr"}, flags: []string{"-p"}, glued: true},
	{programs: []string{"ssh-keygen"}, flags: []string{"-N", "-P"}, spaced: true},
	{programs: []string{"keytool", "jarsigner"}, flags: []string{"-storepass", "-keypass", "-srcstorepass", "-deststorepass", "-srckeypass", "-destkeypass"}, spaced: true},
	{programs: []string{"openssl"}, flags: []string{"-k"}, spaced: true},
	// macOS: `security unlock-keychain -p …`, `security add-generic-password -w …`.
	{programs: []string{"security"}, flags: []string{"-p", "-w"}, spaced: true},
	{programs: []string{"curl"}, flags: []string{"-u", "--user", "-U", "--proxy-user"}, spaced: true, glued: true, userpass: true},
	{programs: []string{"gh"}, subcommand: "secret", flags: []string{"-b", "--body"}, spaced: true},
}

// pattern is the flag's text pattern: the program, optionally its
// subcommand, then the flag and its value on the same command (no newline
// or `|`, `;`, `&` between).
func (c commandSecretFlag) pattern() string {
	quote := func(words []string) string {
		quoted := make([]string, len(words))
		for i, word := range words {
			quoted[i] = regexp.QuoteMeta(word)
		}
		return strings.Join(quoted, "|")
	}
	p := "(?:^|[\\s;&|(`'\"/])(?:" + quote(c.programs) + `)(?:\.exe)?\b`
	if c.subcommand != "" {
		p += `[^\n|;&]*?[ \t]` + regexp.QuoteMeta(c.subcommand) + `\b`
	}
	p += `[^\n|;&]*?[ \t](?:` + quote(c.flags) + `)`
	switch {
	case c.spaced && c.glued:
		p += `(?:[ \t]+|=)?`
	case c.spaced:
		p += `(?:[ \t]+|=)`
	}
	if c.userpass {
		// `user:secret`, or quoted, where the secret may hold spaces.
		return p + `(?:'[^'\n:]*:(?P<value>[^'\n]+)'|"[^"\n:]*:(?P<value>[^"\n]+)"|[^\s:'"]*:(?P<value>[^\s'"]+))`
	}
	return p + `(?P<value>` + credentialFlagValue + `)`
}

// isProgram reports whether one argument-vector element names one of the
// flag's programs, with or without a directory.
func (c commandSecretFlag) isProgram(arg string) bool {
	base := strings.TrimSuffix(arg[strings.LastIndexByte(arg, '/')+1:], ".exe")
	return slices.Contains(c.programs, base)
}

// credentialContextPatterns are the credential shapes recognized by the
// text around the value rather than by a name and separator. Each captures
// the secret as "value", so only it is replaced, and each lists the needles
// that gate the lines it is run on (see linePattern).
var credentialContextPatterns = func() []linePattern {
	patterns := make([]linePattern, 0, len(commandSecretFlags)+16)
	for _, flag := range commandSecretFlags {
		needles := make([]string, len(flag.programs))
		for i, program := range flag.programs {
			needles[i] = strings.ToLower(program)
		}
		patterns = append(patterns, linePattern{re: regexp.MustCompile(flag.pattern()), needles: needles})
	}
	for _, p := range []struct {
		re      string
		needles []string
	}{
		// .netrc: `machine host login user password secret`, on one line or
		// as a `password secret` line of its own.
		// A value starting with a separator belongs to an assignment
		// (`passwd : x`), which credentialAssignment reads.
		{`(?m)\b(?:machine|default)\b[^\n]*?[ \t](?:password|passwd)[ \t]+(?P<value>[^\s:=][^\s]*)`, []string{"pass"}},
		{`(?m)^[ \t]*(?:password|passwd)[ \t]+(?P<value>[^\s:=][^\s]*)[ \t]*\r?$`, []string{"pass"}},
		// openssl `-pass pass:secret`, `-passin pass:secret`.
		{`(?:^|[\s'"=])pass:(?P<value>[^\s'"]+)`, []string{"pass:"}},
		// `aws configure set aws_secret_access_key X`, `npm config set
		// //registry/:_authToken X`, `git config user.password X`.
		{`(?i)\b(?:config(?:ure)?[ \t]+set|git[ \t]+config)[ \t]+(?:-{1,2}[a-z][a-z-]*[ \t]+)*[^\s=]*?` + credentialName + `[ \t]+(?P<value>` + credentialFlagValue + `)`, vocabularyNeedles},
		// fish `set -gx TOKEN value`, csh and launchctl `setenv TOKEN value`.
		{`(?i)(?:^|[\s;(])(?:set[ \t]+(?:-[a-z]+[ \t]+)+|setenv[ \t]+)` + credentialName + `[ \t]+(?P<value>` + credentialFlagValue + `)`, vocabularyNeedles},
		// PowerShell `ConvertTo-SecureString "secret" -AsPlainText`.
		{`(?i)\bConvertTo-SecureString[ \t]+(?:-String[ \t]+)?(?P<value>` + credentialFlagValue + `)`, []string{"convertto-securestring"}},
		// XML: `<password>secret</password>`, and `<add key="ApiKey"
		// value="secret"/>` (also `name=`).
		{`(?i)<` + credentialName + `(?:[ \t][^<>\n]*)?>(?P<value>[^<\n]+)</`, vocabularyNeedles},
		{`(?i)<[a-z][^<>\n]*?\b(?:key|name)[ \t]*=[ \t]*["']` + credentialName + `["'][^<>\n]*?\bvalue[ \t]*=[ \t]*(?P<value>"[^"\n]*"|'[^'\n]*')`, vocabularyNeedles},
		// A name/value pair on one line whose name is a credential, in JSON
		// or Python outside a whole JSON string (a HAR header, a Kubernetes
		// env entry in a log line): `{"name": "Authorization", "value": …}`.
		{`(?i)["']?\b(?:name|key)["']?[ \t]*:[ \t]*["']` + credentialName + `["'][ \t]*,[ \t]*["']?value["']?[ \t]*:[ \t]*(?P<value>"(?:[^"\\\n]|\\.)*"|'[^'\n]*')`, vocabularyNeedles},
		// URL query parameters whose names are not credential words on
		// their own: `?key=`, `&sig=` (Azure SAS), `X-Amz-Signature=`.
		{`(?i)[?&](?:key|sig|signature|x-amz-signature|x-goog-signature)=(?P<value>[^&#\s"'<>\\]+)`, []string{"key=", "sig=", "signature="}},
		// Incoming-webhook URLs, whose path is the secret.
		{`(?i)\bhooks\.slack\.com/(?:services|workflows|triggers)/(?P<value>[A-Za-z0-9/_-]{8,})`, []string{"hooks.slack.com/"}},
		{`(?i)\bdiscord(?:app)?\.com/api/webhooks/(?P<value>[0-9]+/[A-Za-z0-9_-]{8,})`, []string{"discord"}},
		{`(?i)\.webhook\.office\.com/webhookb2/(?P<value>[A-Za-z0-9@/_.-]{8,})`, []string{".webhook.office.com/"}},
		// Docker's config.json registry login, base64 of `user:secret`, when
		// the file is shown as text rather than parsed as JSON: `"auth"` is
		// too common a word to redact everywhere (credentialVocabulary).
		{`(?i)\\*"(?:auth|identitytoken)\\*"[ \t]*:[ \t]*\\*"(?P<value>[A-Za-z0-9+/._-]{12,}={0,2})\\*"`, []string{`"auth`, "identitytoken"}},
		// A wallet's seed phrase after a name holding `mnemonic`
		// (`MNEMONIC="…"`, `mnemonic: …`, `--mnemonic "…"`): 12 to 24
		// words of three to eight letters, the BIP-39 word list's lengths,
		// separated by spaces or commas or written as an array (`["legal",
		// "winner", …]`), and nothing else before the end of the value but
		// a comment. `mnemonic` alone is not a credential name (an
		// assembler's `mnemonic = "mov"`), so the words must look like a
		// phrase.
		{`(?im)(?:^|[^a-z0-9])[a-z0-9_.-]*mnemonic(?:[_. -]?(?:phrase|words))?` + credentialQuote + `(?:` + credentialSeparator + `|[ \t]+)(?:\[[ \t]*)?(?:\\*["'])?(?P<value>` + seedWord + `(?:` + seedArraySeparator + seedWord + `){11,23})` + seedEnd, []string{"mnemonic"}},
		// The same after a name holding `seed` (`SEED=…`, `wallet seed:`),
		// a word too common to take on its own: only when the value is
		// exactly 12, 15, 18, 21, or 24 words, the lengths a seed phrase
		// has. `seed phrase` and `recovery phrase` are credential names
		// (credentialVocabulary), whatever their value.
		{`(?im)(?:^|[^a-z0-9])[a-z0-9_.-]*seed(?:[_. -]?words)?` + credentialQuote + credentialSeparator + `(?:\\*["'])?(?P<value>` + seedWord + `(?:` + seedSeparator + seedWord + `){11}(?:(?:` + seedSeparator + seedWord + `){3}(?:(?:` + seedSeparator + seedWord + `){3}(?:(?:` + seedSeparator + seedWord + `){3}(?:(?:` + seedSeparator + seedWord + `){3})?)?)?)?)` + seedEnd, []string{"seed"}},
		// A bearer token outside an Authorization header.
		{`(?:^|[^A-Za-z0-9_])(?i:bearer)[ \t]+(?P<value>[A-Za-z0-9._~+/-]{20,}=*)`, []string{"bearer"}},
	} {
		patterns = append(patterns, linePattern{re: regexp.MustCompile(p.re), needles: p.needles})
	}
	// A URL-encoded assignment, in a query string or form body nested in
	// another (`?next=%2Flogin%3Fpassword%3DS3cret`, `password%3AS3cret`):
	// the value runs to an encoded `&` or `,`, or what ends a URL. Only
	// lines holding a `%` are searched.
	patterns = append(patterns, linePattern{
		re:      regexp.MustCompile(`(?i)` + credentialLead + credentialName + `(?:%22)?(?:%3D|%3A)(?:%20|\+)*(?P<value>(?:[^&%\s"'<>#\\]|%(?:[013-9a-f][0-9a-f]|2[0-57-9abd-f]))+)`),
		needles: vocabularyNeedles,
		anyOf:   "%",
	})
	return patterns
}()

// redactArgv returns a copy of an argument vector with its secret values
// replaced (`["mysql", "-uroot", "-pS3cret"]`, `["curl", "-u", "me:S3cret",
// …]`, `["gh", "auth", "login", "--token", "abc"]`), and the value of a
// credential-named pair (`["X-Api-Key", "abc"]`), which the text patterns
// see only one element at a time, and reports whether anything was
// replaced. An array holding anything but strings is not an argument
// vector, and is returned as it is.
func redactArgv(items []any) ([]any, bool) {
	if len(items) < 2 {
		return items, false
	}
	argv := make([]string, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return items, false
		}
		argv[i] = s
	}
	// A [name, value] pair whose name is a credential: a header list
	// (`[["X-Api-Key", "…"]]`, Python requests and fetch both accept one).
	if len(argv) == 2 && argv[1] != "" && argv[1] != redactedMarker &&
		!strings.ContainsAny(argv[0], " \t\n") && isCredentialKey(argv[0]) {
		return []any{argv[0], redactedMarker}, true
	}
	changed := false
	set := func(i int, value string) {
		if argv[i] != value {
			argv[i], changed = value, true
		}
	}
	for _, entry := range commandSecretFlags {
		entry.redactArgv(argv, set)
	}
	for i := 0; i+1 < len(argv); i++ {
		if credentialArgvFlag.MatchString(argv[i]) && argv[i+1] != "" && argv[i+1][0] != '-' {
			set(i+1, redactedMarker)
		}
	}
	if !changed {
		return items, false
	}
	out := make([]any, len(argv))
	for i, arg := range argv {
		out[i] = arg
	}
	return out, true
}

// redactArgv replaces, through set, the value of the flag's first
// occurrence after its program in argv. Only the program's own flag is
// taken: what follows may be another program's arguments (`sshpass -p …
// ssh -p 22`).
func (c commandSecretFlag) redactArgv(argv []string, set func(int, string)) {
	program := slices.IndexFunc(argv, c.isProgram)
	if program < 0 || (c.subcommand != "" && !slices.Contains(argv[program+1:], c.subcommand)) {
		return
	}
	for i := program + 1; i < len(argv); i++ {
		arg := argv[i]
		for _, flag := range c.flags {
			switch {
			case arg == flag && c.spaced && i+1 < len(argv) && argv[i+1] != "" && argv[i+1][0] != '-':
				set(i+1, redactedArgValue(argv[i+1], c.userpass))
			case c.spaced && strings.HasPrefix(arg, flag+"="):
				set(i, flag+"="+redactedArgValue(arg[len(flag)+1:], c.userpass))
			case c.glued && len(arg) > len(flag) && strings.HasPrefix(arg, flag) && arg[len(flag)] != '-' && arg[len(flag)] != '=':
				set(i, flag+redactedArgValue(arg[len(flag):], c.userpass))
			default:
				continue
			}
			return
		}
	}
}

// redactedMarker replaces every redacted value.
const redactedMarker = "[REDACTED]"

func redactedArgValue(value string, userpass bool) string {
	if !userpass {
		return redactedMarker
	}
	if colon := strings.IndexByte(value, ':'); colon >= 0 {
		return value[:colon+1] + redactedMarker
	}
	return value
}

// pemBegin and pemEnd are the armor lines of a PEM or PGP private key.
var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	// pemHeader is an armor header line inside a PEM or PGP block
	// (`Proc-Type: 4,ENCRYPTED`, `Version: GnuPG v2`).
	pemHeader = regexp.MustCompile(`^(?:Proc-Type|DEK-Info|Version|Comment|Hash|Charset|MessageID):`)
)

// minPEMBodyBytes is how much base64 a private key block must hold, when
// its END line is missing, to be taken as a key. Source code that only
// names the BEGIN line (`const header = "-----BEGIN RSA PRIVATE KEY-----"`)
// holds none.
const minPEMBodyBytes = 16

// minPEMFallbackRun is the longest base64 run (a key line is 64 characters)
// that makes the text between a BEGIN and an END line a key even when its
// lines are decorated in a way pemLineCore does not know. Code between two
// constants that name the armor lines has no run this long.
const minPEMFallbackRun = 48

// maxPEMFallbackBytes bounds how far an END line may be from its BEGIN line
// for that fallback (an 8192-bit RSA key is about 6.5 KB).
const maxPEMFallbackBytes = 16 * 1024

// redactPrivateKeyBlocks replaces each PEM or PGP private key with
// [REDACTED]: from its BEGIN line through its END line when everything
// between is key body (base64, armor headers, real or JSON-escaped line
// breaks, each line possibly decorated as a display of a file decorates it:
// see pemLineCore), or, when the END line is missing (a key cut off by a
// truncated record), through the base64 lines that follow the BEGIN line.
// Filter 10 took everything after a BEGIN line with no END line, so source
// code that merely mentions the BEGIN line lost the rest of the file.
//
// A key split between two strings (a file read in two parts, a tool result
// cut into chunks) leaves the second string with body lines and an END line
// but no BEGIN line; those body lines are redacted too (see keyTailStart).
func redactPrivateKeyBlocks(s string) (string, bool) {
	var out strings.Builder
	pos, hit := 0, false
	for pos < len(s) {
		loc := pemBegin.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		begin, bodyStart := pos+loc[0], pos+loc[1]
		end := privateKeyBlockEnd(s, bodyStart)
		if end < 0 {
			out.WriteString(s[pos:bodyStart])
			pos = bodyStart
			continue
		}
		out.WriteString(s[pos:begin])
		out.WriteString(redactedMarker)
		pos, hit = end, true
	}
	if hit {
		out.WriteString(s[pos:])
		s = out.String()
	}
	tails, tailHit := redactKeyTails(s)
	return tails, hit || tailHit
}

// privateKeyBlockEnd returns the index just past a private key block whose
// body starts at from, or -1 when what follows the BEGIN line is not a key.
func privateKeyBlockEnd(s string, from int) int {
	if loc := pemEnd.FindStringIndex(s[from:]); loc != nil {
		body := s[from : from+loc[0]]
		// A body in a form pemLineCore does not know still holds the key's
		// long base64 lines: fail closed and take the block, unless another
		// block (a certificate) starts inside it, so a key cut short does
		// not take the certificate after it along to a later key's END.
		if isPEMBody(body) || (len(body) <= maxPEMFallbackBytes && longestBase64Run(body) >= minPEMFallbackRun && !strings.Contains(body, "-----BEGIN ")) {
			return from + loc[1]
		}
	}
	// No END line: the BEGIN line must end where it is (but for a closing
	// decoration), then take the body lines that follow, stopping at the
	// first line that is not one. A key written on one line, its body after
	// the BEGIN line (`"-----BEGIN … KEY----- MIIE…`), is taken to the end
	// of that line.
	lineEnd, next := pemLineEnd(s, from)
	end, body := -1, 0
	// Punctuation alone after it (`cat -A`'s `$`, a closing `")`) is a
	// decoration too; the body lines must then still be there.
	switch rest := strings.TrimSpace(pemLineSuffix.ReplaceAllString(s[from:lineEnd], "")); {
	case longestBase64Run(rest) >= minPEMFallbackRun:
		end, body = from+len(strings.TrimRight(pemLineSuffix.ReplaceAllString(s[from:lineEnd], ""), " \t")), longestBase64Run(rest)
	case strings.IndexFunc(rest, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0:
		return -1
	}
	for pos := next; pos >= 0 && pos < len(s); {
		lineEnd, next = pemLineEnd(s, pos)
		line := s[pos:lineEnd]
		coreStart, coreEnd := pemLineCore(line)
		core := line[coreStart:coreEnd]
		switch {
		case core == "" || pemHeader.MatchString(core):
		case isBase64Line(core):
			body += len(core)
			end = pos + coreEnd
		case longestBase64Run(core) >= minPEMFallbackRun:
			// A key line decorated in a way pemLineCore does not know
			// (`cat -A`'s `$`, `grep -rn`'s file name): fail closed.
			body += longestBase64Run(core)
			end = pos + coreEnd
		default:
			// A key cut off inside a line (`…base64"` at the end of a JSON
			// string) keeps its base64 prefix in the block, when a closing
			// quote or an escape ends it; a line of code does not.
			if prefix := base64Prefix(core); len(prefix) >= 4 && strings.IndexByte("\"'\\,", core[len(prefix)]) >= 0 {
				body += len(prefix)
				end = pos + coreStart + len(prefix)
			}
			next = -1
		}
		pos = next
	}
	if body < minPEMBodyBytes {
		return -1
	}
	return end
}

// redactKeyTails redacts, with its END line, the body of every private key
// whose END line has no BEGIN line before it in s: the base64 lines right
// above the END line (see keyTailStart). What remains after
// redactPrivateKeyBlocks has no key an END line closes, so an END line left
// is either such a tail or code that names the armor line.
func redactKeyTails(s string) (string, bool) {
	matches := pemEnd.FindAllStringIndex(s, -1)
	if matches == nil {
		return s, false
	}
	var out strings.Builder
	last, hit := 0, false
	for _, loc := range matches {
		start := keyTailStart(s, last, loc[0])
		if start < 0 {
			continue
		}
		out.WriteString(s[last:start])
		out.WriteString(redactedMarker)
		last, hit = loc[1], true
	}
	if !hit {
		return s, false
	}
	out.WriteString(s[last:])
	return out.String(), true
}

// keyTailStart returns where the body of a key whose END line starts at
// endAt begins, reading no further back than floor, or -1 when it is not
// the end of a key: the END line must start its line (but for a display's
// decoration, pemLinePrefix), and the lines right above it must hold at
// least minPEMBodyBytes of key body, read as privateKeyBlockEnd reads the
// lines after a BEGIN line. The first body line's decoration is kept.
func keyTailStart(s string, floor, endAt int) int {
	floor = max(floor, endAt-maxPEMFallbackBytes)
	var lines []valueSpan
	endLineStart := -1
	for pos := floor; pos >= 0 && pos <= endAt; {
		lineEnd, next := pemLineEnd(s, pos)
		if next < 0 || next > endAt {
			endLineStart = pos
			break
		}
		lines = append(lines, valueSpan{pos, lineEnd})
		pos = next
	}
	if endLineStart < 0 {
		return -1
	}
	if lead := s[endLineStart:endAt]; len(pemLinePrefix.FindString(lead)) != len(lead) {
		return -1
	}
	start, body := -1, 0
scan:
	for i := len(lines) - 1; i >= 0; i-- {
		line := s[lines[i].start:lines[i].end]
		coreStart, coreEnd := pemLineCore(line)
		core := line[coreStart:coreEnd]
		switch {
		case core == "" || pemHeader.MatchString(core):
		case isBase64Line(core):
			body += len(core)
			start = lines[i].start + coreStart
		case longestBase64Run(core) >= minPEMFallbackRun:
			body += longestBase64Run(core)
			start = lines[i].start + coreStart + firstBase64Run(core, minPEMFallbackRun)
		default:
			// The first line read may start inside a key line after other
			// text (`{"output": "…base64`): its base64 suffix is body.
			if suffix := base64Suffix(core); len(suffix) >= 4 && i == 0 {
				body += len(suffix)
				start = lines[i].start + coreEnd - len(suffix)
			}
			break scan
		}
	}
	if body < minPEMBodyBytes {
		return -1
	}
	return start
}

// firstBase64Run returns where the first run of at least n base64
// characters in s starts, or 0 when there is none.
func firstBase64Run(s string, n int) int {
	run := 0
	for i := range len(s) {
		if !isBase64Byte(s[i]) {
			run = 0
			continue
		}
		if run++; run >= n {
			return i + 1 - run
		}
	}
	return 0
}

// base64Suffix returns the longest suffix of line made of base64
// characters.
func base64Suffix(line string) string {
	for i := len(line) - 1; i >= 0; i-- {
		if !isBase64Byte(line[i]) {
			return line[i+1:]
		}
	}
	return line
}

// pemLinePrefix and pemLineSuffix match what a display of a file puts
// around each line of a key, so a key read through one is still recognized:
// a line number (`cat -n`'s `     2<TAB>`, the Claude Code Read tool's
// `     2→`, `grep -n`'s `2:`), a diff, quote, or comment marker (`+`, `-`,
// `>`, `#`, `//`, `*`, `;`), and in source code a string's quotes, an
// escaped line break, and what joins the strings (`"MIIE…\n" +`). Filter
// 11 as first written read only bare lines, so a key file read by Claude
// Code, whose Read tool numbers every line, was not redacted at all.
var (
	pemLinePrefix = regexp.MustCompile(`^[ \t]*(?:[0-9]+(?:→|\t|:|[ \t]+))?[ \t]*(?:(?:[-+>#*;]|//)[ \t]*)*["'` + "`" + `]?`)
	pemLineSuffix = regexp.MustCompile(`(?:\\r)?(?:\\n)?["'` + "`" + `]?[ \t]*[,;+)]*[ \t]*\\?\r?$`)
)

// pemLineCore returns where the content of one line of a key block starts
// and ends in line, without the decoration pemLinePrefix and pemLineSuffix
// match, and without surrounding whitespace.
func pemLineCore(line string) (start, end int) {
	start = len(pemLinePrefix.FindString(line))
	end = len(line)
	if loc := pemLineSuffix.FindStringIndex(line[start:]); loc != nil {
		end = start + loc[0]
	}
	for start < end && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	for end > start && (line[end-1] == ' ' || line[end-1] == '\t') {
		end--
	}
	return start, end
}

// longestBase64Run returns the length of the longest run of base64
// characters in s.
func longestBase64Run(s string) int {
	longest, run := 0, 0
	for i := range len(s) {
		if isBase64Byte(s[i]) {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// pemLineEnd returns the end of the line starting at from and where the
// next line starts (-1 at the end of s). A line ends at a real line break or
// a JSON-escaped one (`\n`, `\r\n`).
func pemLineEnd(s string, from int) (end, next int) {
	for i := from; i < len(s); i++ {
		switch {
		case s[i] == '\n':
			end = i
			if end > from && s[end-1] == '\r' {
				end--
			}
			return end, i + 1
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == 'n':
			end = i
			if end-2 >= from && s[end-2:end] == `\r` {
				end -= 2
			}
			return end, i + 2
		}
	}
	return len(s), -1
}

// isPEMBody reports whether the text between a BEGIN and an END line is a
// key's body: base64 lines, armor headers, and blank lines only, each
// possibly decorated (pemLineCore).
func isPEMBody(body string) bool {
	for pos := 0; pos <= len(body); {
		end, next := pemLineEnd(body, pos)
		line := strings.ReplaceAll(body[pos:end], `\t`, "")
		coreStart, coreEnd := pemLineCore(line)
		line = line[coreStart:coreEnd]
		if line != "" && !pemHeader.MatchString(line) && !isBase64Line(line) {
			return false
		}
		if next < 0 {
			break
		}
		pos = next
	}
	return true
}

func isBase64Line(line string) bool {
	return line != "" && len(base64Prefix(line)) == len(line)
}

// base64Prefix returns the longest prefix of line made of base64 characters.
func base64Prefix(line string) string {
	for i := range len(line) {
		if !isBase64Byte(line[i]) {
			return line[:i]
		}
	}
	return line
}

func isBase64Byte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '='
}

// urlScheme finds the start of a URL's authority (`postgres://`).
var urlScheme = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://`)

// redactURLUserinfo replaces the userinfo of every URL (`scheme://user:pass@host`,
// or a bare `scheme://user@host`) with [REDACTED], keeping the scheme and
// host so the reference stays legible. Filter 10 ended the userinfo at the
// first `@` and never crossed a `/`, so a password holding either
// (`postgres://u:p@ss@db`, `postgres://u:pa/ss@db`) kept the rest of itself.
// See userinfoScanner.end for how the end is found.
//
// Each URL is read to the end of its token (the next whitespace, quote, or
// angle bracket). URLs glued together into one long token (`http://x`
// repeated with nothing between) used to have that token read again for
// each of them, so the cost grew with the square of its length: 1.6 MB took
// a minute. The scanner now carries what it found forward along the string.
func redactURLUserinfo(s string) (string, bool) {
	if !strings.Contains(s, "://") {
		return s, false
	}
	matches := urlScheme.FindAllStringIndex(s, -1)
	if matches == nil {
		return s, false
	}
	var out strings.Builder
	last, hit := 0, false
	scan := userinfoScanner{s: s, tokenStop: -1, hostFrom: -1}
	for _, match := range matches {
		start := match[1]
		if start < last {
			continue
		}
		at := scan.end(start)
		if at <= start {
			continue
		}
		out.WriteString(s[last:start])
		out.WriteString(redactedMarker)
		last, hit = at, true
	}
	if !hit {
		return s, false
	}
	out.WriteString(s[last:])
	return out.String(), true
}

// urlEndChars are the characters that end a URL's token.
const urlEndChars = " \t\r\n\"'<>`"

// userinfoScanner finds the end of each URL's userinfo in s. Its queries
// come in increasing order of start (the URLs of one string, in order), so
// it keeps the next token stop and the next `@` a host name follows, and
// reads each part of s a bounded number of times.
type userinfoScanner struct {
	s string
	// tokenStop is the first token stop at or after the last start queried
	// (len(s) when there is none), or -1 before the first query.
	tokenStop int
	// hostFrom is where the last search for an `@` a host name follows
	// started, or -1 before the first search.
	hostFrom int
	// hostAt is the first such `@` at or after hostFrom (len(s) when there
	// is none).
	hostAt int
}

// end returns the index in s of the `@` that ends the userinfo of the URL
// whose authority starts at start, or -1 when it has none. The authority
// runs to the first `/`, `?`, or `#`, and an `@` inside it ends the
// userinfo at the last one, so a password may hold `@`. A password may also
// hold `/`, `?`, or `#`, which cut the authority short: when the authority
// is `user:…` with no `@`, and the part after the colon is not a port, the
// userinfo ends at the first later `@` in the token that a host name
// follows. So `https://medium.com/@user` (no colon) and
// `http://host:8080/?to=a@b` (a port) keep their text.
func (u *userinfoScanner) end(start int) int {
	if u.tokenStop < start {
		u.tokenStop = len(u.s)
		if i := strings.IndexAny(u.s[start:], urlEndChars); i >= 0 {
			u.tokenStop = start + i
		}
	}
	token := u.s[start:u.tokenStop]
	authorityEnd := strings.IndexAny(token, "/?#")
	if authorityEnd < 0 {
		authorityEnd = len(token)
	}
	authority := token[:authorityEnd]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		return start + at
	}
	colon := strings.IndexByte(authority, ':')
	if colon < 0 || isDigits(authority[colon+1:]) {
		return -1
	}
	if at := u.nextHostAt(start + authorityEnd); at < u.tokenStop {
		return at
	}
	return -1
}

// nextHostAt returns the first `@` at or after from that a host name
// follows (see hostStartsAt), or len(s). No such `@` lies between hostFrom
// and hostAt, so a query in that range is answered without reading s.
func (u *userinfoScanner) nextHostAt(from int) int {
	if u.hostFrom >= 0 && from >= u.hostFrom && from <= u.hostAt {
		return u.hostAt
	}
	u.hostFrom, u.hostAt = from, len(u.s)
	for i := from; i < len(u.s); i++ {
		next := strings.IndexByte(u.s[i:], '@')
		if next < 0 {
			break
		}
		i += next
		if hostStartsAt(u.s, i+1) {
			u.hostAt = i
			break
		}
	}
	return u.hostAt
}

// hostStartsAt reports whether s at i begins with a host name (or a
// bracketed IPv6 address) that ends the authority: at a port, path, query,
// fragment, or the end of the URL's token.
func hostStartsAt(s string, i int) bool {
	j := i
	for ; j < len(s); j++ {
		c := s[j]
		if strings.IndexByte(":/?#", c) >= 0 || strings.IndexByte(urlEndChars, c) >= 0 {
			break
		}
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && strings.IndexByte(".-[]_", c) < 0 {
			return false
		}
	}
	return j > i
}

func isDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// pgpassField is one field of a .pgpass line: anything but a colon, a
// quote, or whitespace, where `\:` and `\\` are a literal colon and
// backslash. pgpassHost is the host field, which is not all digits, so a
// timestamp (`14:05:33:…`) is not a line of the file.
const (
	pgpassField = `(?:\\[:\\]|[^\s:\\"'])+`                                                 //nolint:gosec // G101: regex fragment for a .pgpass field, not a credential
	pgpassHost  = `(?:\\[:\\]|[^\s:\\"'])*(?:\\[:\\]|[^\s:\\"'0-9])(?:\\[:\\]|[^\s:\\"'])*` //nolint:gosec // G101: regex fragment for a .pgpass field, not a credential
)

// pgpassLinePattern matches a line of a PostgreSQL password file,
// `host:port:database:user:password`, whose port matches port, as a
// display shows it: after a line number (`     3→`, `3:`), the file's name
// as grep prints it (`/home/me/.pgpass.bak:3:`), a diff or quote marker,
// or `echo`. In quotes the password runs to the closing quote; otherwise
// to the end of the line, whatever it holds (quotes, spaces, a trailing
// backslash).
func pgpassLinePattern(port string) *regexp.Regexp {
	fields := pgpassHost + `:` + port + `:` + pgpassField + `:` + pgpassField + `:`
	return regexp.MustCompile(`(?m)^[ \t]*(?:[0-9]+(?:→|\t|:)[ \t]*)?(?:[^\s:]*(?i:pgpass)[^\s:]*:(?:[0-9]+:)?)?(?:(?:echo|printf)[ \t]+(?:-[a-z]+[ \t]+)*)?(?:[+>][ \t]*)?` +
		`(?:\\*"` + fields + `(?P<value>(?:[^"\\\n]|\\[^"\\\n])+)\\*"[^\n]*` +
		`|'` + fields + `(?P<value>[^'\n]+)'[^\n]*` +
		`|` + fields + `(?P<value>[^\s](?:[^\r\n]*[^\s])?)[ \t]*\r?)$`)
}

// pgpassNamedLine is a .pgpass line with any port of two or more digits,
// taken only in a string that names the file (redactPgpassLines);
// pgpassPortLine is one whose port is PostgreSQL's or PgBouncer's usual
// one (5432, 6432) or `*`, taken anywhere: that is how the Claude Code Read
// tool shows the file, its path in the call rather than the result.
var (
	pgpassNamedLine = pgpassLinePattern(`(?:[0-9]{2,5}|\*)`)
	pgpassPortLine  = linePattern{re: pgpassLinePattern(`(?:5432|6432|\*)`), needles: []string{"5432", "6432", ":*:"}, anyOf: ":"}
)

// redactPgpassLines redacts the password of every .pgpass line: with a
// usual port anywhere, and with any port in a string that mentions
// `.pgpass` or `PGPASSFILE` (`cat ~/.pgpass`, a heredoc into it, grep's
// `.pgpass.bak:` prefix). Five colon-separated fields with another port
// and no such mention are too common a shape (a compiler diagnostic, a log
// line) to redact on their own.
func redactPgpassLines(t needleText) (string, bool) {
	pattern := pgpassPortLine
	if strings.Contains(t.lower, ".pgpass") || strings.Contains(t.lower, "pgpassfile") {
		// No needle gates the lines: the name is elsewhere in the string.
		pattern = linePattern{re: pgpassNamedLine, anyOf: ":"}
	}
	return redactMatches(pattern, t, true)
}

// lineNumberPrefix matches the line number a display of a file puts before
// each line: `cat -n`'s `     2<TAB>` and the Claude Code Read tool's
// `     2→`. Agents see most files through one, so the line-based rules
// below read a line after it.
var lineNumberPrefix = regexp.MustCompile(`^[ \t]*[0-9]+(?:→|\t)`)

// displayedLine splits one line of s, starting at pos, into its line-number
// prefix (when numbered is true and the line has one) and the rest. ok is
// false when numbered is true but the line has no prefix: it is not a line
// of the same display.
func displayedLine(line string, numbered bool) (prefix int, ok bool) {
	if !numbered {
		return 0, true
	}
	loc := lineNumberPrefix.FindStringIndex(line)
	if loc == nil {
		return 0, false
	}
	return loc[1], true
}

// yamlCredentialKey matches a YAML key line whose value is not on the line:
// a block scalar (`password: |`, `token: >-`) or a value on the lines below
// (`password:` then an indented `S3cret`), possibly after a line number
// (lineNumberPrefix). The "indent" group is the line's indentation and any
// sequence dash, which set the column the value must be indented past.
var yamlCredentialKey = regexp.MustCompile(`(?im)^(?P<number>[ \t]*[0-9]+(?:→|\t))?(?P<indent>[ \t]*(?:-[ \t]+)?)` + credentialQuote + credentialName + credentialQuote + `[ \t]*:[ \t]*(?P<indicator>[|>][0-9+-]*)?[ \t]*(?:#[^\n]*)?\r?$`)

// yamlStructure matches the start of a line that is YAML structure rather
// than a scalar: a mapping entry, a sequence item, a comment, a flow
// collection, an anchor or alias, or a complex key.
var yamlStructure = regexp.MustCompile(`^(?:-(?:[ \t]|$)|#|\{|\[|&|\*|\?[ \t]|(?:"[^"]*"|'[^']*'|[^\s#'"][^#]*?)[ \t]*:(?:[ \t]|$))`)

// redactYAMLBlockValues replaces the value of a credential key written over
// the following lines with one [REDACTED] line at the value's indentation:
// a block scalar (`password: |` and its indented lines) whole, or an
// indented plain or quoted scalar on the next lines. Filter 10 read only
// the key's own line, so it kept both (and replaced a `|` indicator with
// the marker). A key whose indented lines are a mapping or a sequence
// (`secrets:` then `db: …`, `passwords:` then `- …`) is a structure: every
// scalar value in it is redacted and its keys, comments, and layout are
// kept (see yamlStructureSpans), as redactCredentialStructures does for
// JSON. In a file shown with line numbers, each line is read after its
// number, and the numbers of the value's lines go with it.
func redactYAMLBlockValues(t needleText) (string, bool) {
	s := t.s
	matches := lineMatches(linePattern{yamlCredentialKey, vocabularyNeedles, ":"}, t)
	if matches == nil {
		return s, false
	}
	indicatorGroup := yamlCredentialKey.SubexpIndex("indicator")
	numberGroup := yamlCredentialKey.SubexpIndex("number")
	indentGroup := yamlCredentialKey.SubexpIndex("indent")
	var out strings.Builder
	last, hit := 0, false
	for _, match := range matches {
		lineEnd := match[1]
		if match[0] < last || lineEnd >= len(s) || s[lineEnd] != '\n' {
			continue
		}
		numbered := match[2*numberGroup] >= 0
		threshold := match[2*indentGroup+1] - match[2*indentGroup]
		blockIndicator := match[2*indicatorGroup] >= 0
		start, end, indent, structure := lineEnd+1, -1, "", false
		for pos := start; pos < len(s); {
			next := strings.IndexByte(s[pos:], '\n')
			lineStop := len(s)
			if next >= 0 {
				lineStop = pos + next
			}
			line := strings.TrimSuffix(s[pos:lineStop], "\r")
			prefix, sameDisplay := displayedLine(line, numbered)
			if !sameDisplay {
				break
			}
			if content := line[prefix:]; strings.TrimSpace(content) != "" {
				trimmed := strings.TrimLeft(content, " \t")
				width := len(content) - len(trimmed)
				if end < 0 && !blockIndicator && (width > threshold || width == threshold && isYAMLSequenceItem(trimmed)) && yamlStructure.MatchString(trimmed) {
					structure = true
					break
				}
				if width <= threshold {
					break
				}
				if end < 0 {
					indent = line[:prefix+width]
				}
				end = pos + len(line)
			}
			if next < 0 {
				break
			}
			pos = lineStop + 1
		}
		if structure {
			spans, regionEnd := yamlStructureSpans(s, start, numbered, threshold)
			if len(spans) == 0 {
				continue
			}
			out.WriteString(s[last:spans[0].start])
			for i, span := range spans {
				if i > 0 {
					out.WriteString(s[spans[i-1].end:span.start])
				}
				writeRedacted(&out, s[span.start:span.end])
			}
			out.WriteString(s[spans[len(spans)-1].end:regionEnd])
			last, hit = regionEnd, true
			continue
		}
		if end < 0 {
			continue
		}
		out.WriteString(s[last:start])
		out.WriteString(indent + redactedMarker)
		last, hit = end, true
	}
	if !hit {
		return s, false
	}
	out.WriteString(s[last:])
	return out.String(), true
}

// isYAMLSequenceItem reports whether a line's content, after its
// indentation, is a sequence item.
func isYAMLSequenceItem(trimmed string) bool {
	return trimmed == "-" || strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "-\t")
}

// yamlStructureSpans returns the value of every scalar in the YAML mapping
// or sequence under a credential key, starting at pos, and where the
// structure ends: the lines indented past the key's column threshold (or,
// for a sequence, at it), in the same display. A mapping entry's value
// (`db: S3cret`) and a sequence item (`- S3cret`) are values, and so is
// every line of a block scalar or a multi-line plain scalar; keys,
// comments, and the values of descriptive keys (`type:`, `name:`, see
// descriptiveStructureKeys) are kept.
func yamlStructureSpans(s string, pos int, numbered bool, threshold int) ([]valueSpan, int) {
	var spans []valueSpan
	regionEnd, first, sequenceAtKey := pos, true, false
	for pos < len(s) {
		next := strings.IndexByte(s[pos:], '\n')
		lineStop := len(s)
		if next >= 0 {
			lineStop = pos + next
		}
		line := strings.TrimSuffix(s[pos:lineStop], "\r")
		prefix, sameDisplay := displayedLine(line, numbered)
		if !sameDisplay {
			break
		}
		content := line[prefix:]
		trimmed := strings.TrimLeft(content, " \t")
		width := len(content) - len(trimmed)
		if strings.TrimSpace(content) != "" {
			if first {
				sequenceAtKey, first = width == threshold && isYAMLSequenceItem(trimmed), false
			}
			if width < threshold || (width == threshold && (!sequenceAtKey || !isYAMLSequenceItem(trimmed))) {
				break
			}
			if at, length := yamlLineValue(trimmed); length > 0 {
				start := pos + prefix + width + at
				spans = append(spans, valueSpan{start, start + length})
			}
			regionEnd = pos + len(line)
		}
		if next < 0 {
			break
		}
		pos = lineStop + 1
	}
	return spans, regionEnd
}

// yamlMappingKey matches a mapping entry's key and separator at the start
// of a line's content.
var yamlMappingKey = regexp.MustCompile(`^(?:"(?P<dq>[^"]*)"|'(?P<sq>[^']*)'|(?P<plain>[^\s#'"][^#]*?))[ \t]*:(?:[ \t]+|$)`)

// yamlLineValue returns where the value in one line of a YAML structure
// starts in trimmed (the line's content after its indentation) and its
// length, or length 0 when the line holds none to redact.
func yamlLineValue(trimmed string) (at, length int) {
	for isYAMLSequenceItem(trimmed[at:]) {
		at++
		for at < len(trimmed) && (trimmed[at] == ' ' || trimmed[at] == '\t') {
			at++
		}
	}
	rest := trimmed[at:]
	if loc := yamlMappingKey.FindStringSubmatchIndex(rest); loc != nil {
		key := ""
		for _, group := range []string{"dq", "sq", "plain"} {
			if i := yamlMappingKey.SubexpIndex(group); loc[2*i] >= 0 {
				key = strings.ToLower(strings.TrimSpace(rest[loc[2*i]:loc[2*i+1]]))
			}
		}
		if descriptiveStructureKeys[key] {
			return 0, 0
		}
		at += loc[1]
		rest = rest[loc[1]:]
	}
	// A comment after the value is kept; a quoted value may hold `#`.
	if quoted := quotedPrefix.FindString(rest); quoted != "" {
		rest = quoted
	} else if comment := strings.Index(rest, " #"); comment >= 0 {
		rest = rest[:comment]
	}
	rest = strings.TrimRight(rest, " \t")
	if rest == "" || rest[0] == '#' || isNonSecretValue(rest) || strings.Trim(rest, `"'`) == redactedMarker {
		return 0, 0
	}
	return at, len(rest)
}

// credentialNameEntry matches a YAML entry whose name is a credential and
// whose value is on a sibling line: `- name: DB_PASSWORD` then `  value: …`,
// a Kubernetes or Compose environment entry, a Helm values list, or a HAR
// header written as YAML (also `key:`), possibly after a line number. It is
// the text form of the structured rule that drops a value labelled as a
// credential (hasSensitiveLabel). The "indent" group sets the column of the
// entry's other keys.
var credentialNameEntry = regexp.MustCompile(`(?im)^(?P<number>[ \t]*[0-9]+(?:→|\t))?(?P<indent>[ \t]*(?:-[ \t]+)?)(?:name|key)[ \t]*:[ \t]*["']?` + credentialName + `["']?[ \t]*\r?$`)

// entryValueKey matches the `value:` key of such an entry and what follows
// it on the line.
var entryValueKey = regexp.MustCompile(`^value[ \t]*:[ \t]*`)

// maxEntryLines bounds how many lines after a credentialNameEntry are
// searched for its `value:` key.
const maxEntryLines = 8

// redactCredentialEntryValues redacts the `value:` of every YAML entry whose
// `name:` is a credential (credentialNameEntry): the rest of its line, or a
// quoted value's contents. Filter 11 as first written redacted such a value
// only in structured data, so a Kubernetes manifest an agent read kept every
// literal environment secret.
func redactCredentialEntryValues(t needleText) (string, bool) {
	s := t.s
	matches := lineMatches(linePattern{credentialNameEntry, vocabularyNeedles, ":"}, t)
	if matches == nil {
		return s, false
	}
	numberGroup := credentialNameEntry.SubexpIndex("number")
	indentGroup := credentialNameEntry.SubexpIndex("indent")
	var spans []valueSpan
	for _, match := range matches {
		lineEnd := match[1]
		if lineEnd >= len(s) || s[lineEnd] != '\n' {
			continue
		}
		numbered := match[2*numberGroup] >= 0
		column := match[2*indentGroup+1] - match[2*indentGroup]
		// Two name lines of one entry (`name: A` then `name: B`, then
		// `value: …`) find the same value; it is redacted once.
		if span, ok := entryValueSpan(s, lineEnd+1, numbered, column); ok && (len(spans) == 0 || span.start >= spans[len(spans)-1].end) {
			spans = append(spans, span)
		}
	}
	if len(spans) == 0 {
		return s, false
	}
	return redactSpans(s, spans), true
}

// entryValueSpan finds the value of the `value:` key at column among the
// lines from pos on, before the entry ends (a line indented less, or a new
// sequence item at the column).
func entryValueSpan(s string, pos int, numbered bool, column int) (valueSpan, bool) {
	for range maxEntryLines {
		if pos >= len(s) {
			break
		}
		lineStop := len(s)
		if next := strings.IndexByte(s[pos:], '\n'); next >= 0 {
			lineStop = pos + next
		}
		line := strings.TrimSuffix(s[pos:lineStop], "\r")
		prefix, sameDisplay := displayedLine(line, numbered)
		if !sameDisplay {
			break
		}
		content := line[prefix:]
		trimmed := strings.TrimLeft(content, " \t")
		width := len(content) - len(trimmed)
		switch {
		case trimmed == "":
		case width < column || (width == column && strings.HasPrefix(trimmed, "-")):
			return valueSpan{}, false
		case width == column:
			if loc := entryValueKey.FindStringIndex(trimmed); loc != nil {
				start := pos + prefix + width + loc[1]
				end := pos + len(line)
				value := s[start:end]
				if quoted := quotedPrefix.FindString(value); quoted != "" {
					end = start + len(quoted)
				} else {
					end = start + len(strings.TrimRight(value, " \t"))
				}
				if end <= start || isNonSecretValue(s[start:end]) {
					return valueSpan{}, false
				}
				return valueSpan{start, end}, true
			}
		}
		pos = lineStop + 1
	}
	return valueSpan{}, false
}

// credentialStructureStart matches a credential name whose value is an
// object or array: `"secret": {"value": "…"}`, `"passwords": ["…"]`,
// `'credentials': {…}`, `apiKey: {value: "…"}`, possibly in JSON escaped
// inside a string. The match ends just past the opening bracket; the
// "quote" group is the name's opening quote with the backslashes that
// escape it.
var credentialStructureStart = regexp.MustCompile(`(?i)` + credentialLead + `(?P<quote>\\*["'])?` + credentialName + credentialQuote + `[ \t]*(?::|=>|=|：)[ \t]*[{\[]`)

// maxStructureBytes bounds how far a credential structure is read.
const maxStructureBytes = 64 * 1024

// redactCredentialStructures redacts every string value inside an object or
// array whose name is a credential (credentialStructureStart), keeping its
// keys, brackets, numbers, and layout: `"secret": {"value": "[REDACTED]"}`.
// Structured data drops such a value whole (isCredentialKey); in text, where
// the structure is part of a larger file (a JSON file shown with line
// numbers, a log line, an edit), filter 11 as first written kept it, since a
// bracket holding a quote is not a value (credentialBracketedValue).
func redactCredentialStructures(t needleText) (string, bool) {
	s := t.s
	matches := lineMatches(linePattern{credentialStructureStart, vocabularyNeedles, "{["}, t)
	if matches == nil {
		return s, false
	}
	quoteGroup := credentialStructureStart.SubexpIndex("quote")
	var spans []valueSpan
	last := 0
	for _, match := range matches {
		if match[0] < last {
			continue
		}
		escapes := 0
		if match[2*quoteGroup] >= 0 {
			escapes = match[2*quoteGroup+1] - match[2*quoteGroup] - 1
		}
		found, end := structureStringValues(s, match[1]-1, escapes)
		spans = append(spans, found...)
		last = end
	}
	if len(spans) == 0 {
		return s, false
	}
	var out strings.Builder
	prev := 0
	for _, span := range spans {
		out.WriteString(s[prev:span.start])
		out.WriteString(redactedMarker)
		prev = span.end
	}
	out.WriteString(s[prev:])
	return out.String(), true
}

// structureStringValues reads the object or array whose opening bracket is
// at open and returns the contents of its string values (strings not
// followed by `:`, which are keys) that are not already [REDACTED] and do
// not belong to a descriptiveStructureKeys key, and
// where the structure ends. escapes is how many backslashes escape each
// quote (0 for plain JSON, 1 for JSON inside a JSON string). A string left
// open takes the rest of its line.
func structureStringValues(s string, open, escapes int) ([]valueSpan, int) {
	var spans []valueSpan
	limit := min(len(s), open+maxStructureBytes)
	depth, lastKey := 0, ""
	for i := open; i < limit; {
		switch c := s[i]; c {
		case '{', '[':
			depth++
			i++
			if c == '{' {
				lastKey = ""
			}
		case '}', ']':
			depth--
			i++
			if c == '}' {
				lastKey = ""
			}
			if depth <= 0 {
				return spans, i
			}
		default:
			quote, width := structureQuoteAt(s, i, escapes)
			if width == 0 {
				i++
				continue
			}
			start := i + width
			end, next := structureStringEnd(s, start, limit, quote, escapes)
			if end < 0 {
				lineEnd := strings.IndexAny(s[start:limit], "\r\n")
				if lineEnd < 0 {
					lineEnd = limit - start
				}
				if content := s[start : start+lineEnd]; content != "" && content != redactedMarker {
					spans = append(spans, valueSpan{start, start + lineEnd})
				}
				return spans, start + lineEnd
			}
			key := false
			for j := next; j < limit; j++ {
				if s[j] != ' ' && s[j] != '\t' {
					key = s[j] == ':'
					break
				}
			}
			switch content := s[start:end]; {
			case key:
				lastKey = strings.ToLower(content)
			case content != "" && content != redactedMarker && !descriptiveStructureKeys[lastKey]:
				spans = append(spans, valueSpan{start, end})
			}
			i = next
		}
	}
	return spans, limit
}

// descriptiveStructureKeys are keys inside a credential structure whose
// value describes the credential rather than being it (`"credentials":
// {"type": "service_account", …}`); their string values are kept.
var descriptiveStructureKeys = map[string]bool{
	"type": true, "kind": true, "name": true, "description": true, "provider": true,
	"scheme": true, "alg": true, "algorithm": true, "format": true, "encoding": true,
}

// structureQuoteAt returns the quote character and the width of the quote
// (with its escaping backslashes) that opens a string at i, or width 0.
// With no escapes, either quote opens a string; with escapes, only exactly
// that many backslashes before a double quote do.
func structureQuoteAt(s string, i, escapes int) (byte, int) {
	if escapes == 0 {
		if s[i] == '"' || s[i] == '\'' {
			return s[i], 1
		}
		return 0, 0
	}
	if s[i] != '\\' || (i > 0 && s[i-1] == '\\') {
		return 0, 0
	}
	run := 0
	for i+run < len(s) && s[i+run] == '\\' {
		run++
	}
	if run == escapes && i+run < len(s) && s[i+run] == '"' {
		return '"', run + 1
	}
	return 0, 0
}

// structureStringEnd returns where the contents of a string opened by quote
// end and where the text after its closing quote starts, or -1 when the
// string does not close before limit or the end of its line.
func structureStringEnd(s string, start, limit int, quote byte, escapes int) (end, next int) {
	for j := start; j < limit; {
		c := s[j]
		switch {
		case c == '\n' || c == '\r':
			return -1, -1
		case escapes == 0 && c == '\\':
			j += 2
		case escapes == 0 && c == quote:
			return j, j + 1
		case escapes > 0 && c == '\\':
			run := 0
			for j+run < limit && s[j+run] == '\\' {
				run++
			}
			if run == escapes && j+run < limit && s[j+run] == '"' {
				return j, j + run + 1
			}
			j += run + 1
		default:
			j++
		}
	}
	return -1, -1
}
