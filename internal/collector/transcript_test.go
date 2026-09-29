package collector

import (
	"errors"
	"fmt"
	"testing"
)

// Each of the collector's size-limit errors matches ErrSizeLimit, wrapped or
// not, and they still tell each other apart.
func TestSizeLimitErrorsMatchErrSizeLimit(t *testing.T) {
	t.Parallel()
	for _, err := range []error{errTranscriptTooLarge, errRecordTooLarge} {
		if !errors.Is(fmt.Errorf("filter: %w", err), ErrSizeLimit) {
			t.Errorf("%v does not match ErrSizeLimit", err)
		}
	}
	if errors.Is(errTranscriptTooLarge, errRecordTooLarge) || errors.Is(errRecordTooLarge, errTranscriptTooLarge) {
		t.Error("the two size limits match each other")
	}
	if errors.Is(errors.New(errTranscriptTooLarge.Error()), ErrSizeLimit) {
		t.Error("an error with the same text matches ErrSizeLimit")
	}
}
