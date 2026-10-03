package collector

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
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
	defer func() { _ = snapshot.Close() }()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not the selected transcript"), 0600); err != nil {
		t.Fatal(err)
	}
	filtered, _, err := FilterTranscriptSnapshot(context.Background(), snapshot, "claude", time.Time{}, DefaultMaxTranscriptBytes, testSources)
	if err != nil || len(filtered.Records) != 1 {
		t.Fatalf("verified handle: records=%d err=%v", len(filtered.Records), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := FilterTranscriptSnapshot(ctx, snapshot, "claude", time.Time{}, DefaultMaxTranscriptBytes, testSources); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled filter: %v", err)
	}
}

func TestFileReaderPassesCancellationToTranscriptFilter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := fileReader{reg: archive.SessionRegistration{TranscriptPath: path}}
	if _, _, err := source.Filter(ctx, claude.Filter{}, DefaultMaxTranscriptBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}

// Filtering and retained comparison must use the operation's selected filter,
// even when a lookup constructs another instance for provider resolution.
func TestProviderReaderUsesSelectedFilter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	selectedError := errors.New("selected filter")
	selected := selectedTranscriptFilter{TranscriptFilter: claude.Filter{}, err: selectedError}
	r, _ := newSourceReader(archive.SessionRegistration{TranscriptPath: path, Harness: archive.Harness{Name: "claude"}}, Options{Sources: testSources})
	if _, _, err := r.Filter(t.Context(), selected, DefaultMaxTranscriptBytes); !errors.Is(err, selectedError) {
		t.Fatalf("selected filter was replaced: %v", err)
	}
}

type selectedTranscriptFilter struct {
	agentapi.TranscriptFilter
	err error
}

func (f selectedTranscriptFilter) Filter(context.Context, agentapi.NativeInput, agentapi.FilterContext) (archive.FilteredTranscript, error) {
	return archive.FilteredTranscript{}, f.err
}

func TestQualifiedAppendOnlySourceRetainsSizeForRewriteDetection(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	const kind archive.SourceKind = "example/append"
	reg := archive.SessionRegistration{ArchiveSessionID: "qualified-source", SourceKind: kind, Harness: archive.Harness{Name: "claude"}}
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := sessionScan{local: store, reg: reg, published: published, opts: Options{Sources: qualifiedAppendSources{}}}
	observation := agentapi.SourceObservation{Present: true, Size: 100, Signature: agentapi.SourceSignature{Version: 1, Provider: "example", Token: "revision-a"}}
	observed := observe(reg.SourceKind, observation)
	if err := scan.recordScanSignature(observed, archive.SourceBundle{}); err != nil {
		t.Fatal(err)
	}
	signature, found, err := store.LoadScanSignature(reg.ArchiveSessionID)
	if err != nil || !found || signature.TranscriptSize != 100 {
		t.Fatalf("saved raw size: %#v found=%v err=%v", signature, found, err)
	}
	for _, size := range []int64{50, 100, 150} {
		observation.Size = size
		rewritten, err := scan.rewrittenSinceCapture(sourceRead{observed: observe(reg.SourceKind, observation)})
		if err != nil || rewritten != (size < 100) {
			t.Fatalf("size %d: rewritten=%v err=%v", size, rewritten, err)
		}
	}
}

type qualifiedAppendSources struct{}

func (qualifiedAppendSources) LookupSources(string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	return qualifiedAppendProvider{}, claude.Filter{}, true
}

type qualifiedAppendProvider struct{ sourceio.FileProvider }

func (qualifiedAppendProvider) Describe(agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return agentapi.SourceSemantics{Mutation: agentapi.AppendOnly, Provider: "example"}, nil
}
