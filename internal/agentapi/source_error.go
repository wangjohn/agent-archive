package agentapi

import (
	"context"
	"errors"
)

// FailureKind controls retry and fallback without inspecting provider error text.
type FailureKind string

// Failure kinds distinguish source absence, retryable reads, refusals and cleanup.
const (
	Missing        FailureKind = "missing"
	Unavailable    FailureKind = "unavailable"
	Changed        FailureKind = "changed"
	FormatMismatch FailureKind = "format_mismatch"
	Unsafe         FailureKind = "unsafe"
	Limit          FailureKind = "limit"
	Cleanup        FailureKind = "cleanup"
)

// SourceError retains a local cause and a bounded public classification.
type SourceError struct {
	Kind     FailureKind
	Err      error
	Observed *SourceObservation
}

func (e *SourceError) Error() string {
	if e.Kind == Unsafe || e.Kind == FormatMismatch {
		return "unsafe source format"
	}
	return "source " + string(e.Kind)
}
func (e *SourceError) Unwrap() error { return e.Err }

// Failure reports the outer classified failure, including joined cleanup errors.
func Failure(err error) FailureKind {
	var e *SourceError
	if errors.As(err, &e) {
		return e.Kind
	}
	return Unavailable
}

// Wrap classifies a native cause without losing errors.Is/As behavior.
func Wrap(k FailureKind, err error) error {
	if err == nil {
		return nil
	}
	return &SourceError{Kind: k, Err: err}
}

// ErrClosed rejects use of a borrowed input after its owner closes.
var ErrClosed = errors.New("source owner is closed")

// ErrRawLimit reports an aggregate source value limit.
var ErrRawLimit = errors.New("source exceeds raw size limit")

// Deterministic permits caching only stable filter/limit failures without cleanup or retryable causes.
func Deterministic(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return deterministicTree(err)
}
func deterministicTree(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !deterministicTree(e) {
				return false
			}
		}
		return true
	}
	var e *SourceError
	if errors.As(err, &e) && e.Kind != Unsafe && e.Kind != FormatMismatch && e.Kind != Limit {
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return deterministicTree(wrapped.Unwrap())
	}
	return true
}

// ErrorObservation is the exact verified observation attached to a deterministic read failure.
func ErrorObservation(err error) (SourceObservation, bool) {
	var e *SourceError
	if errors.As(err, &e) && e.Observed != nil {
		return *e.Observed, true
	}
	return SourceObservation{}, false
}

// HasFailure inspects every joined failure, including secondary cleanup errors.
func HasFailure(err error, kind FailureKind) bool {
	if err == nil {
		return false
	}
	var e *SourceError
	if errors.As(err, &e) && e.Kind == kind {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if HasFailure(e, kind) {
				return true
			}
		}
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return HasFailure(single.Unwrap(), kind)
	}
	return false
}
