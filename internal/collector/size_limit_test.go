package collector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// keptTranscript is a Codex transcript whose user messages, all of which the
// filter keeps, come to about n bytes.
func keptTranscript(n int) string {
	var b strings.Builder
	b.WriteString(`{"type":"turn_context","model":"gpt-test"}` + "\n")
	text := strings.Repeat("kept words ", 6000)
	for b.Len() < n {
		b.WriteString(`{"type":"response_item","timestamp":"2026-01-01T00:01:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}` + "\n")
	}
	return b.String()
}

// TestSessionOverItsReadShareIsANamedGapOnce: a session that needs more than
// one session's share of a pass is recorded as a size-limit gap naming that
// limit, once: the next pass skips it without reading it, and the pass's
// status names the limit. It is not an error every pass.
//
// Not parallel: it lowers sessionReadCeiling and counts transcriptFilters.
func TestSessionOverItsReadShareIsANamedGapOnce(t *testing.T) {
	previous := sessionReadCeiling
	sessionReadCeiling = 36 << 20
	t.Cleanup(func() { sessionReadCeiling = previous })
	path := writeTranscript(t, t.TempDir(), "large.jsonl", keptTranscript(40<<20))
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 || len(result.Published) != 0 || len(result.Deferred) != 0 {
		t.Fatalf("over its share must be a recorded gap: %#v %v", result, result.Errors)
	}
	if reason, blocked, err := local.LoadBlocked("session-1"); err != nil || !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	signature, _, err := local.LoadScanSignature("session-1")
	if err != nil || signature.SizeLimit == nil || signature.SizeLimit.Kind != state.SizeLimitReadBudget || signature.SizeLimit.LimitBytes != 36<<20 || signature.SizeLimit.SizeBytes < 40<<20 {
		t.Fatalf("gap does not name the read limit: %+v err=%v", signature.SizeLimit, err)
	}
	status, err := local.LoadStatus()
	if err != nil || len(status.LastErrors) != 1 || !strings.Contains(status.LastErrors[0], "(per-session read memory 36 MiB)") || !IsSizeLimitProblem(status.LastErrors[0]) {
		t.Fatalf("status problem: %q err=%v", status.LastErrors, err)
	}
	filters := transcriptFilters.Load()
	again, err := Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Errors) != 0 || len(again.Skipped) != 1 || transcriptFilters.Load() != filters {
		t.Fatalf("an unchanged gap was read again: %#v filters %d -> %d", again, filters, transcriptFilters.Load())
	}
	if status, err := local.LoadStatus(); err != nil || len(status.LastErrors) != 1 || !strings.Contains(status.LastErrors[0], "per-session read memory 36 MiB") {
		t.Fatalf("second pass status: %q err=%v", status.LastErrors, err)
	}
	// Within its share, the same session is captured.
	sessionReadCeiling = previous
	if err := local.SaveRegistration(registration(t, writeTranscript(t, t.TempDir(), "large.jsonl", keptTranscript(40<<20)))); err != nil {
		t.Fatal(err)
	}
	captured, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(captured.Errors) != 0 || len(captured.Published) != 1 {
		t.Fatalf("within its share: %#v err=%v", captured, err)
	}
}

// TestRaisedRawLimitReadsAGapAgain: a size-limit gap stands only while the
// limit it was reached under does. A gap from before the raw limit was
// recorded (a Codex thread over the former 128 MiB cap) is read again.
func TestRaisedRawLimitReadsAGapAgain(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}
	publishOnce(t, local, remote, reg, &opts)
	signature, _, err := local.LoadScanSignature(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	signature.Failed, signature.Blocked = true, state.BlockedReasonTranscriptTooLarge
	signature.FailedMaxBytes, signature.FailedRecordLimit = opts.maxTranscriptBytes(), recordLimit
	signature.FailedRawLimit = opts.maxRawBytes(reg.Harness.Name)
	if err := local.SaveScanSignature(reg.ArchiveSessionID, signature); err != nil {
		t.Fatal(err)
	}
	stored, _, err := local.LoadRegistration(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged(t, local, stored, opts) {
		t.Fatal("a gap under the current limits is read again")
	}
	signature.FailedRawLimit = 128 << 20
	if err := local.SaveScanSignature(reg.ArchiveSessionID, signature); err != nil {
		t.Fatal(err)
	}
	if unchanged(t, local, stored, opts) {
		t.Fatal("a gap reached under a lower raw limit still stands")
	}
}

func TestCodexRawLimitIsTheSharedConstant(t *testing.T) {
	t.Parallel()
	if got := maxRawBytes(archive.HarnessCodex, DefaultMaxTranscriptBytes); got != archive.MaxCodexRawTranscriptBytes || got < DefaultMaxRawTranscriptBytes {
		t.Fatalf("Codex raw limit %d", got)
	}
	if got := maxRawBytes(archive.HarnessClaude, DefaultMaxTranscriptBytes); got != DefaultMaxRawTranscriptBytes {
		t.Fatalf("general raw limit %d", got)
	}
}

// TestReadBudgetDeferralIsOnlyTheSourceRead: a source read refused while
// others held the pass's ledger defers the session; the same refusal
// anywhere else stays a failure, as does one joined with a failure.
func TestReadBudgetDeferralIsOnlyTheSourceRead(t *testing.T) {
	t.Parallel()
	refusal := agentapi.ReadBudgetLimit(errors.New("native record buffer budget exhausted"))
	if reason, ok := deferralReason(&readBudgetDeferralError{err: refusal}); !ok || reason != DeferredReadBudget {
		t.Fatalf("source read refusal: %q %v", reason, ok)
	}
	if reason, ok := deferralReason(errors.Join(&readBudgetDeferralError{err: refusal}, errors.New("upload failed"))); ok {
		t.Fatalf("joined with a failure deferred as %q", reason)
	}
	if reason, ok := deferralReason(refusal); ok {
		t.Fatalf("a refusal outside the source read deferred as %q", reason)
	}
}

func TestSizeLimitOfNamesEachLimit(t *testing.T) {
	t.Parallel()
	observed := sourceState{file: transcriptFileInfo{Size: 900}}
	for _, c := range []struct {
		err  error
		want state.SizeLimitGap
	}{
		{agentapi.RawLimit(700<<20, 512<<20, nil), state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: 512 << 20, SizeBytes: 700 << 20}},
		{checkFilteredSize(archive.FilteredTranscript{Boundary: archive.CaptureBoundary{RetainedBytes: 70}}, 64), state.SizeLimitGap{Kind: state.SizeLimitFiltered, LimitBytes: 64, SizeBytes: 70}},
		{errSessionReadCeiling, state.SizeLimitGap{Kind: state.SizeLimitReadBudget, LimitBytes: sessionReadCeiling, SizeBytes: 900}},
		{translateSourceError(agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)), state.SizeLimitGap{Kind: state.SizeLimitRecord, LimitBytes: recordLimit, SizeBytes: 900}},
	} {
		got := sizeLimitOf(c.err, observed, archive.HarnessCodex, DefaultMaxTranscriptBytes)
		if got == nil || *got != c.want {
			t.Errorf("%v: %+v, want %+v", c.err, got, c.want)
		}
	}
	if got := SizeLimitLabel(archive.HarnessCodex, state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: 512 << 20}); got != "Codex raw 512 MiB" {
		t.Fatal(got)
	}
}
