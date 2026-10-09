package discovery

import (
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestFirstTaskRejectionDoesNotOverrideRetryableReadFailures(t *testing.T) {
	t.Parallel()
	if nativeProofOutcome(errOwnTaskRejected) != "own_task_rejected" {
		t.Fatal("decisive first task not classified")
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, agentapi.Wrap(agentapi.Changed, errors.New("changed")), agentapi.Wrap(agentapi.Cleanup, errors.New("cleanup")), agentapi.Wrap(agentapi.Limit, errors.New("budget")), agentapi.Wrap(agentapi.Unsafe, errors.New("source unsafe")), agentapi.Wrap(agentapi.FormatMismatch, errors.New("source format changed"))} {
		if nativeProofOutcome(errors.Join(errOwnTaskRejected, err)) == "own_task_rejected" {
			t.Fatal("retryable read failure became terminal", err)
		}
	}
}
