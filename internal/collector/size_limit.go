package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// filteredLimitError is errTranscriptTooLarge for what the filter kept.
type filteredLimitError struct {
	size  int64
	limit int64
}

func (e *filteredLimitError) Error() string {
	return fmt.Sprintf("%v of %d bytes after filtering (%d bytes)", errTranscriptTooLarge, e.limit, e.size)
}

func (e *filteredLimitError) Unwrap() error { return errTranscriptTooLarge }

// errSessionReadCeiling is a read the session's own share of the pass's
// ledger (SessionReadCeiling) refused: waiting does not change it.
var errSessionReadCeiling = errors.New("session needs more memory to read than one session may hold in a pass")

// overSessionCeiling reports a read refused at the session's own share of
// the pass's ledger (see SessionReadCeiling), with nothing else, such as a
// change or a cancellation, that a later read could clear.
func (s *sessionScan) overSessionCeiling(err error) bool {
	return errors.Is(err, agentapi.ErrReadBudget) && s.readBudget().ScopeExceeded() &&
		!agentapi.HasFailure(err, agentapi.Cleanup) && !agentapi.HasFailure(err, agentapi.Changed) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// overCeiling ends a scan whose work other than the source read itself
// (refreshing the last publication, building, compressing or reconciling
// what was read, saving it) needed more than the session's share of the
// pass: the same size-limit gap a read over it records, at the state the
// source was read at, or is in now when the scan never read it. A scan that
// left a publication pending keeps its error instead, for the next pass to
// finish.
func (s *sessionScan) overCeiling(err error) (sessionOutcome, error) {
	if !s.overSessionCeiling(err) {
		return outcomeSkipped, err
	}
	if owed, owedErr := s.local.Outstanding(s.reg, false); owedErr != nil || owed.Upload {
		return outcomeSkipped, err
	}
	s.releaseRetained()
	var read sourceRead
	if s.lastRead != nil {
		read = *s.lastRead
	} else {
		reader, available := newSourceReader(s.reg, s.opts)
		adapter, adapterErr := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
		if !available || adapterErr != nil {
			return outcomeSkipped, err
		}
		observed, signatureErr := reader.Signature(s.ctx)
		if signatureErr != nil {
			return outcomeSkipped, errors.Join(err, signatureErr)
		}
		read = sourceRead{adapter: adapter, observed: observed}
	}
	s.sizeLimit = sizeLimitOf(errSessionReadCeiling, read.observed, s.reg.Harness.Name, s.opts.maxTranscriptBytes())
	return s.blockThenRemember(state.BlockedReasonTranscriptTooLarge, read)
}

// sizeLimitOf names the limit a size-limit failure stopped at, or nil when
// err carries no sizes (observed supplies the source's size then).
func sizeLimitOf(err error, observed sourceState, harness string, maxBytes int64) *state.SizeLimitGap {
	var raw *agentapi.RawLimitError
	var filtered *filteredLimitError
	switch {
	case errors.As(err, &raw):
		return &state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: raw.Limit, SizeBytes: raw.Size}
	case errors.As(err, &filtered):
		return &state.SizeLimitGap{Kind: state.SizeLimitFiltered, LimitBytes: filtered.limit, SizeBytes: filtered.size}
	case errors.Is(err, errSessionReadCeiling):
		return &state.SizeLimitGap{Kind: state.SizeLimitReadBudget, LimitBytes: sessionReadCeiling, SizeBytes: observed.size()}
	case errors.Is(err, errRecordTooLarge), errors.Is(err, archive.ErrRecordTooLarge):
		return &state.SizeLimitGap{Kind: state.SizeLimitRecord, LimitBytes: recordLimit, SizeBytes: observed.size()}
	case errors.Is(err, errTranscriptTooLarge):
		return &state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: maxRawBytes(harness, maxBytes), SizeBytes: observed.size()}
	}
	return nil
}

// SizeLimitLabel words a size-limit gap's limit, as status and the pass's
// problem name it: "Codex raw 512 MiB".
func SizeLimitLabel(harness string, limit state.SizeLimitGap) string {
	mib := fmt.Sprintf("%d MiB", limit.LimitBytes>>20)
	switch limit.Kind {
	case state.SizeLimitRaw:
		if harness == archive.HarnessCodex {
			return "Codex raw " + mib
		}
		return "raw " + mib
	case state.SizeLimitFiltered:
		return "filtered " + mib
	case state.SizeLimitRecord:
		return "record " + mib
	case state.SizeLimitReadBudget:
		return "per-session read memory " + mib
	}
	return mib
}

// sizeLimitProblemPrefix begins every SizeLimitProblem.
const sizeLimitProblemPrefix = " session(s) stopped being captured at a size limit"

// SizeLimitProblem is the problem a pass records in its Status when n
// sessions sit in a size-limit gap: not a failure, but capture of them has
// stopped at their last snapshot. limits are the limits they stopped at
// (SizeLimitLabel), named once each.
func SizeLimitProblem(n int, limits ...string) string {
	named := slices.Compact(slices.Sorted(slices.Values(limits)))
	text := fmt.Sprintf("%d%s", n, sizeLimitProblemPrefix)
	if len(named) > 0 {
		text += " (" + strings.Join(named, ", ") + ")"
	}
	return text + ", kept at their last snapshot; agent-archive status APP lists them"
}

// IsSizeLimitProblem reports whether problem is a SizeLimitProblem, for any
// count and limits.
func IsSizeLimitProblem(problem string) bool {
	var n int
	if _, err := fmt.Sscanf(problem, "%d ", &n); err != nil || n < 1 {
		return false
	}
	rest, ok := strings.CutPrefix(problem, fmt.Sprintf("%d%s", n, sizeLimitProblemPrefix))
	if !ok {
		return false
	}
	const suffix = ", kept at their last snapshot; agent-archive status APP lists them"
	if rest == suffix {
		return true
	}
	inner, ok := strings.CutSuffix(rest, ")"+suffix)
	return ok && strings.HasPrefix(inner, " (") && !strings.ContainsAny(inner[2:], "()")
}
