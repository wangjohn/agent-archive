package archive

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var textStart = time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)

func gapsWithCode(gaps []CaptureGap, code string) int {
	n := 0
	for _, gap := range gaps {
		if gap.Code == code {
			n++
		}
	}
	return n
}

// cursorTextSession builds a synthetic Cursor text transcript of exchanges
// user/assistant/tool sections, each assistant body padded to bodyBytes over
// two lines. Every tenth tool section carries a synthetic credential.
func cursorTextSession(exchanges, bodyBytes int) string {
	var b strings.Builder
	for i := 0; i < exchanges; i++ {
		fmt.Fprintf(&b, "user: request %d\n", i)
		fmt.Fprintf(&b, "assistant: answer %d begins\n%s\n", i, strings.Repeat("a", bodyBytes))
		if i%10 == 0 {
			fmt.Fprintf(&b, "tool: output %d token=synthetic-secret-%d\n", i, i)
		} else {
			fmt.Fprintf(&b, "tool: output %d\n", i)
		}
	}
	return b.String()
}

// Filter 6 sanitized the joined transcript as one string and truncated it to
// 64 KB. A 5 MB transcript is now retained whole, with redaction applied to
// the sections that need it and no truncation.
func TestCursorTextRetainsAFiveMegabyteTranscriptInFull(t *testing.T) {
	const exchanges = 2600
	input := cursorTextSession(exchanges, 2000)
	if len(input) < 5<<20 {
		t.Fatalf("test transcript is %d bytes", len(input))
	}
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(input), textStart)
	if err != nil {
		t.Fatal(err)
	}
	text := filtered.Text[0]
	if gapsWithCode(filtered.Gaps, "content_truncated") != 0 {
		t.Fatal("a transcript of small sections was truncated")
	}
	if strings.Contains(text, "synthetic-secret") || gapsWithCode(filtered.Gaps, "sensitive_content_redacted") != 1 {
		t.Fatalf("redaction: leaked=%t gaps=%d", strings.Contains(text, "synthetic-secret"), gapsWithCode(filtered.Gaps, "sensitive_content_redacted"))
	}
	if !strings.Contains(text, fmt.Sprintf("user: request %d\n", exchanges-1)) || !strings.HasSuffix(text, fmt.Sprintf("tool: output %d", exchanges-1)) {
		t.Fatal("the end of the transcript was lost")
	}
	if got, want := strings.Count(text, "\nassistant: answer "), exchanges; got != want {
		t.Fatalf("%d assistant sections retained, want %d", got, want)
	}
	// Apart from the redactions, the text is the input's own lines.
	if len(text) < len(input)-exchanges*40 {
		t.Fatalf("retained %d of %d bytes", len(text), len(input))
	}
}

// The 64 KB cap now applies per section: an oversize section is truncated and
// reported, and the sections around it are intact and in order.
func TestCursorTextTruncatesOnlyTheOversizeSection(t *testing.T) {
	big := "assistant: long answer\n" + strings.Repeat("b", 100*1024)
	input := "user: first\n" + big + "\ntool: after\nuser: second\nassistant: done\n"
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(input), textStart)
	if err != nil {
		t.Fatal(err)
	}
	if gapsWithCode(filtered.Gaps, "content_truncated") != 1 {
		t.Fatalf("gaps = %#v", filtered.Gaps)
	}
	want := "user: first\n" + big[:64*1024] + "\ntool: after\nuser: second\nassistant: done"
	if filtered.Text[0] != want {
		t.Fatalf("retained text differs: got %d bytes, want %d", len(filtered.Text[0]), len(want))
	}
}

// Sections keep their order and their line structure exactly; hidden sections
// and their continuation lines are omitted, and the gap is recorded once
// however many hidden sections there were.
func TestCursorTextKeepsLineStructureAndOmitsHiddenSections(t *testing.T) {
	input := "User: hello\nsecond line\nSystem: hidden one\nhidden continuation\nAssistant: ok\n  indented line\nthinking: hidden two\nTool: result\n"
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(input), textStart)
	if err != nil {
		t.Fatal(err)
	}
	if want := "User: hello\nsecond line\nAssistant: ok\n  indented line\nTool: result"; filtered.Text[0] != want {
		t.Fatalf("text = %q, want %q", filtered.Text[0], want)
	}
	if gapsWithCode(filtered.Gaps, "hidden_instruction_omitted") != 1 {
		t.Fatalf("gaps = %#v", filtered.Gaps)
	}
}

// The transcript is bounded by the record size limit, not 2 MB, and over it
// fails with ErrRecordTooLarge, which the collector records as a gap.
func TestCursorTextOverTheRecordLimit(t *testing.T) {
	withRecordLimit(t, 4096)
	fits := "user: hi\nassistant: " + strings.Repeat("c", 4096-len("user: hi\nassistant: "))
	if _, err := (CursorAdapter{}).FilterText(strings.NewReader(fits), textStart); err != nil {
		t.Fatalf("a transcript of exactly the limit was refused: %v", err)
	}
	if _, err := (CursorAdapter{}).FilterText(strings.NewReader(fits+"c"), textStart); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("err = %v, want ErrRecordTooLarge", err)
	}
}

// The handoff reads a text transcript back by its section prefixes. Under
// filter 6 a transcript over 64 KB lost everything after its first 64 KB, so
// a handoff could not say where the session left off.
func TestHandoffOfALargeCursorTextTranscriptReachesTheEnd(t *testing.T) {
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(cursorTextSession(200, 1000)), textStart)
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, textStart.Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Exchanges) != 200 || h.Exchanges[199].Prompt != "request 199" {
		t.Fatalf("exchanges = %d", len(h.Exchanges))
	}
	if !strings.HasPrefix(h.LeftOff, "answer 199 begins") {
		t.Fatalf("left off = %.40q", h.LeftOff)
	}
}

// Sanitizing applies to every retained line of a section, not only its role
// line: a credential on a continuation line is redacted, and an injected
// instruction block that spans continuation lines is stripped, as it is from
// JSONL string content. Hidden sections never leak: one that directly follows
// a visible section, one whose prefix varies in case or leading whitespace,
// and one that is the last section, with all of their continuation lines.
// Recording each gap once does not hide distinct gaps: the two
// hidden_instruction_omitted details, the redaction, and the structure gap
// are all reported.
func TestCursorTextSanitizesContinuationLinesAndOmitsHiddenTail(t *testing.T) {
	input := strings.Join([]string{
		"user: hello",
		"  token=synthetic-continuation-secret",
		"SYSTEM: hidden one",
		"hidden continuation one",
		"assistant: ok <system-reminder>",
		"injected line",
		"</system-reminder> visible tail",
		"tool: result",
		"  Thinking: hidden two",
		"hidden continuation two",
	}, "\n") + "\n"
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(input), textStart)
	if err != nil {
		t.Fatal(err)
	}
	text := filtered.Text[0]
	if want := "user: hello\n  token=[REDACTED]\nassistant: ok  visible tail\ntool: result"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	for _, leaked := range []string{"hidden", "injected", "synthetic-continuation-secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("retained text contains %q", leaked)
		}
	}
	details := map[string]bool{}
	for _, gap := range filtered.Gaps {
		if gap.Code == "hidden_instruction_omitted" {
			details[gap.Detail] = true
		}
	}
	if len(details) != 2 || !details["text section omitted"] || !details["injected instruction block omitted"] {
		t.Fatalf("hidden_instruction_omitted details = %v", details)
	}
	if gapsWithCode(filtered.Gaps, "sensitive_content_redacted") != 1 || gapsWithCode(filtered.Gaps, "text_structure_partial") != 1 {
		t.Fatalf("gaps = %#v", filtered.Gaps)
	}
}

// An injected block that never closes drops the rest of its own section, as
// it does for a JSONL string, and no more: the sections after it are kept.
func TestCursorTextUnclosedInjectedBlockDropsOnlyItsSection(t *testing.T) {
	input := "user: a\nassistant: b <system-reminder>\nnever closed\nuser: c\nassistant: d\n"
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(input), textStart)
	if err != nil {
		t.Fatal(err)
	}
	if want := "user: a\nassistant: b\nuser: c\nassistant: d"; filtered.Text[0] != want {
		t.Fatalf("text = %q, want %q", filtered.Text[0], want)
	}
}

// A transcript of only hidden sections retains nothing and is refused.
func TestCursorTextOnlyHiddenSectionsIsNotRetainable(t *testing.T) {
	_, err := (CursorAdapter{}).FilterText(strings.NewReader("system: secret\nmore secret\n"), textStart)
	var ferr *FilterError
	if !errors.As(err, &ferr) {
		t.Fatalf("err = %v, want a FilterError", err)
	}
}

func BenchmarkCursorTextFilterFiveMegabytes(b *testing.B) {
	input := cursorTextSession(2600, 2000)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := (CursorAdapter{}).FilterText(strings.NewReader(input), textStart); err != nil {
			b.Fatal(err)
		}
	}
}
