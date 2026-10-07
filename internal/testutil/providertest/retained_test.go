package providertest

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRetainedFixtureMaximumSetHasReadableExactIdentities(t *testing.T) {
	remote := storagetest.NewMemoryStore()
	f := PutRetainedFixture(t, remote, archive.MaxPreservedRevisions, time.Now().UTC())
	key, err := archive.MetadataObjectKey(f.Registration.Harness.Name, f.Registration.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	m, err := reader.ReadMetadata(t.Context(), remote, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.History.Preserved) != archive.MaxPreservedRevisions {
		t.Fatal("fixture did not reach the preserved-ref bound")
	}
	for _, ref := range m.History.Preserved {
		bundle, err := reader.LoadRevisionSource(t.Context(), remote, m, ref, reader.Limits{})
		if err != nil || bundle.History.ActiveRolloutID != ref.RevisionID || !bundle.Capture.CapturedAt.Equal(ref.CapturedAt) {
			t.Fatal("fixture supplied unreadable or misattributed retained evidence", err)
		}
	}
}
