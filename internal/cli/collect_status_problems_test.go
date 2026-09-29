package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
)

// sizeLimitProblem stands for a problem collector.Run recorded that is not a
// count of failed sessions.
const sizeLimitProblem = "1 session(s) stopped being captured: over the transcript size limit, kept at their last snapshot"

// savedProblems saves a Status holding problems, as collector.Run leaves it,
// and returns the store.
func savedProblems(t *testing.T, home string, problems ...string) *state.Store {
	t.Helper()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	var status state.Status
	status.SetLastErrors(problems...)
	if err := store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	return store
}

func lastErrors(t *testing.T, store *state.Store) []string {
	t.Helper()
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	return status.LastErrors
}

// A read-back failure and a retention hold after collector.Run are more
// problems of the same pass: the ones collector.Run recorded stay.
func TestPassProblemsAfterCollectionAreAdded(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, _, bucket := publishedThroughSync(t, now)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store := savedProblems(t, home, sizeLimitProblem)
	// A clock three months ahead of storage's holds retention's deletions.
	env.Now = func() time.Time { return now.Add(91 * 24 * time.Hour) }
	verifyErr := errors.New("read-back verification: not found")
	if _, err := finishPassWithRetention(home, env, cfg, store, bucket, time.Time{}, collector.Result{}, verifyErr); !errors.Is(err, verifyErr) {
		t.Fatalf("pass error = %v, want the verification failure", err)
	}
	got := lastErrors(t, store)
	if len(got) != 3 || got[0] != sizeLimitProblem || !strings.Contains(got[1], "clock is ahead") || got[2] != verifyErr.Error() {
		t.Fatalf("LastErrors = %q, want the size-limit gap, the retention hold and the verification failure", got)
	}
}

// The summary of sessions still needing capture after the pass takes the
// place of collector.Run's count of failed sessions; its other problems stay.
func TestSessionsNeedingCaptureReplaceOnlyTheFailedCount(t *testing.T) {
	t.Parallel()
	store := savedProblems(t, t.TempDir(), collector.FailedSessionsProblem(2), sizeLimitProblem)
	failed := map[string]error{"a": errors.New("publish failed"), "b": errors.New("publish failed")}
	if err := recordSessionIssues(store, failed, func(string) bool { return false }, collector.FailedSessionsProblem(2)); err != nil {
		t.Fatal(err)
	}
	want := []string{issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 2}}), sizeLimitProblem}
	if got := lastErrors(t, store); !slices.Equal(got, want) {
		t.Fatalf("LastErrors = %q want %q", got, want)
	}
}

// Retention's per-session failures update the pass's summary of failed
// sessions, which covers the collector's too, and leave its other problems.
func TestRetentionFailuresUpdateTheFailedCount(t *testing.T) {
	t.Parallel()
	collected := issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 2}})
	for name, tc := range map[string]struct {
		collectorFailures int
		recorded          []string
		want              []string
	}{
		"after failed sessions": {
			collectorFailures: 2,
			recorded:          []string{collected, sizeLimitProblem},
			want:              []string{issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 2}, issueRetentionFailed: {sessions: 1}}), sizeLimitProblem},
		},
		"with none failed before": {
			recorded: []string{sizeLimitProblem},
			want:     []string{sizeLimitProblem, issueSummary(map[string]issueTally{issueRetentionFailed: {sessions: 1}})},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := savedProblems(t, t.TempDir(), tc.recorded...)
			result := collector.Result{Errors: map[string]error{}}
			for i := range tc.collectorFailures {
				result.Errors["collected-"+string(rune('a'+i))] = errors.New("publish failed")
			}
			recordRetentionErrors(store, &result, retention.Result{Errors: map[string]error{"expired": errors.New("delete failed")}})
			if got := lastErrors(t, store); !slices.Equal(got, tc.want) {
				t.Fatalf("LastErrors = %q want %q", got, tc.want)
			}
		})
	}
}

// A failure before collector.Run replaces the problems recorded, which are
// the previous pass's.
func TestFailureBeforeCollectionReplacesThePreviousPassProblems(t *testing.T) {
	t.Parallel()
	store := savedProblems(t, t.TempDir(), collector.FailedSessionsProblem(1), sizeLimitProblem)
	recordPreflightError(store, errors.New("open storage: no credentials"))
	if got, want := lastErrors(t, store), []string{"open storage: no credentials"}; !slices.Equal(got, want) {
		t.Fatalf("LastErrors = %q want %q", got, want)
	}
}
