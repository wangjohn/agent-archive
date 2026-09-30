package capture

import (
	"testing"
	"time"
)

func TestLockWaitShrinksByWhatTheLookupUsed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		spent time.Duration
		want  time.Duration
	}{
		{0, hooksLockWait},
		{-time.Millisecond, hooksLockWait},
		{200 * time.Millisecond, 550 * time.Millisecond},
		{repoKeyBudget, hooksLockWait - repoKeyBudget},
		{5 * time.Second, minHooksLockWait},
	} {
		if got := lockWaitAfter(tc.spent); got != tc.want {
			t.Errorf("lockWaitAfter(%v) = %v, want %v", tc.spent, got, tc.want)
		}
	}
	// A lookup that uses its whole budget still leaves the lock wait within
	// the allowance the hook had before it asked for a key at all.
	if repoKeyBudget+lockWaitAfter(repoKeyBudget) > hooksLockWait+minHooksLockWait {
		t.Error("the lookup budget and the lock wait together exceed the hook's allowance")
	}
}
