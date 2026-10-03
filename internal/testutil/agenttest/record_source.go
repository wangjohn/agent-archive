package agenttest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// RecordSource checks independent cursors, cancellation and terminal ownership
// for a real provider populated with at least two synthetic records.
func RecordSource(t *testing.T, p agentapi.SourceProvider, ref agentapi.SourceRef, env agentapi.SourceEnvironment) {
	t.Helper()
	ctx := t.Context()
	pass, err := p.OpenPass(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := pass.Read(ctx, ref, agentapi.ReadLimits{RawBytes: 256 << 20, RecordBytes: 64 << 20})
	if err != nil {
		_ = pass.Close()
		t.Fatal(err)
	}
	first, second := snapshot.Input(), snapshot.Input()
	if first.File != nil || first.Records == nil || second.Records == nil {
		t.Fatal("record provider returned wrong input union")
	}
	a, ok, err := first.Records.Next(ctx)
	if err != nil || !ok {
		t.Fatalf("first record: %+v %v %v", a, ok, err)
	}
	saved := bytes.Clone(a.Raw)
	b, ok, err := second.Records.Next(ctx)
	if err != nil || !ok || a.Kind != b.Kind || a.Key != b.Key || !bytes.Equal(saved, b.Raw) {
		t.Fatalf("independent cursor: %+v %v %v", b, ok, err)
	}
	if _, ok, err := first.Records.Next(ctx); err != nil || !ok || !bytes.Equal(saved, a.Raw) {
		t.Fatalf("borrowed first record changed: %v %v", ok, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := second.Records.Next(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cursor cancellation: %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.Records.Next(ctx); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("closed snapshot cursor: %v", err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pass.Read(ctx, ref, agentapi.ReadLimits{}); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("terminal pass reopened: %v", err)
	}
}
