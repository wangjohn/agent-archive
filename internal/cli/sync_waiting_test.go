package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

// New evidence inside the upload interval is saved and held, and sync says
// so, with when it is due, rather than calling the session unchanged.
func TestSyncReportsPublicationsWaitingForTheUploadInterval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, _, path, _ := publishedThroughSync(t, now)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(30 * time.Second) }
	var out, errOut strings.Builder
	if code := runSyncCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatalf("sync exit=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	due := waitingSummary([]string{"x"}, now.Add(3*time.Minute))
	want := "Scanned 1 session(s): 0 published, " + due + "0 unchanged, 0 failed.\n"
	if out.String() != want {
		t.Fatalf("sync output:\n%q\nwant\n%q", out.String(), want)
	}
}

func TestWaitingSummary(t *testing.T) {
	t.Parallel()
	next := time.Date(2026, 9, 24, 21, 27, 33, 0, time.UTC)
	if got := waitingSummary(nil, next); got != "" {
		t.Errorf("no waiting publications: %q", got)
	}
	want := "5 waiting for the upload interval (next at " + next.Local().Format("15:04") + "), "
	if got := waitingSummary(make([]string, 5), next); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := waitingSummary(make([]string, 2), time.Time{}); got != "2 waiting for the upload interval, " {
		t.Errorf("unknown due time: %q", got)
	}
}
