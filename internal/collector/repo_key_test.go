package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// countingLookup stands in for the git lookup: it answers from keys (a root
// not in it has none) and counts how often each root was asked about.
type countingLookup struct {
	mu    sync.Mutex
	keys  map[string]string
	asked map[string]int
}

func (l *countingLookup) lookup(root string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.asked == nil {
		l.asked = map[string]int{}
	}
	l.asked[root]++
	return l.keys[root]
}

func (l *countingLookup) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, count := range l.asked {
		n += count
	}
	return n
}

func (l *countingLookup) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = nil
}

var (
	widgetKey = archive.RepoKey("https://example.test/acme/widget.git")
	gadgetKey = archive.RepoKey("https://example.test/acme/gadget.git")
)

func TestPublicationCarriesTheRegistrationsRepoKeyWithoutAskingGit(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	reg.RepoKey = widgetKey
	remote := storagetest.NewMemoryStore()
	git := &countingLookup{keys: map[string]string{"/p": gadgetKey}}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", RepoKey: git.lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	if got := publishOnce(t, local, remote, reg, &opts); got.RepoKey != widgetKey {
		t.Errorf("repo_key = %q, want the registration's %q", got.RepoKey, widgetKey)
	}
	if git.total() != 0 {
		t.Errorf("git was asked %d times for a registration that has a key", git.total())
	}
}

func TestPublicationDerivesTheRepoKeyFromTheProjectRootWhenTheRegistrationHasNone(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	git := &countingLookup{keys: map[string]string{"/p": widgetKey}}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", RepoKey: git.lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	if got := publishOnce(t, local, remote, reg, &opts); got.RepoKey != widgetKey {
		t.Errorf("repo_key = %q, want %q", got.RepoKey, widgetKey)
	}
	if git.asked["/p"] != 1 {
		t.Errorf("git asked about /p %d times, want 1", git.asked["/p"])
	}
}

func TestPublicationWithoutARepoKeyOmitsTheField(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	publishOnce(t, local, remote, reg, &opts)
	key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	raw, err := remote.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "repo_key") {
		t.Errorf("a session with no repository carries a repo_key: %s", raw)
	}
}

// Only a hash may reach the sidecar, whatever a lookup or a registration
// hands the collector.
func TestPublicationNeverCarriesAnythingButARepoKey(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	reg.RepoKey = "https://user:synthetic-token@example.test/acme/widget.git"
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	publishOnce(t, local, remote, reg, &opts)
	key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	raw, err := remote.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-token") || strings.Contains(string(raw), "repo_key") {
		t.Errorf("sidecar carries a value that is not a repo key: %s", raw)
	}
}

// A sidecar from any earlier parser gains repo_key, once: 0.13.0 predates the
// token fields, 0.15.0 is the last parser before repo_key.
func TestParserUpgradeGivesAnOldSidecarItsRepoKeyOnce(t *testing.T) {
	t.Parallel()
	for _, oldVersion := range []string{"0.13.0", "0.15.0"} {
		t.Run(oldVersion, func(t *testing.T) {
			t.Parallel()
			checkOldSidecarGainsRepoKey(t, oldVersion)
		})
	}
}

func checkOldSidecarGainsRepoKey(t *testing.T, oldVersion string) {
	t.Helper()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := &countedPublications{ObjectStore: storagetest.NewMemoryStore()}
	git := &countingLookup{keys: map[string]string{"/p": widgetKey}}
	now := reg.RegisteredAt.Add(time.Hour)
	// The sidecar as an earlier parser wrote it: no repo_key.
	old := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: oldVersion, RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return now }}
	if before := publishOnce(t, local, remote, reg, &old); before.RepoKey != "" || before.Parser.Version != oldVersion {
		t.Fatalf("setup: old sidecar = %+v", before)
	}

	now = now.Add(time.Hour)
	current := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", RepoKey: git.lookup, Now: func() time.Time { return now }}
	remote.keys = nil
	result, err := Run(context.Background(), local, remote, current)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("refresh: %#v %v", result, err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.RepoKey != widgetKey || after.Parser.Version != archive.DefaultParserVersion {
		t.Fatalf("refreshed sidecar = repo_key %q, parser %q; want %q, %q", after.RepoKey, after.Parser.Version, widgetKey, archive.DefaultParserVersion)
	}
	// The refresh republishes metadata (and its listing entries) over the
	// retained source, never the source itself.
	metadataKey, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	wroteMetadata := false
	for _, key := range remote.keys {
		wroteMetadata = wroteMetadata || key == metadataKey
		if strings.Contains(key, "/source.") {
			t.Errorf("the refresh rewrote the source: %s", key)
		}
	}
	if !wroteMetadata {
		t.Errorf("the refresh wrote %v, not the metadata", remote.keys)
	}

	// Nothing changed since: the next passes publish nothing, and do not ask
	// git again for a session that is already current.
	now = now.Add(time.Hour)
	remote.keys = nil
	git.reset()
	result, err = Run(context.Background(), local, remote, current)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 || len(remote.keys) != 0 {
		t.Fatalf("unchanged pass: %#v %v %v", result, err, remote.keys)
	}
	if git.total() != 0 {
		t.Errorf("an unchanged session asked git %d times", git.total())
	}
}

func TestRefreshSweepAsksGitOncePerProjectRoot(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	roots := []string{"/widget", "/widget", "/widget", "/gadget", "/gadget"}
	dir := t.TempDir()
	for i, root := range roots {
		reg := registration(t, writeTranscript(t, dir, fmt.Sprintf("s%d.jsonl", i), codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID, reg.ProjectRoot = fmt.Sprintf("session-%d", i), fmt.Sprintf("native-%d", i), root
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	git := &countingLookup{keys: map[string]string{"/widget": widgetKey, "/gadget": gadgetKey}}
	old := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, old); err != nil || len(result.Errors) != 0 || len(result.Published) != len(roots) {
		t.Fatalf("setup: %#v %v", result, err)
	}

	now = now.Add(time.Hour)
	current := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "two", RepoKey: git.lookup, Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, current); err != nil || len(result.Errors) != 0 || len(result.Published) != len(roots) {
		t.Fatalf("refresh: %#v %v", result, err)
	}
	if git.asked["/widget"] != 1 || git.asked["/gadget"] != 1 || git.total() != 2 {
		t.Errorf("git was asked %v for five sessions of two projects, want once per project", git.asked)
	}
	for i, root := range roots {
		want := map[string]string{"/widget": widgetKey, "/gadget": gadgetKey}[root]
		if got := fetchMetadata(t, remote, "codex", fmt.Sprintf("session-%d", i)).RepoKey; got != want {
			t.Errorf("session-%d repo_key = %q, want %q", i, got, want)
		}
	}
}

func TestSubagentRegistrationCopiesTheParentsRepoKey(t *testing.T) {
	t.Parallel()
	parent := archive.SessionRegistration{
		ArchiveSessionID: "parent", NativeSessionID: "native-parent", ProjectID: "project-1", ProjectRoot: "/p",
		RepoKey: widgetKey, Harness: archive.Harness{Name: "claude"},
	}
	child := assembleSubagentRegistration(parent, state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-parent:subagent:a", AgentID: "a"})
	if child.RepoKey != widgetKey {
		t.Errorf("subagent repo key = %q, want its parent's %q", child.RepoKey, widgetKey)
	}
}
