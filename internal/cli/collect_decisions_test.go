package cli

import (
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestCollectorPassHolder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		quiet bool
		pass  passOptions
		want  string
	}{
		{false, passOptions{}, "sync"},
		{true, passOptions{}, "scheduled collection"},
		{true, passOptions{progress: func(collector.Progress) {}}, "backfill upload"},
	} {
		if got := collectorPassHolder(tc.quiet, tc.pass); got != tc.want {
			t.Errorf("holder = %q, want %q", got, tc.want)
		}
	}
}

func TestSessionIssueCodeKeepsRawErrorsOutOfStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New("secret output"), "capture_or_publication_failed"},
		{state.ErrQuarantined, "local_state_unreadable"},
		{errors.New("transcript truncated, compacted, or rewritten"), "transcript_discontinuity"},
		{errors.New("collection limit exceeded"), "transcript_size_limit"},
	} {
		if got := sessionIssueCode(tc.err); got != tc.want {
			t.Errorf("issue %q: code = %q, want %q", tc.err, got, tc.want)
		}
	}
}
