package archive

// SanitizeObject applies the shared privacy policy to a native codec's object.
// Codecs still own format recognition and must not retain an unrecognized record.
func SanitizeObject(in map[string]any, state *PrivacyState) (map[string]any, bool) {
	return sanitizeObject(in, state)
}

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

// These operations expose mandatory privacy decisions to native envelope
// shapers. None changes or replaces the redaction policy.
func PrivacyHiddenObject(value map[string]any) bool           { return isHiddenObject(value) }
func PrivacyBinaryObject(value map[string]any) (string, bool) { return binaryContentBlock(value) }
func PrivacyBlockedKey(key string) bool                       { return blockedKeys[key] }
func PrivacySensitiveLabel(value map[string]any) bool         { return hasSensitiveLabel(value) }
func PrivacyDeniedArgument(key, tool string, labelled bool) bool {
	return deniedToolArgument(key, tool, labelled)
}
func PrivacyRedactArgv(value []any) ([]any, bool) { return redactArgv(value) }
