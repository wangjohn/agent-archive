package collector

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFilterSnapshotKeepsVerifiedHandleAndHonorsCancellation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.jsonl")
	content := []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"Original question\"}}\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := transcriptio.Open(transcriptio.OS{}, path, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not the selected transcript"), 0600); err != nil {
		t.Fatal(err)
	}
	filtered, _, err := FilterTranscriptSnapshot(context.Background(), snapshot, "claude", time.Time{}, DefaultMaxTranscriptBytes)
	if err != nil || len(filtered.Records) != 1 {
		t.Fatalf("verified handle: records=%d err=%v", len(filtered.Records), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := FilterTranscriptSnapshot(ctx, snapshot, "claude", time.Time{}, DefaultMaxTranscriptBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled filter: %v", err)
	}
}
