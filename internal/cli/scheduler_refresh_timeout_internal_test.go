package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// A launchctl that hangs while refresh restarts the job ends on its own:
// refresh absorbs Ctrl-C and SIGTERM while it does, so the 30 s bound on
// bootstrap and bootout (launchctlChangeTimeout, shortened here) is what
// ends it. A bootout that times out fails the transaction; its rollback asks
// the job's state, finds it still loaded, tries to stop it, and times out
// again, so the refresh ends with its journal left, every file as it was
// (the job is stopped before the files change), and the way out in the
// message. The journal is the ordinary one, with the job recorded loaded.
func TestRefreshWithAHungBootoutEndsAndLeavesItsJournal(t *testing.T) {
	previous := launchctlChangeTimeout
	launchctlChangeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { launchctlChangeTimeout = previous })
	r := newRefreshInstall(t, false, true, modeLoaded)
	r.launchd.hang = map[string]bool{"bootout": true}
	before := tree(t, r.home, r.userHome)

	done := make(chan struct{})
	var code int
	var stdout, stderr string
	go func() {
		defer close(done)
		code, stdout, stderr = r.run(t)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("refresh hung: nothing bounds launchctl bootout")
	}

	if code != 1 || stdout != "" {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"agent-archive: setup --refresh: stop previous collector: launchctl bootout: context deadline exceeded",
		"rollback incomplete; run setup again: cannot recover the interrupted setup: launchctl could not stop the background collector",
		"The interrupted setup is recorded in " + setupjournal.JournalPath(r.home),
		"agent-archive setup --abandon-recovery",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if want := []string{r.print(), r.print(), r.bootout(), r.print(), r.print(), r.bootout()}; !slices.Equal(r.launchd.argv(), want) {
		t.Errorf("launchctl calls\n%q\nwant\n%q", r.launchd.argv(), want)
	}
	// Only the upper bound: a loaded runner can pass the 50 ms before the stand-in
	// reads the clock.
	if left := r.launchd.remaining["bootout"]; left == noDeadline || left > launchctlChangeTimeout {
		t.Errorf("bootout ran with %v to its deadline, want a deadline at most %v away", left, launchctlChangeTimeout)
	}
	if !setupjournal.TransactionPending(r.home) {
		t.Fatal("the journal is gone")
	}
	journal := readText(t, setupjournal.JournalPath(r.home))
	if strings.Contains(journal, "files_only") || !strings.Contains(journal, `"was_loaded": true`) {
		t.Errorf("journal:\n%s", journal)
	}
	scratch := []string{"setup.lock", "hooks.lock", "collector.lock", collectorLockRecordName, "setup-transaction.json"}
	for _, path := range differences(before, tree(t, r.home, r.userHome)) {
		if !slices.ContainsFunc(scratch, func(name string) bool { return strings.HasSuffix(path, "/"+name) }) {
			t.Errorf("a refresh whose launchctl hung changed %s", path)
		}
	}
}
