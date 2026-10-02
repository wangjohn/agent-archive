package archive

// SanitizeValue applies the same privacy policy to scalar or structured tool content.
func SanitizeValue(in any, state *PrivacyState) (any, bool) { return sanitizeValue(in, state) }

// PrivacyOmit records a field omitted by the common privacy policy.
func (s *PrivacyState) PrivacyOmit(key string) { s.omitField(key) }

// PrivacyDeny records a tool argument denied by the common privacy policy.
func (s *PrivacyState) PrivacyDeny(key string) { s.denyArgument(key) }

// RedactSensitive applies the shared credential and instruction redaction policy.
func RedactSensitive(value string) (string, bool) { return redactSensitive(value) }

// JSONHasDuplicateKeys refuses ambiguous structured content before sanitizing it.
func JSONHasDuplicateKeys(value string) bool { return jsonHasDuplicateKeys(value) }

// MaxSanitizeStringPasses bounds repeated redaction of encoded strings.
const MaxSanitizeStringPasses = maxSanitizeStringPasses

// PrivacyHiddenObject recognizes objects containing hidden instruction content.
func PrivacyHiddenObject(value map[string]any) bool { return isHiddenObject(value) }

// PrivacyBinaryObject recognizes binary content that shared privacy policy omits.
func PrivacyBinaryObject(value map[string]any) (string, bool) { return binaryContentBlock(value) }

// PrivacyBlockedKey reports keys forbidden by shared privacy policy.
func PrivacyBlockedKey(key string) bool { return blockedKeys[key] }

// PrivacySensitiveLabel detects labels describing sensitive typed input.
func PrivacySensitiveLabel(value map[string]any) bool { return hasSensitiveLabel(value) }

// PrivacyDeniedArgument applies shared sensitive tool-argument rules.
func PrivacyDeniedArgument(key, tool string, labelled bool) bool {
	return deniedToolArgument(key, tool, labelled)
}

// PrivacyRedactArgv redacts credential arguments using the shared argv policy.
func PrivacyRedactArgv(value []any) ([]any, bool) { return redactArgv(value) }
