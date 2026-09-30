package trace

import (
	"strings"
	"testing"
	"time"
)

// Tests in this package share the process-wide recorder, so none of them
// runs in parallel.

func TestDisabledRecordsNothing(t *testing.T) {
	span := Start("never")
	span.Count("n", 1)
	span.Child("child").End()
	span.End()
	if span != nil || Enabled() {
		t.Fatal("a span was recorded while tracing was off")
	}
	var b strings.Builder
	Write(&b)
	if b.Len() != 0 {
		t.Fatalf("Write with tracing off wrote %q", b.String())
	}
}

// Spans made with Start nest under the innermost span that contains them in
// time; spans made with Child stay under their parent; same-named siblings
// fold into one line with their summed counts.
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
	request := Start("request list")
	time.Sleep(time.Millisecond)
	request.End()
	listing.End()
	after := Start("local activity")
	after.End()
	root.End()

	var b strings.Builder
	Write(&b)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	want := []struct {
		indent string
		text   string
	}{
		{"", "handoff"},
		{"  ", "list metadata"},
		{"    ", "range ×3"},
		{"    ", "request list"},
		{"  ", "local activity"},
	}
	if len(lines) != len(want)+1 {
		t.Fatalf("trace has %d lines, want %d:\n%s", len(lines), len(want)+1, b.String())
	}
	for i, w := range want {
		if line := lines[i+1]; !strings.Contains(line, "  "+w.indent+w.text+"  ") {
			t.Errorf("line %d = %q, want %q indented %d", i+1, line, w.text, len(w.indent))
		}
	}
	if !strings.Contains(b.String(), "range ×3") || !strings.Contains(b.String(), "keys 2100") || !strings.Contains(b.String(), "sidecars 662") {
		t.Fatalf("counts or folding missing:\n%s", b.String())
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
