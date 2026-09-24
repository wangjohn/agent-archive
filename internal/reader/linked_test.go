package reader

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestLinkedSessionsResolveWithoutDownloadingOrPinningSources(t *testing.T) {
	parent, _, store := fixture(t)
	parent.LinkedSessions = []archive.LinkedSessionReference{{SessionID: "child", Relationship: "subagent", Status: archive.LinkedSessionPending}}
	ctx := context.Background()
	check := func(want LinkedState) {
		t.Helper()
		got := ResolveLinkedSessions(ctx, store, parent)
		if len(got) != 1 || got[0].State != want {
			t.Fatalf("want %s got %+v", want, got)
		}
	}
	check(LinkedStatePending)
	child := parent
	child.SessionID = "child"
	child.ParentSessionID = parent.SessionID
	child.LinkedSessions = nil
	child.SourceBundle.Key = "sessions/codex/child/source." + strings.Repeat("a", 64) + ".jsonl.gz"
	child.SourceBundle.SHA256 = strings.Repeat("a", 64)
	key, _ := archive.MetadataObjectKey("codex", "child")
	save := func() {
		t.Helper()
		data, err := json.Marshal(child)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
	}
	save()
	// No child source was uploaded: resolution must be metadata-only.
	check(LinkedStateMetadataAvailable)
	parent.LinkedSessions[0].Status = archive.LinkedSessionPublished
	child.ParentSessionID = "other-parent"
	save()
	check(LinkedStateIdentityMismatch)
	child.ParentSessionID = parent.SessionID
	child.MachineID = "other-machine"
	save()
	check(LinkedStateIdentityMismatch)
	if err := store.Put(ctx, key, []byte("invalid-json")); err != nil {
		t.Fatal(err)
	}
	check(LinkedStateLookupFailed)
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	check(LinkedStateUnavailableOrExpired)
	parent.LinkedSessions[0].Status = archive.LinkedSessionUnavailable
	check(LinkedStateUnavailable)
}

func TestSourceReadRejectsMismatchedParent(t *testing.T) {
	metadata, _, store := fixture(t)
	metadata.ParentSessionID = "unrelated-parent"
	if _, err := LoadSource(context.Background(), store, metadata, Limits{}); err == nil {
		t.Fatal("source ownership mismatch accepted")
	}
}
