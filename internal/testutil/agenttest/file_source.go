package agenttest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// FileSource checks a verified file boundary, append detection and terminal
// ownership. Its caller supplies an isolated writable fixture file.
func FileSource(t *testing.T, p agentapi.SourceProvider, ref agentapi.SourceRef, want []byte) {
	t.Helper()
	ctx := t.Context()
	pass, err := p.OpenPass(ctx, agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := pass.Read(ctx, ref, agentapi.ReadLimits{RawBytes: 256 << 20, RecordBytes: 64 << 20})
	if err != nil {
		_ = pass.Close()
		t.Fatal(err)
	}
	input := snapshot.Input()
	if input.File == nil || input.Records != nil {
		t.Fatal("file provider returned wrong input union")
	}
	raw := make([]byte, len(want))
	if _, err := input.File.ReadAt(raw, 0); err != nil || !bytes.Equal(raw, want) {
		t.Fatalf("borrowed file: %q %v", raw, err)
	}
	f, err := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte("{}\n"))
	err = errors.Join(writeErr, f.Close())
	if err != nil {
		t.Fatal(err)
	}
	if input.File.Length() != int64(len(want)) {
		t.Fatal("append expanded captured boundary")
	}
	if err := input.File.Check(); err == nil {
		t.Fatal("observable append was not rejected")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	called := false
	if _, err := input.File.Records(canceled, true, 0, 64<<20, func([]byte) bool { called = true; return true }); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("file cancellation: %v callback=%v", err, called)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := input.File.ReadAt(raw, 0); err == nil {
		t.Fatal("closed snapshot remained readable")
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pass.Read(ctx, ref, agentapi.ReadLimits{}); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("terminal file pass reopened: %v", err)
	}
}
