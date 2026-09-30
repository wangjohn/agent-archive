package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A publication carries the commits the session's hooks recorded on its
// registration, and nothing when they recorded none: the collector never
// asks git for a commit itself.
func TestPublicationCarriesTheCommitsTheHooksRecorded(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	dirty := true
	reg.StartHead = &archive.GitHead{SHA: strings.Repeat("3f", 20), Dirty: &dirty, ObservedAt: reg.RegisteredAt}
	reg.LastHead = &archive.GitHead{SHA: strings.Repeat("9e", 20), ObservedAt: reg.RegisteredAt.Add(time.Minute)}
	remote := storagetest.NewMemoryStore()
	opts := Options{MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	got := publishOnce(t, local, remote, reg, &opts).GitHead
	if got == nil || got.Start == nil || got.Start.SHA != reg.StartHead.SHA || got.Start.Dirty == nil || !*got.Start.Dirty ||
		got.Last == nil || got.Last.SHA != reg.LastHead.SHA {
		t.Errorf("git_head = %+v, want the registration's", got)
	}
}

func TestPublicationWithoutRecordedCommitsOmitsGitHead(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	opts := Options{MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	publishOnce(t, local, remote, reg, &opts)
	key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	raw, err := remote.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "git_head") {
		t.Errorf("a session with no recorded commit carries git_head: %s", raw)
	}
}
