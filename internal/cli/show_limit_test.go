package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// showLimitTranscript is a Codex session of n exchanges: a prompt, a run of
// tool calls with long output, and a reply, so each of show's trimming steps
// has something to trim outside the protected tail of recent steps.
func showLimitTranscript(project string, n int) string {
	var b strings.Builder
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	line := func(payload string) {
		at = at.Add(time.Second)
		fmt.Fprintf(&b, `{"type":"response_item","timestamp":%q,"payload":%s}`+"\n", at.Format(time.RFC3339), payload)
	}
	fmt.Fprintf(&b, `{"type":"session_meta","timestamp":"2026-01-02T00:00:00Z","payload":{"id":"native-1","cwd":%q}}`+"\n", project)
	fmt.Fprintf(&b, `{"type":"turn_context","timestamp":"2026-01-02T00:00:01Z","payload":{"cwd":%q,"model":"gpt-test"}}`+"\n", project)
	for i := range n {
		line(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt-%d %s"}]}`, i, strings.Repeat("question ", 300)))
		for j := range 4 {
			line(fmt.Sprintf(`{"type":"function_call","name":"exec_command","call_id":"call_%d_%d","arguments":"{\"cmd\":\"go test -run T%d_%d\"}"}`, i, j, i, j))
			line(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d_%d","output":%q}`, i, j, strings.Repeat("ok ", 150)))
		}
		line(fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reply-%d %s"}]}`, i, strings.Repeat("answer ", 200)))
	}
	return b.String()
}

// newShowLimitFixture registers a Codex session through the hook path, fills
// its transcript with n exchanges, and syncs it, so `show` can read it back
// from the archive.
func newShowLimitFixture(t *testing.T, n int) handoffFixture {
	t.Helper()
	f := newHandoffFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.project, "codex.jsonl"), []byte(showLimitTranscript(f.project, n)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runSyncCommand(nil, &out, &errOut, f.env); code != 0 {
		t.Fatalf("sync code=%d stderr=%s", code, errOut.String())
	}
	return f
}

func runShow(t *testing.T, env Env, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"show"}, args...), nil, &out, &errOut, env)
	return out.String(), errOut.String(), code
}

func savedTranscriptPath(f handoffFixture, ext string) string {
	return filepath.Join(f.home, handoffDir, f.id+".transcript"+ext)
}

func TestShowTranscriptUnderTheLimitPrintsAllOfIt(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 2)
	for _, args := range [][]string{{"--transcript"}, {"--transcript", "--full"}, {"--transcript", "--max-bytes", "1000000"}} {
		out, errOut, code := runShow(t, f.env, append([]string{f.id}, args...)...)
		if code != 0 || errOut != "" {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut)
		}
		if strings.Contains(out, "Omitted to fit") || strings.Contains(out, "Full record") || !strings.Contains(out, "reply-1") {
			t.Fatalf("%v: trimmed or incomplete:\n%s", args, out)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home, handoffDir)); len(entries) != 0 {
		t.Fatalf("a full version was saved without trimming: %v", entries)
	}
}

// The default limit is handoff's, so an agent that runs `show --transcript`
// on a large session gets a bounded answer without asking for one.
func TestShowTranscriptDefaultLimitIsHandoffs(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 60)
	unlimited, _, code := runShow(t, f.env, f.id, "--transcript", "--max-bytes", "0")
	if code != 0 || len(unlimited) <= archive.DefaultHandoffMaxBytes {
		t.Fatalf("fixture is too small to test the default: code=%d, %d bytes", code, len(unlimited))
	}
	out, errOut, code := runShow(t, f.env, f.id, "--transcript")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if len(out) > archive.DefaultHandoffMaxBytes || !strings.Contains(out, "Omitted to fit the size limit") {
		t.Fatalf("default output is %d bytes, over %d or untrimmed", len(out), archive.DefaultHandoffMaxBytes)
	}
}

func TestShowTranscriptOverTheLimitIsTrimmedAndSavedInFull(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	full, _, code := runShow(t, f.env, f.id, "--transcript", "--max-bytes", "0")
	if code != 0 || strings.Contains(full, "Omitted to fit") {
		t.Fatalf("--max-bytes 0: code=%d\n%s", code, full)
	}
	if _, err := os.Stat(savedTranscriptPath(f, ".txt")); !os.IsNotExist(err) {
		t.Fatalf("--max-bytes 0 saved a copy: %v", err)
	}

	const limit = 20000
	out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--max-bytes", strconv.Itoa(limit))
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if len(out) > limit || len(out) >= len(full) {
		t.Fatalf("output is %d bytes; limit %d, untrimmed %d", len(out), limit, len(full))
	}
	saved := savedTranscriptPath(f, ".txt")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if !strings.HasPrefix(lines[len(lines)-2], "Omitted to fit the size limit: ") || lines[len(lines)-1] != "Full record: "+saved+" (kept for 7 days; read it for anything omitted here)." {
		t.Fatalf("output does not end by naming the omissions and %s:\n%s", saved, strings.Join(lines[len(lines)-3:], "\n"))
	}
	// The last exchange is the one the person is likely to continue from.
	if !strings.Contains(out, "reply-29") || !strings.Contains(out, "prompt-29") {
		t.Fatalf("the newest exchange was trimmed:\n%s", out)
	}
	info, err := os.Stat(saved)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved full version: %v %v", info, err)
	}
	body, _ := os.ReadFile(saved)
	if string(body) != full {
		t.Fatalf("the saved version is not the untrimmed transcript (%d bytes, want %d)", len(body), len(full))
	}
}

func TestShowTranscriptFullRespectsTheLimit(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	unlimited, _, _ := runShow(t, f.env, f.id, "--transcript", "--full", "--max-bytes", "0")
	brief, _, _ := runShow(t, f.env, f.id, "--transcript", "--max-bytes", "0")
	if len(unlimited) <= len(brief) {
		t.Fatalf("--full adds nothing to trim: %d vs %d", len(unlimited), len(brief))
	}
	// Dropping old results alone is enough here, so nothing else changes.
	limit := len(unlimited) - 4000
	out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--full", "--max-bytes", strconv.Itoa(limit))
	if code != 0 || errOut != "" || len(out) > limit {
		t.Fatalf("code=%d stderr=%s, %d bytes over limit %d", code, errOut, len(out), limit)
	}
	if !strings.Contains(out, "output of ") || !strings.Contains(out, "(output omitted)") || strings.Contains(out, "collapsed to counts") {
		t.Fatalf("expected only tool output to be dropped:\n%s", out[max(0, len(out)-600):])
	}
	// The tightest a --full transcript can get is still bounded.
	out, errOut, code = runShow(t, f.env, f.id, "--transcript", "--full", "--max-bytes", "9000")
	if code != 0 || errOut != "" || len(out) > 9000 {
		t.Fatalf("code=%d stderr=%s, %d bytes over 9000", code, errOut, len(out))
	}
	body, err := os.ReadFile(savedTranscriptPath(f, ".txt"))
	if err != nil || string(body) != unlimited {
		t.Fatalf("saved --full record is not the untrimmed --full transcript: %v", err)
	}
}

// Without --full no result is printed, so it is tool calls and text that go.
func TestShowTranscriptCollapsesToolCallsWithoutFull(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	unlimited, _, _ := runShow(t, f.env, f.id, "--transcript", "--max-bytes", "0")
	limit := len(unlimited) * 6 / 10
	out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--max-bytes", strconv.Itoa(limit))
	if code != 0 || errOut != "" || len(out) > limit {
		t.Fatalf("code=%d stderr=%s, %d bytes over limit %d", code, errOut, len(out), limit)
	}
	if !strings.Contains(out, "▸ 4 tool calls: exec_command ×4") || strings.Contains(out, "output of ") {
		t.Fatalf("expected collapsed tool calls and no dropped output:\n%s", out[max(0, len(out)-800):])
	}
}

// Past every other step the oldest exchanges go, so the output is bounded,
// and the newest is always kept.
func TestShowTranscriptDropsTheOldestExchangesLastAndWarnsWhenItCannotFit(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--max-bytes", "40000")
	if code != 0 || errOut != "" || len(out) > 40000 {
		t.Fatalf("code=%d stderr=%s, %d bytes", code, errOut, len(out))
	}
	if !strings.Contains(out, "long prompts in exchanges 1–30 truncated") || !strings.Contains(out, "…(truncated)") || !strings.Contains(out, "exchanges dropped") {
		t.Fatalf("expected truncated prompts then dropped exchanges:\n%s", out[max(0, len(out)-700):])
	}
	if strings.Contains(out, "┃ prompt-0 ") || !strings.Contains(out, "┃ prompt-29 ") || !strings.Contains(out, "reply-29") {
		t.Fatalf("the oldest exchange was kept or the newest was not:\n%s", out)
	}

	out, errOut, code = runShow(t, f.env, f.id, "--transcript", "--max-bytes", "500")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	want := fmt.Sprintf("agent-archive: show: warning: still %d bytes after trimming, over the 500-byte limit\n", len(out))
	if errOut != want || !strings.Contains(out, "the oldest 29 exchanges dropped") || !strings.Contains(out, "┃ prompt-29 ") {
		t.Fatalf("stderr = %q, want %q\n%s", errOut, want, out[max(0, len(out)-700):])
	}
}

func TestShowTranscriptZeroMeansNoLimit(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 60)
	out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--full", "--max-bytes", "0")
	if code != 0 || errOut != "" || len(out) <= archive.DefaultHandoffMaxBytes || strings.Contains(out, "Omitted to fit") {
		t.Fatalf("code=%d stderr=%s, %d bytes", code, errOut, len(out))
	}
}

func TestShowTranscriptJSONUnderTheLimitIsUnchanged(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 2)
	unlimited, errOut, code := runShow(t, f.env, f.id, "--transcript", "--json", "--max-bytes", "0")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	out, _, code := runShow(t, f.env, f.id, "--transcript", "--json")
	if code != 0 || out != unlimited || strings.Contains(out, `"trimmed"`) || strings.Contains(out, "text_truncated") {
		t.Fatalf("code=%d; output differs from the unlimited one or is marked trimmed:\n%s", code, out)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home, handoffDir)); len(entries) != 0 {
		t.Fatalf("a full version was saved without trimming: %v", entries)
	}
}

// decodeShowJSON reads the two documents `show --transcript --json` prints.
func decodeShowJSON(t *testing.T, data string) (archive.Metadata, normalizedOutput) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(data))
	var first archive.Metadata
	var second normalizedOutput
	if err := decoder.Decode(&first); err != nil {
		t.Fatalf("first document: %v", err)
	}
	if err := decoder.Decode(&second); err != nil {
		t.Fatalf("second document: %v", err)
	}
	if decoder.More() {
		t.Fatalf("more than two documents")
	}
	return first, second
}

func TestShowTranscriptJSONOverTheLimitIsTrimmedValidAndSavedInFull(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	unlimited, _, code := runShow(t, f.env, f.id, "--transcript", "--json", "--max-bytes", "0")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	_, all := decodeShowJSON(t, unlimited)
	if all.Trimmed != nil {
		t.Fatalf("unlimited output is marked trimmed")
	}
	saved := savedTranscriptPath(f, ".json")
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Fatalf("--max-bytes 0 saved a copy: %v", err)
	}

	// Each limit gets more trimming than the one before it, in order.
	var previous []trimKind
	for _, limit := range []int{len(unlimited) - 1000, len(unlimited) / 2, len(unlimited) / 4, len(unlimited) / 8} {
		out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--json", "--max-bytes", strconv.Itoa(limit))
		if code != 0 || errOut != "" || len(out) > limit {
			t.Fatalf("limit %d: code=%d stderr=%s, %d bytes", limit, code, errOut, len(out))
		}
		meta, view := decodeShowJSON(t, out)
		if meta.SessionID != f.id || view.Trimmed == nil {
			t.Fatalf("limit %d: not trimmed or wrong session: %+v", limit, view.Trimmed)
		}
		if view.Trimmed.MaxBytes != limit || view.Trimmed.FullRecord != saved || len(view.Trimmed.Omitted) == 0 {
			t.Fatalf("limit %d: trimmed = %+v", limit, view.Trimmed)
		}
		var kinds []trimKind
		for _, o := range view.Trimmed.Omitted {
			if o.Count == 0 {
				t.Fatalf("limit %d: empty omission %+v", limit, o)
			}
			kinds = append(kinds, o.Kind)
		}
		if len(kinds) < len(previous) {
			t.Fatalf("limit %d applied fewer steps (%v) than a looser limit (%v)", limit, kinds, previous)
		}
		previous = kinds
		// The full record is named at the end: the last field of the last document.
		if !strings.HasSuffix(strings.TrimSpace(out), `"full_record": "`+saved+"\"\n  }\n}") {
			t.Fatalf("limit %d: output does not end by naming %s:\n%s", limit, saved, out[max(0, len(out)-200):])
		}
	}
	body, err := os.ReadFile(saved)
	if err != nil || string(body) != unlimited {
		t.Fatalf("the saved version is not the untrimmed output: %v", err)
	}
	if info, err := os.Stat(saved); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved full version: %v %v", info, err)
	}
	want := []trimKind{trimToolResults, trimToolInput, trimAssistantText, trimPromptText, trimOldestRecords}
	if fmt.Sprint(previous) != fmt.Sprint(want[:len(previous)]) {
		t.Fatalf("steps applied out of order: %v", previous)
	}
}

func TestShowTranscriptJSONKeepsTheNewestRecordsWhenItHasToDropSome(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	unlimited, _, _ := runShow(t, f.env, f.id, "--transcript", "--json", "--max-bytes", "0")
	_, all := decodeShowJSON(t, unlimited)
	sidecar := strings.Index(unlimited, "\n}\n") + 3
	limit := sidecar + 6000
	out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--json", "--max-bytes", strconv.Itoa(limit))
	if code != 0 || errOut != "" || len(out) > limit {
		t.Fatalf("code=%d stderr=%s, %d bytes over %d", code, errOut, len(out), limit)
	}
	_, view := decodeShowJSON(t, out)
	if len(view.Turns) == 0 || len(view.Turns) >= len(all.Turns) {
		t.Fatalf("kept %d of %d turns", len(view.Turns), len(all.Turns))
	}
	if view.Turns[len(view.Turns)-1].RecordIndex != all.Turns[len(all.Turns)-1].RecordIndex {
		t.Fatalf("the newest turn was dropped")
	}
	if last := view.Trimmed.Omitted[len(view.Trimmed.Omitted)-1]; last.Kind != trimOldestRecords || last.Count == 0 {
		t.Fatalf("omitted = %+v", view.Trimmed.Omitted)
	}
	// What is kept is the newest suffix, in order, untouched.
	for i, turn := range view.Turns {
		want := all.Turns[len(all.Turns)-len(view.Turns)+i]
		if turn.RecordIndex != want.RecordIndex || turn.Role != want.Role {
			t.Fatalf("turn %d = %+v, want %+v", i, turn, want)
		}
	}
}

func TestShowTranscriptJSONWarnsWhenItCannotFit(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--json", "--max-bytes", "10")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	_, view := decodeShowJSON(t, out)
	if len(view.Turns) != 0 || view.Trimmed == nil {
		t.Fatalf("expected every record dropped: %d turns, trimmed=%+v", len(view.Turns), view.Trimmed)
	}
	if want := fmt.Sprintf("agent-archive: show: warning: still %d bytes after trimming, over the 10-byte limit\n", len(out)); errOut != want {
		t.Fatalf("stderr = %q, want %q", errOut, want)
	}
}

func TestShowTranscriptNormalizedAlsoHonorsTheLimit(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	out, _, code := runShow(t, f.env, f.id, "--normalized", "--max-bytes", "15000")
	_, view := decodeShowJSON(t, out)
	if code != 0 || len(out) > 15000 || view.Trimmed == nil {
		t.Fatalf("code=%d, %d bytes, trimmed=%+v", code, len(out), view.Trimmed)
	}
}

func TestShowMaxBytesIsValidated(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 1)
	for args, want := range map[string]string{
		"--transcript --max-bytes -1":        "--max-bytes must be 0 or more",
		"--max-bytes 100":                    "--max-bytes needs --transcript",
		"--json --max-bytes 0":               "--max-bytes needs --transcript",
		"--transcript --max-bytes lots":      "--max-bytes",
		"--transcript --full --json":         "--full is for the readable transcript",
		"--transcript --max-bytes 10 --full": "",
	} {
		out, errOut, code := runShow(t, f.env, append([]string{f.id}, strings.Fields(args)...)...)
		if want == "" {
			if code != 0 || errOut == "" && out == "" {
				t.Errorf("%s: code=%d stderr=%s", args, code, errOut)
			}
			continue
		}
		if code != 2 || !strings.Contains(errOut, want) || out != "" {
			t.Errorf("%s: code=%d stderr=%q stdout=%q", args, code, errOut, out)
		}
	}
}

// Saved transcripts share handoffs' seven days: older ones are removed
// whenever a transcript is shown, and newer ones stay.
func TestShowTranscriptPrunesSavedTranscriptsAfterSevenDays(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	if _, _, code := runShow(t, f.env, f.id, "--transcript", "--max-bytes", "20000"); code != 0 {
		t.Fatal("show failed")
	}
	saved := savedTranscriptPath(f, ".txt")
	other := filepath.Join(f.home, handoffDir, "other.transcript.json")
	if err := os.WriteFile(other, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	age := func(path string, by time.Duration) {
		when := f.env.now().Add(-by)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	age(saved, 6*24*time.Hour+23*time.Hour)
	age(other, 7*24*time.Hour+time.Minute)
	// An untrimmed show prunes too.
	if _, _, code := runShow(t, f.env, f.id, "--transcript", "--max-bytes", "0"); code != 0 {
		t.Fatal("show failed")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("a transcript saved more than seven days ago was kept: %v", err)
	}
	if _, err := os.Stat(saved); err != nil {
		t.Fatalf("a transcript saved less than seven days ago was removed: %v", err)
	}
	age(saved, 8*24*time.Hour)
	if _, _, code := runShow(t, f.env, f.id, "--transcript", "--json", "--max-bytes", "0"); code != 0 {
		t.Fatal("show failed")
	}
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Fatalf("an eight-day-old transcript was kept: %v", err)
	}
}

// The text and JSON records of one session sit side by side, and neither
// replaces a handoff's saved record.
func TestShowTranscriptRecordsDoNotCollideWithHandoffs(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	handoffPath := filepath.Join(f.home, handoffDir, f.id+".md")
	if err := os.MkdirAll(filepath.Dir(handoffPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoffPath, []byte("handoff record"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--transcript"}, {"--transcript", "--json"}} {
		if _, _, code := runShow(t, f.env, append([]string{f.id}, append(args, "--max-bytes", "20000")...)...); code != 0 {
			t.Fatalf("%v failed", args)
		}
	}
	for _, ext := range []string{".txt", ".json"} {
		if _, err := os.Stat(savedTranscriptPath(f, ext)); err != nil {
			t.Fatalf("no saved %s record: %v", ext, err)
		}
	}
	if body, _ := os.ReadFile(handoffPath); string(body) != "handoff record" {
		t.Fatalf("show replaced the handoff's record: %q", body)
	}
}

// When the copy cannot be saved the output says so, rather than naming a file
// that is not there.
func TestShowTranscriptSaysSoWhenTheFullVersionCannotBeSaved(t *testing.T) {
	t.Parallel()
	f := newShowLimitFixture(t, 30)
	// A file where the folder should be.
	if err := os.WriteFile(filepath.Join(f.home, handoffDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runShow(t, f.env, f.id, "--transcript", "--max-bytes", "20000")
	if code != 0 || len(out) > 20000 {
		t.Fatalf("code=%d, %d bytes", code, len(out))
	}
	if !strings.Contains(errOut, "could not save the untrimmed transcript") || strings.Contains(out, "Full record") || !strings.HasSuffix(out, "Run again with --max-bytes 0 to print all of it.\n") {
		t.Fatalf("stderr=%q\n%s", errOut, out[max(0, len(out)-300):])
	}
	out, errOut, code = runShow(t, f.env, f.id, "--transcript", "--json", "--max-bytes", "20000")
	_, view := decodeShowJSON(t, out)
	if code != 0 || !strings.Contains(errOut, "could not save the untrimmed transcript") || view.Trimmed == nil || view.Trimmed.FullRecord != "" {
		t.Fatalf("code=%d stderr=%q trimmed=%+v", code, errOut, view.Trimmed)
	}
}

// A terminal's colors and width belong to the display only: the limit counts
// what is printed, and the saved copy is plain.
func TestFitTranscriptToLimitMeasuresWhatIsPrintedAndSavesPlainText(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	transcript := transcriptFixture(t)
	bundle := archive.SourceBundle{ArchiveSessionID: view.SessionID}
	home := t.TempDir()
	style := textStyle{color: true, width: 80}
	opts := transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC, Style: style}, Full: true}
	plain := opts
	plain.Style = textStyle{}
	full := renderTranscriptBytes(view, transcript, plain)
	limit := len(renderTranscriptBytes(view, transcript, opts)) - 40
	var stderr bytes.Buffer
	fitted, shown := fitTranscriptToLimit(view, transcript, bundle, opts, limit, home, &stderr)
	if len(fitted.Elisions) == 0 || stderr.Len() != 0 {
		t.Fatalf("expected trimming, stderr=%q, %d elisions, %d vs limit %d", stderr.String(), len(fitted.Elisions), len(renderTranscriptBytes(view, fitted, shown)), limit)
	}
	if got := renderTranscriptBytes(view, fitted, shown); len(got) > limit || !bytes.Contains(got, []byte("\x1b[")) {
		t.Fatalf("printed %d bytes over limit %d, or lost its color", len(got), limit)
	}
	body, err := os.ReadFile(shown.FullRecord)
	if err != nil || !bytes.Equal(body, full) || bytes.Contains(body, []byte("\x1b")) {
		t.Fatalf("saved copy is not the plain, untrimmed transcript: %v", err)
	}
}

// The limit counts the footer that names the saved file, so output never
// runs past it by that footer's length, whichever exchange it lands on.
func TestFitTranscriptToLimitCountsTheFooterItPrints(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	var transcript archive.Transcript
	for i := range 40 {
		exchange := archive.TranscriptExchange{Kind: archive.TranscriptExchangePrompt, Text: fmt.Sprintf("prompt %d", i)}
		exchange.Steps = append(exchange.Steps, archive.TranscriptStep{Kind: archive.TranscriptStepText, Text: fmt.Sprintf("reply %d %s", i, strings.Repeat("word ", 40))})
		for j := range 3 {
			exchange.Steps = append(exchange.Steps, archive.TranscriptStep{Kind: archive.TranscriptStepTool, Tool: &archive.HandoffToolCall{Name: "Bash", Summary: fmt.Sprintf("run %d", j)}})
		}
		transcript.Exchanges = append(transcript.Exchanges, exchange)
	}
	opts := transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC}}
	untrimmed := len(renderTranscriptBytes(view, transcript, opts))
	bundle := archive.SourceBundle{ArchiveSessionID: view.SessionID}
	home := t.TempDir()
	trimmed := 0
	for limit := untrimmed / 2; limit < untrimmed/2+150; limit += 3 {
		var stderr bytes.Buffer
		fitted, shown := fitTranscriptToLimit(view, transcript, bundle, opts, limit, home, &stderr)
		printed := renderTranscriptBytes(view, fitted, shown)
		if len(printed) > limit || stderr.Len() != 0 {
			t.Fatalf("limit %d: printed %d bytes, stderr=%q", limit, len(printed), stderr.String())
		}
		if len(fitted.Elisions) > 0 {
			trimmed++
		}
	}
	if trimmed == 0 {
		t.Fatalf("nothing was trimmed, so nothing was tested (untrimmed %d bytes)", untrimmed)
	}
}
