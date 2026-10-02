package sourceio

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBorrowedFileLifetimeAndIdempotentClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := (FileProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := p.Read(context.Background(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{RawBytes: 3, RecordBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	first, second := snap.Input(), snap.Input()
	if first.File != second.File || first.File.Length() != 3 {
		t.Fatal("Input copied its file view")
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if err = snap.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = first.File.ReadAt(make([]byte, 1), 0); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("expired read: %v", err)
	}
	if _, err = p.Signature(context.Background(), agentapi.SourceRef{Path: path}); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal(err)
	}
}

func TestProviderSignatureCeilingAndLegacy32Hash(t *testing.T) {
	legacy := cursorstore.Signature{LastUpdatedAt: -1, HeaderCount: 2, LastBubbleID: strings.Repeat("opaque-", 10000), MessageRows: 1, LastMessageHash: strings.Repeat("a", 32)}
	cursor := CursorSignature(legacy)
	if len(cursor.Token) != 64 {
		t.Fatal(cursor)
	}
	b, _ := json.Marshal(cursor)
	if len(b) > 256 {
		t.Fatalf("signature has %d bytes", len(b))
	}
	if err := ValidateSignature(cursor); err != nil {
		t.Fatal(err)
	}
	changed := legacy
	changed.LastBubbleID += "x"
	if CursorSignature(changed) == cursor {
		t.Fatal("native ID was truncated")
	}
	file := FileSignature(0, time.Unix(0, -1).UnixNano())
	if len(file.Token) != 22 {
		t.Fatal(file)
	}
	if FileSignature(0, -1) != file || FileSignature(0, 0) == file {
		t.Fatal("signed timestamp equality changed")
	}
	invalid := cursor
	invalid.Provider = strings.Repeat("p", 65)
	if ValidateSignature(invalid) == nil {
		t.Fatal("unbounded provider")
	}
	invalid = cursor
	invalid.Version = 2
	if ValidateSignature(invalid) == nil {
		t.Fatal("unknown signature version")
	}
}

func TestCancellationAndRawBoundaryLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (FileProvider{}).OpenPass(ctx, agentapi.SourceEnvironment{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "transcript")
	if err := os.WriteFile(path, []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	p, _ := (FileProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{})
	defer func() { _ = p.Close() }()
	if _, err := p.Read(context.Background(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{RawBytes: 3}); !errors.Is(err, agentapi.ErrRawLimit) {
		t.Fatal(err)
	}
	missing, err := p.Signature(context.Background(), agentapi.SourceRef{Path: path + "-missing"})
	if !errors.Is(err, os.ErrNotExist) || missing.Present {
		t.Fatalf("missing: %+v %v", missing, err)
	}
}

func TestCleanupFaultIsJoinedAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("four"), 0600); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("synthetic close fault")
	files := faultFiles{fault: fault}
	p, _ := (FileProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{Files: files})
	_, err := p.Read(context.Background(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{RawBytes: 1})
	if !errors.Is(err, agentapi.ErrRawLimit) || !errors.Is(err, fault) || !agentapi.HasFailure(err, agentapi.Cleanup) || agentapi.Deterministic(err) {
		t.Fatalf("lost joined cleanup fault: %v", err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
}

type faultFiles struct {
	transcriptio.OS
	fault error
}

func (f faultFiles) OpenRegular(path string) (transcriptio.File, error) {
	file, err := f.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return &faultFile{File: file, fault: f.fault}, nil
}

type faultFile struct {
	transcriptio.File
	fault error
}

func (f *faultFile) Close() error { return errors.Join(f.File.Close(), f.fault) }

// A refusal is cacheable only if the source remains the exact observed snapshot.
func TestFailedFilterStillVerifiesSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pass, err := (FileProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	snap, err := pass.Read(context.Background(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = FilterJSONL(context.Background(), snap.Input(), agentapi.FilterContext{}, func(r io.Reader) (archive.FilteredTranscript, error) {
		if _, readErr := io.Copy(io.Discard, r); readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(path, []byte("changed\n"), 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
		return archive.FilteredTranscript{}, archive.ErrUnsafeSourceFormat
	})
	if !agentapi.HasFailure(err, agentapi.FormatMismatch) || !agentapi.HasFailure(err, agentapi.Changed) || agentapi.Deterministic(err) {
		t.Fatalf("lost snapshot verification: %v", err)
	}
}

func TestRefusedFilterRetainsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pass, err := (FileProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	snap, err := pass.Read(context.Background(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = FilterJSONL(ctx, snap.Input(), agentapi.FilterContext{}, func(io.Reader) (archive.FilteredTranscript, error) {
		cancel()
		return archive.FilteredTranscript{}, &archive.FilterError{Reason: "transcript cannot be read"}
	})
	if !errors.Is(err, context.Canceled) || agentapi.Deterministic(err) {
		t.Fatalf("filter refusal hid cancellation: %v", err)
	}
}
