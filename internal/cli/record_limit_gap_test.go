package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
)

// A session blocked by one oversize record shows as a capture gap with its
// own explanation, and never as an error. (The collector's record limit
// cannot be lowered from this package; the collector's own tests drive the
// block and show the pass records no error, which is what sync's exit code
// is made of. This checks the status wiring.)
func TestStatusReportsTheRecordSizeLimitAsAGap(t *testing.T) {
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	reg, _ := onlyRegistration(t, home)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, publishedAt, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("setup: %v", err)
	}
	if err := store.SaveBlocked(reg.ArchiveSessionID, bundle, publishedAt, state.BlockedReasonRecordTooLarge); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Collector.LastError != "" {
		t.Fatalf("a record-size gap surfaced as an error: %q", view.Collector.LastError)
	}
	gaps := view.Apps[0].CaptureGaps
	if len(gaps) != 1 || gaps[0].Code != "record_size_limit" || !strings.Contains(gaps[0].Detail, "64 MiB record size limit") || !strings.Contains(gaps[0].Detail, "resumes when the transcript changes") {
		t.Fatalf("gaps = %+v", gaps)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "Last error") || !strings.Contains(out.String(), "1 with a capture gap") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
}
