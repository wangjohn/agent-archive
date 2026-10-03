package capture

import (
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

var (
	startCommit = strings.Repeat("3f", 20)
	laterCommit = strings.Repeat("9e", 20)
)

// headLookup stands in for the git lookup: it answers sha for every
// directory, dirty when asked, and records each question.
type headLookup struct {
	mu    sync.Mutex
	sha   string
	dirty *bool
	asked []headQuestion
}

type headQuestion struct {
	dir       string
	withDirty bool
}

func (l *headLookup) lookup(dir string, withDirty bool) (string, *bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, headQuestion{dir, withDirty})
	if !withDirty {
		return l.sha, nil
	}
	return l.sha, l.dirty
}

func (l *headLookup) questions() []headQuestion {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]headQuestion(nil), l.asked...)
}

func onlyRegistration(t *testing.T, home string) archive.SessionRegistration {
	t.Helper()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v, err = %v; want one", regs, err)
	}
	return regs[0]
}

func stopPayload(cwd string) map[string]any {
	return map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "cwd": cwd}
}

// A new session records the commit its working directory had checked out
// and whether the tree was dirty, asked of the directory the hook reports:
// here a linked worktree inside the project, whose HEAD is its own.
func TestHookRecordsTheStartingCommitOfTheSessionsWorkingDirectory(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	git := &headLookup{sha: startCommit, dirty: new(true)}
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	worktree := "/work/widget/.claude/worktrees/fix-login"
	if err := HandleEvent(home, "claude", startPayload(worktree), at, WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
		t.Fatal(err)
	}
	reg := onlyRegistration(t, home)
	if reg.StartHead == nil || reg.StartHead.SHA != startCommit || reg.StartHead.Dirty == nil || !*reg.StartHead.Dirty || !reg.StartHead.ObservedAt.Equal(at) {
		t.Errorf("StartHead = %+v, want %s, dirty, at %v", reg.StartHead, startCommit, at)
	}
	if reg.LastHead != nil {
		t.Errorf("LastHead = %+v before any stop", reg.LastHead)
	}
	if got := git.questions(); len(got) != 1 || got[0] != (headQuestion{worktree, true}) {
		t.Errorf("git was asked %v, want once, with dirty, in %s", got, worktree)
	}
}

// Cursor's desktop app registers a chat at its first prompt, with the
// workspace root as its directory.
func TestHookRecordsTheStartingCommitOfACursorChatAtItsFirstPrompt(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	git := &headLookup{sha: startCommit, dirty: new(false)}
	payload := map[string]any{
		"hook_event_name": "beforeSubmitPrompt", "conversation_id": "chat-1", "session_id": "chat-1",
		"workspace_roots": []any{"/work/widget"}, "transcript_path": nil,
	}
	if err := HandleEvent(home, "cursor", payload, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
		t.Fatal(err)
	}
	if reg := onlyRegistration(t, home); reg.StartHead == nil || reg.StartHead.SHA != startCommit || reg.StartHead.Dirty == nil || *reg.StartHead.Dirty {
		t.Errorf("StartHead = %+v, want %s, clean", reg.StartHead, startCommit)
	}
}

// Whatever the lookup does wrong, the session still registers, without a
// starting commit rather than with a made-up one.
func TestHookRegistersWithoutAStartingCommitWhenGitCannotTell(t *testing.T) {
	t.Parallel()
	for name, lookup := range map[string]GitHeadFunc{
		"no lookup":             nil,
		"not a repository":      func(string, bool) (string, *bool) { return "", nil },
		"a dirty tree, no HEAD": func(string, bool) (string, *bool) { return "", new(true) },
		"a lookup that panics":  func(string, bool) (string, *bool) { panic("git blew up") },
		"an abbreviation":       func(string, bool) (string, *bool) { return "3f9c2ab", new(false) },
		"a branch name":         func(string, bool) (string, *bool) { return "main", new(false) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			if err := HandleEvent(home, "codex", startPayload("/work/widget"), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), WithDecoders(testDecoders), WithGitHead(lookup)); err != nil {
				t.Fatal(err)
			}
			if reg := onlyRegistration(t, home); reg.StartHead != nil {
				t.Errorf("StartHead = %+v, want none", reg.StartHead)
			}
		})
	}
}

// A dirty flag git could not work out in time is unknown, not clean.
func TestHookRecordsTheStartingCommitWithoutADirtyFlagGitCouldNotGive(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	git := &headLookup{sha: startCommit}
	if err := HandleEvent(home, "codex", startPayload("/work/widget"), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
		t.Fatal(err)
	}
	if reg := onlyRegistration(t, home); reg.StartHead == nil || reg.StartHead.SHA != startCommit || reg.StartHead.Dirty != nil {
		t.Errorf("StartHead = %+v, want %s with no dirty flag", reg.StartHead, startCommit)
	}
}

func TestHookDoesNotAskForACommitForAStartItDeclinesOrAlreadyRegistered(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	git := &headLookup{sha: startCommit}
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	outside := startPayload("/elsewhere/other")
	resumed := startPayload("/work/widget")
	resumed["source"] = "resume"
	for _, payload := range []map[string]any{outside, resumed} {
		if err := HandleEvent(home, "codex", payload, at, WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
			t.Fatal(err)
		}
	}
	if got := git.questions(); len(got) != 0 {
		t.Fatalf("git was asked %v for starts that were declined", got)
	}
	for range 2 {
		if err := HandleEvent(home, "codex", startPayload("/work/widget"), at, WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
			t.Fatal(err)
		}
	}
	if got := git.questions(); len(got) != 1 {
		t.Errorf("git was asked %v, want once for the one new session", got)
	}
}

// Each stop asks for HEAD, without the dirty flag, and records it when it
// moved: the last commit seen, from the first stop that saw it.
func TestHookRecordsTheLastCommitAStopSees(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	git := &headLookup{sha: startCommit, dirty: new(false)}
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if err := HandleEvent(home, "codex", startPayload("/work/widget"), start, WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
		t.Fatal(err)
	}
	stop := func(at time.Time) archive.SessionRegistration {
		t.Helper()
		if err := HandleEvent(home, "codex", stopPayload("/work/widget/src"), at, WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
			t.Fatal(err)
		}
		return onlyRegistration(t, home)
	}
	first := start.Add(time.Minute)
	if reg := stop(first); reg.LastHead == nil || reg.LastHead.SHA != startCommit || !reg.LastHead.ObservedAt.Equal(first) || reg.LastHead.Dirty != nil {
		t.Fatalf("LastHead after the first stop = %+v", reg.LastHead)
	}
	if reg := stop(start.Add(2 * time.Minute)); !reg.LastHead.ObservedAt.Equal(first) {
		t.Errorf("a stop at the same commit rewrote LastHead: %+v", reg.LastHead)
	}
	git.mu.Lock()
	git.sha = laterCommit
	git.mu.Unlock()
	moved := start.Add(3 * time.Minute)
	reg := stop(moved)
	if reg.LastHead == nil || reg.LastHead.SHA != laterCommit || !reg.LastHead.ObservedAt.Equal(moved) {
		t.Errorf("LastHead after HEAD moved = %+v", reg.LastHead)
	}
	if reg.StartHead == nil || reg.StartHead.SHA != startCommit {
		t.Errorf("StartHead changed after stops: %+v", reg.StartHead)
	}
	for _, q := range git.questions()[1:] {
		if q != (headQuestion{"/work/widget/src", false}) {
			t.Errorf("a stop asked %+v, want HEAD alone in the reported directory", q)
		}
	}
	// A stop that cannot read HEAD keeps the last commit recorded.
	git.mu.Lock()
	git.sha = ""
	git.mu.Unlock()
	if reg := stop(start.Add(4 * time.Minute)); reg.LastHead == nil || reg.LastHead.SHA != laterCommit {
		t.Errorf("a failed lookup dropped LastHead: %+v", reg.LastHead)
	}
}

func TestHookDoesNotAskForACommitAtTheStopOfASessionItDoesNotArchive(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	git := &headLookup{sha: startCommit}
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, payload := range []map[string]any{stopPayload("/work/widget"), stopPayload("/elsewhere/other")} {
		if err := HandleEvent(home, "codex", payload, at, WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
			t.Fatal(err)
		}
	}
	if got := git.questions(); len(got) != 0 {
		t.Errorf("git was asked %v at stops of sessions that are not registered", got)
	}
}

// The commit is asked before hooks.lock is taken, like the repository key,
// and at the same time as it: each lookup here waits for the other to start,
// so asking one after the other would lose both to the budget.
func TestHookAsksForTheCommitAndTheRepoKeyAtOnceBeforeTakingTheLock(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	headStarted, keyStarted := make(chan struct{}), make(chan struct{})
	waitFor := func(started chan struct{}) bool {
		select {
		case <-started:
			return true
		case <-time.After(repoKeyBudget / 2):
			return false
		}
	}
	lockFree := func() {
		unlock, err := local.NamedLockWait(home, "hooks.lock", 50*time.Millisecond)
		if err != nil {
			t.Errorf("hooks.lock was held while a lookup ran: %v", err)
			return
		}
		unlock()
	}
	head := WithGitHead(func(string, bool) (string, *bool) {
		close(headStarted)
		lockFree()
		if !waitFor(keyStarted) {
			return "", nil
		}
		return startCommit, new(false)
	})
	key := WithRepoKey(func(string) string {
		close(keyStarted)
		if !waitFor(headStarted) {
			return ""
		}
		return archive.RepoKey("https://example.test/acme/widget.git")
	})
	if err := HandleEvent(home, "codex", startPayload("/work/widget"), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), WithDecoders(testDecoders), head, key); err != nil {
		t.Fatal(err)
	}
	if reg := onlyRegistration(t, home); reg.StartHead == nil || reg.RepoKey == "" {
		t.Errorf("registration has StartHead %+v and RepoKey %q; want both", reg.StartHead, reg.RepoKey)
	}
}

// A lookup that hangs costs the hook the budget, not the hang.
func TestHookGivesUpOnACommitLookupThatHangs(t *testing.T) {
	wall := time.Now()
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Until(wall))
		home := t.TempDir()
		at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
		release := make(chan struct{})
		answer := time.AfterFunc(time.Minute, func() { close(release) })
		t.Cleanup(func() {
			if answer.Stop() {
				close(release)
			}
		})
		lookup := WithGitHead(func(string, bool) (string, *bool) { <-release; return startCommit, nil })
		start := time.Now()
		if err := HandleEvent(home, "codex", startPayload("/work/widget"), at, WithDecoders(testDecoders), lookup); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != repoKeyBudget {
			t.Errorf("hung lookup spent %v, want %v", elapsed, repoKeyBudget)
		}
		if reg := onlyRegistration(t, home); reg.StartHead != nil {
			t.Errorf("late lookup recorded: %+v", reg.StartHead)
		}
	})
}

func TestStopCommitLookupRequiresTheRegisteredAgentAndProject(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	if err := HandleEvent(home, "claude", startPayload("/work/widget"), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: "/work/other", Included: true, ActivatedAt: at.Add(-time.Hour)})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	git := &headLookup{sha: laterCommit}
	for _, tc := range []struct {
		agent string
		root  string
	}{{"codex", "/work/widget"}, {"claude", "/work/other"}} {
		if err := HandleEvent(home, tc.agent, stopPayload(tc.root), at.Add(time.Minute), WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
			t.Fatal(err)
		}
	}
	if got := git.questions(); len(got) != 0 {
		t.Errorf("git asked for another agent or project: %v", got)
	}
	if reg := onlyRegistration(t, home); reg.LastHead != nil {
		t.Errorf("last HEAD changed: %+v", reg.LastHead)
	}
}

func TestOlderStopCommitCannotReplaceANewerObservation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	if err := HandleEvent(home, "codex", startPayload("/work/widget"), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	reg := onlyRegistration(t, home)
	store := state.OpenReadOnly(home)
	newer := &archive.GitHead{SHA: laterCommit, ObservedAt: at.Add(2 * time.Minute)}
	older := &archive.GitHead{SHA: startCommit, ObservedAt: at.Add(time.Minute)}
	if err := recordLastHead(store, reg, newer); err != nil {
		t.Fatal(err)
	}
	// Model a concurrent hook whose lookup started earlier but acquired the
	// shared lock after the newer hook. Its cached registration is also stale.
	if err := recordLastHead(store, reg, older); err != nil {
		t.Fatal(err)
	}
	if got := onlyRegistration(t, home).LastHead; got == nil || got.SHA != laterCommit || !got.ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("newer observation replaced: %+v", got)
	}
}

// A later stop at the same commit must still fence an older in-flight
// observation at another commit, even after the registration is reloaded.
func TestOlderStopCannotReplaceALaterRepeatedCommit(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	if err := HandleEvent(home, "codex", startPayload("/work/widget"), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store := state.OpenReadOnly(home)
	for _, observation := range []struct {
		sha     string
		minutes int
	}{
		{laterCommit, 1}, {laterCommit, 3}, {startCommit, 2},
	} {
		reg := onlyRegistration(t, home)
		head := &archive.GitHead{SHA: observation.sha, ObservedAt: at.Add(time.Duration(observation.minutes) * time.Minute)}
		if err := recordLastHead(store, reg, head); err != nil {
			t.Fatal(err)
		}
	}
	got := onlyRegistration(t, home).LastHead
	if got == nil || got.SHA != laterCommit || !got.ObservedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("older stop replaced latest repeated commit: %+v", got)
	}
}

// A stop that finds hooks.lock busy is replayed from its queued intent, and
// the replay records the commit the hook saw before it queued.
func TestContendedStopKeepsTheCommitItSaw(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	conversation := "5f3c2a10-0000-4000-8000-00000000c456"
	git := &headLookup{sha: startCommit, dirty: new(false)}
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), at, WithDecoders(testDecoders), WithGitHead(git.lookup)); err != nil {
		t.Fatal(err)
	}
	git.mu.Lock()
	git.sha = laterCommit
	git.mu.Unlock()
	stopAt := at.Add(time.Minute)
	batch, err := testBatch("cursor", cursorDesktopPayload("stop", conversation, project, cursorTranscriptLocation(t, conversation)), stopAt)
	if err != nil {
		t.Fatal(err)
	}
	busy := func(string, time.Duration) (func(), error) { return nil, local.ErrBusy }
	if err := handleBatch(home, "cursor", batch, stopAt, busy, nil, eventOptions{decoders: testDecoders, gitHead: git.lookup}); err != nil {
		t.Fatalf("contended stop was not queued: %v", err)
	}
	if reg := onlyRegistration(t, home); reg.LastHead != nil && reg.LastHead.SHA == laterCommit {
		t.Fatalf("the contended stop wrote without the lock: %+v", reg.LastHead)
	}
	if err := ReplayAdmissionIntents(home, stopAt.Add(time.Second), testDecoders); err != nil {
		t.Fatal(err)
	}
	reg := onlyRegistration(t, home)
	if reg.LastHead == nil || reg.LastHead.SHA != laterCommit || !reg.LastHead.ObservedAt.Equal(stopAt) {
		t.Errorf("LastHead after replay = %+v, want %s seen at %s", reg.LastHead, laterCommit, stopAt)
	}
}
