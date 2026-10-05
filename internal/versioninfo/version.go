// Package versioninfo holds pure version interpretation shared by status and inspection.
package versioninfo

import (
	"regexp"
	"strconv"
	"strings"
)

// DirectoryPattern matches a directory named exactly for a version, such as
// "2.1.280"; unlike versionPattern it does not find one inside other text,
// so "backup-2.1.300" is not a version directory.
var DirectoryPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)+(?:[-+][0-9A-Za-z.-]+)?$`)

// Compare orders two versions by their numeric components,
// returning -1, 0, or 1. A missing component counts as zero, and any
// pre-release or build suffix is ignored.
func Compare(a, b string) int {
	parts := func(value string) []string {
		value = Normalize(value)
		if i := strings.IndexAny(value, "-+"); i >= 0 {
			value = value[:i]
		}
		return strings.Split(value, ".")
	}
	left, right := parts(a), parts(b)
	for i := 0; i < len(left) || i < len(right); i++ {
		var l, r int
		if i < len(left) {
			l, _ = strconv.Atoi(left[i])
		}
		if i < len(right) {
			r, _ = strconv.Atoi(right[i])
		}
		if l != r {
			if l < r {
				return -1
			}
			return 1
		}
	}
	return 0
}

// versionPattern finds a dotted numeric version with an optional pre-release
// or build suffix anywhere in a tool's `--version` answer, such as
// "codex-cli 1.2.3", "v1.2.3", or "1.2.3.4".
var versionPattern = regexp.MustCompile(`(?:^|[^0-9])([0-9]+(?:\.[0-9]+)+(?:[-+][0-9A-Za-z.-]+)?)(?:$|[^0-9A-Za-z.+-])`)

// Normalize extracts the version number from a version answer. When
// no dotted numeric version is present, the trimmed answer itself is the
// version so opaque schemes still compare by exact text.
func Normalize(value string) string {
	value = strings.TrimSpace(value)
	match := versionPattern.FindStringSubmatch(value)
	if len(match) == 2 {
		return match[1]
	}
	return value
}

// Shape classifies a version as a dotted numeric string or an opaque
// label. Two versions of different shapes come from different numbering
// schemes and cannot be compared.
func Shape(value string) string {
	if versionPattern.MatchString(value) {
		return "dotted"
	}
	return "opaque"
}
