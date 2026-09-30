package trace

import (
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// Tests in this package share the process-wide recorder, so none of them
// runs in parallel.

// tree renders the recording as "indent+label" lines: the offset and
// timing columns dropped, the indentation kept exactly.
func tree(t *testing.T) []string {
	t.Helper()
	var b strings.Builder
	Write(&b)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "agent-archive trace") {
		t.Fatalf("no trace header:\n%s", b.String())
	}
	out := make([]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		// "%9s  " is the offset column; the label ends at the two spaces
		// before its timing.
		rest := line[11:]
		indent := rest[:len(rest)-len(strings.TrimLeft(rest, " "))]
		label, _, _ := strings.Cut(strings.TrimLeft(rest, " "), "  ")
		out = append(out, indent+label)
	}
	return out
}

func sameLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("trace tree:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDisabledRecordsNothing(t *testing.T) {
	span := Start("never")
	span.Count("n", 1)
	span.Child("child").End()
	span.End()
	if span != nil || StartLeaf("never") != nil || Enabled() {
		t.Fatal("a span was recorded while tracing was off")
	}
	var b strings.Builder
	Write(&b)
	if b.Len() != 0 {
		t.Fatalf("Write with tracing off wrote %q", b.String())
	}
}

// Spans made with Start nest under the innermost span that contains them in
// time, and only that span; spans made with Child stay under their parent;
// same-named siblings fold into one line with their summed counts.
func TestWriteNestsFoldsAndCounts(t *testing.T) {
	disable := Enable()
	defer disable()
	root := Start("handoff")
	listing := Start("list metadata")
	listing.Count("sidecars", 662)
	for range 3 {
		r := listing.Child("range")
		r.Count("keys", 700)
		time.Sleep(time.Millisecond)
		r.End()
	}
	request := StartLeaf("request list")
	time.Sleep(time.Millisecond)
	request.End()
	listing.End()
	after := Start("local activity")
	time.Sleep(time.Millisecond)
	after.End()
	root.End()

	sameLines(t, tree(t), []string{
		"handoff",
		"  list metadata",
		"    range ×3",
		"    request list",
		"  local activity",
	})
	var b strings.Builder
	Write(&b)
	if !strings.Contains(b.String(), "keys 2100") || !strings.Contains(b.String(), "sidecars 662") {
		t.Fatalf("counts missing:\n%s", b.String())
	}
}

// Requests running at once overlap, and one that starts later can end
// sooner. As leaves they stay side by side and fold, instead of nesting
// inside one another.
func TestOverlappingLeavesFoldInsteadOfNesting(t *testing.T) {
	disable := Enable()
	defer disable()
	reads := Start("read sidecars")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		request := StartLeaf("request get")
		go func() {
			defer wg.Done()
			// Later requests finish first.
			time.Sleep(time.Duration(16-i) * time.Millisecond)
			request.Count("bytes", 10)
			request.End()
		}()
	}
	wg.Wait()
	reads.End()
	sameLines(t, tree(t), []string{"read sidecars", "  request get ×8"})
}

// A span never ended (a response body nobody closed) is placed by where it
// started, marked unfinished, and swallows nothing that follows it.
func TestUnfinishedSpanHoldsNothing(t *testing.T) {
	disable := Enable()
	defer disable()
	root := Start("list")
	Start("request get") // never ended
	time.Sleep(time.Millisecond)
	later := Start("read sidecars")
	time.Sleep(time.Millisecond)
	later.End()
	root.End()
	sameLines(t, tree(t), []string{"list", "  request get", "  read sidecars"})
	var b strings.Builder
	Write(&b)
	if !strings.Contains(b.String(), "(unfinished)") {
		t.Fatalf("an unended span is not marked:\n%s", b.String())
	}
}

func TestEndTwiceKeepsTheFirstEnd(t *testing.T) {
	disable := Enable()
	defer disable()
	span := Start("once")
	span.End()
	first := span.end
	time.Sleep(time.Millisecond)
	span.End()
	if !span.end.Equal(first) {
		t.Fatal("a second End moved the span's end")
	}
}

func TestDisableStopsOnlyItsOwnRecording(t *testing.T) {
	disableFirst := Enable()
	disableSecond := Enable()
	disableFirst()
	if !Enabled() {
		t.Fatal("disabling an earlier recording stopped the current one")
	}
	disableSecond()
	if Enabled() {
		t.Fatal("tracing still on after the current recording was disabled")
	}
}
