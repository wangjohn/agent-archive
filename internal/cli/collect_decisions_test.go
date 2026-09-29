package cli

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/collector"
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
