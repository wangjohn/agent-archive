package agenttest

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Sources checks shared lifetime, presence, bounds and cancellation contracts
// against one synthetic stable source under its provider's consistency model.
func Sources(t *testing.T, provider agentapi.SourceProvider, env agentapi.SourceEnvironment, ref, missing agentapi.SourceRef) {
	t.Helper()
	ctx := t.Context()
	pass, err := provider.OpenPass(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	})
	before, err := pass.Signature(ctx, ref)
	if err != nil || !before.Present {
		t.Fatalf("stable signature: %+v %v", before, err)
	}
	if missing, err := pass.Signature(ctx, missing); err == nil || missing.Present || agentapi.Failure(err) != agentapi.Missing {
		t.Fatalf("missing source: %+v %v", missing, err)
	}
	if counter, ok := pass.(interface{ Attempts() int }); ok && counter.Attempts() != 0 {
		t.Fatal("signature-only observations attempted a database snapshot")
	}
	if _, err := pass.Read(ctx, ref, agentapi.ReadLimits{RawBytes: 1, RecordBytes: 1}); !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatalf("bounded refusal: %v", err)
	}
	snapshot, err := pass.Read(ctx, ref, agentapi.ReadLimits{RawBytes: 1 << 20, RecordBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if observed := snapshot.Observation(); !observed.Present || observed.Signature != before.Signature {
		t.Fatalf("stable read observation: %+v want %+v", observed, before)
	}
	if counter, ok := pass.(interface{ Attempts() int }); ok && counter.Attempts() > 1 {
		t.Fatal("bounded refusal and successful read repeated the snapshot attempt")
	}
	input := snapshot.Input()
	if (input.File == nil) == (input.Records == nil) {
		t.Fatal("input must contain exactly one file or record view")
	}
	checkBorrowedInput(t, snapshot, input)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := pass.Read(canceled, ref, agentapi.ReadLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if input.File != nil {
		if _, err := input.File.ReadAt(make([]byte, 1), 0); !errors.Is(err, agentapi.ErrClosed) {
			t.Fatalf("expired file: %v", err)
		}
	} else if _, _, err := input.Records.Next(ctx); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("expired records: %v", err)
	}
	if _, err := pass.Signature(ctx, ref); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("closed pass: %v", err)
	}
}

func checkBorrowedInput(t *testing.T, snapshot agentapi.SourceSnapshot, input agentapi.NativeInput) {
	t.Helper()
	ctx := t.Context()
	if input.File != nil {
		if _, err := input.File.ReadAt(make([]byte, 1), input.File.Length()); !errors.Is(err, io.EOF) {
			t.Fatalf("unbounded file view: %v", err)
		}
		if input.File != snapshot.Input().File {
			t.Fatal("Input copied the file view")
		}
	} else {
		first, ok, err := input.Records.Next(ctx)
		if err != nil || !ok || len(first.Raw) == 0 {
			t.Fatalf("initial record: %+v %v %v", first, ok, err)
		}
		again, ok, err := snapshot.Input().Records.Next(ctx)
		if err != nil || !ok || &first.Raw[0] != &again.Raw[0] {
			t.Fatal("Input copied or reparsed native record bytes")
		}
	}
}
