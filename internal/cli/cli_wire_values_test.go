package cli

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/capture"
)

// TestWireValueSpellings pins values that status --json and the stored
// capture diagnostics write. The typed constants are the only place these
// spellings live, so a renamed constant value would otherwise change the
// output format without failing any test.
func TestWireValueSpellings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got  string
		want string
	}{
		{string(capabilityDocumented), "documented"},
		{string(capabilityFixtureValidated), "fixture_validated"},
		{string(capabilityUnavailable), "unavailable"},
		{string(capabilityUnknown), "unknown"},
		{string(capture.DiagnosticUnknownSessionStart), "session_start_unknown"},
		{string(capture.DiagnosticPreActivationStart), "session_started_before_activation"},
		{string(capture.DiagnosticSetupInProgress), "setup_in_progress"},
		{string(storageAccessConfirmedBySetup), "setup"},
		{string(storageAccessConfirmedByCollector), "collector"},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("value = %q, want %q", c.got, c.want)
		}
	}
}
