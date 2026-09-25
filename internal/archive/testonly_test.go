package archive

import "errors"

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

// IsFilterError supports callers which need to retain a previous source bundle
// when a new native format cannot be safely filtered.
func IsFilterError(err error) bool {
	var target *FilterError
	return errors.As(err, &target)
}
