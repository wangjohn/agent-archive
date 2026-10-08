package catalog

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func largeLinkedMutation(t *testing.T, w *Writer, id string) CatalogMutation {
	t.Helper()
	m := mutation(t, w, id)
	for i := range 2000 {
		m.Next.Summary.LinkedSessions = append(m.Next.Summary.LinkedSessions, archive.LinkedSessionReference{SessionID: fmt.Sprintf("linked-%04d", i), Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: m.Next.Summary.CapturedAt})
	}
	raw, err := json.Marshal(m.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= maxNodeBytes || len(raw) >= 32<<20 {
		t.Fatal("fixture did not exercise supported metadata overflow", len(raw))
	}
	var decoded archive.Metadata
	if err = json.Unmarshal(raw, &decoded); err != nil || len(decoded.LinkedSessions) != 2000 {
		t.Fatal("invalid metadata fixture", err)
	}
	m.Next.Metadata, err = w.PutImmutable(t.Context(), KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.verifyEntry(t.Context(), m.SessionKey, m.Next); err != nil {
		t.Fatal("metadata fixture authority invalid", err)
	}
	return m
}

func TestPublicationSupportsMetadataLargerThanTreeLeaf(t *testing.T) {
	w, _ := fixture(t)
	m := largeLinkedMutation(t, w, "large-linked")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatalf("valid 2000-link metadata publication failed: %v", err)
	}
}
