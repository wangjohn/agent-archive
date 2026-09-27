package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStyleRolesColorOnlyWhenAllowed(t *testing.T) {
	t.Parallel()
	roles := []struct {
		name  string
		apply func(textStyle, string) string
		code  string
	}{
		{"ok", textStyle.ok, "32"},
		{"warn", textStyle.warn, "33"},
		{"fail", textStyle.fail, "31"},
		{"cmd", textStyle.cmd, "36"},
		{"dim", textStyle.dim, "2"},
		{"bold", textStyle.bold, "1"},
	}
	for _, role := range roles {
		if got, want := role.apply(textStyle{color: true}, "text"), "\x1b["+role.code+"mtext\x1b[0m"; got != want {
			t.Errorf("%s with color = %q, want %q", role.name, got, want)
		}
		if got := role.apply(textStyle{}, "text"); got != "text" {
			t.Errorf("%s without color = %q, want plain text", role.name, got)
		}
		if got := role.apply(textStyle{color: true}, ""); got != "" {
			t.Errorf("%s of empty text = %q, want nothing", role.name, got)
		}
	}
}

func TestStatusMarksAreTheirSymbols(t *testing.T) {
	t.Parallel()
	color, plain := textStyle{color: true}, textStyle{}
	marks := []struct {
		name     string
		color    string
		plain    string
		want     string
		wantCode string
	}{
		{"ok", color.okMark(), plain.okMark(), "✓", "32"},
		{"warn", color.warnMark(), plain.warnMark(), "!", "33"},
		{"fail", color.failMark(), plain.failMark(), "✗", "31"},
	}
	for _, mark := range marks {
		if mark.plain != mark.want {
			t.Errorf("%s mark without color = %q, want %q", mark.name, mark.plain, mark.want)
		}
		if want := "\x1b[" + mark.wantCode + "m" + mark.want + "\x1b[0m"; mark.color != want {
			t.Errorf("%s mark with color = %q, want %q", mark.name, mark.color, want)
		}
	}
}

func TestTerminalStyleHonorsNoColorAndDumbTerminals(t *testing.T) {
	t.Parallel()
	env := func(vars map[string]string) func(string) string {
		return func(key string) string { return vars[key] }
	}
	cases := []struct {
		name string
		vars map[string]string
		want textStyle
	}{
		{"color terminal", map[string]string{"TERM": "xterm-256color"}, textStyle{color: true, live: true, width: 80}},
		{"NO_COLOR", map[string]string{"TERM": "xterm-256color", "NO_COLOR": "1"}, textStyle{live: true, width: 80}},
		{"TERM=dumb", map[string]string{"TERM": "dumb"}, textStyle{width: 80}},
	}
	for _, c := range cases {
		if got := terminalStyle(env(c.vars), 80); got != c.want {
			t.Errorf("%s: style = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestStyleForIsPlainWhenOutputIsNotATerminal(t *testing.T) {
	t.Parallel()
	if got := styleFor(&bytes.Buffer{}); got != (textStyle{}) {
		t.Errorf("style for a buffer = %+v, want plain", got)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if got := styleFor(file); got != (textStyle{}) {
		t.Errorf("style for a regular file = %+v, want plain", got)
	}
}

func TestHangingIndentWrapsUnderTheText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prefix string
		text   string
		width  int
		want   string
	}{
		{
			"fits on one line",
			"  ! ", "Bucket is private.", 40,
			"  ! Bucket is private.",
		},
		{
			"wraps under the text, not the prefix",
			"  ! ", "The bucket looks public. Fix its access before archiving.", 30,
			"  ! The bucket looks public.\n    Fix its access before\n    archiving.",
		},
		{
			"a word exactly filling the line stays on it",
			"- ", "aaaa bbbb", 6,
			"- aaaa\n  bbbb",
		},
		{
			"a word longer than the line is never split",
			"  ! ", "Open https://example.com/a/very/long/link/that/does/not/fit now", 20,
			"  ! Open\n    https://example.com/a/very/long/link/that/does/not/fit\n    now",
		},
		{
			"line breaks are kept and indented",
			"  ✓ ", "Connected.\nsecond line", 40,
			"  ✓ Connected.\n    second line",
		},
		{
			"no width means no wrapping",
			"  ! ", "a long line that would wrap\nnext", 0,
			"  ! a long line that would wrap\n    next",
		},
		{
			"color codes take no columns",
			"  \x1b[33m!\x1b[0m ", "aaaa \x1b[36mbbbb\x1b[0m cccc", 13,
			"  \x1b[33m!\x1b[0m aaaa \x1b[36mbbbb\x1b[0m\n    cccc",
		},
	}
	for _, c := range cases {
		if got := hangingIndent(c.prefix, c.text, c.width); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		if c.width > 0 {
			for line := range strings.SplitSeq(hangingIndent(c.prefix, c.text, c.width), "\n") {
				if visibleWidth(line) > c.width && !strings.Contains(line, "https://") {
					t.Errorf("%s: line %q is wider than %d columns", c.name, line, c.width)
				}
			}
		}
	}
	if got, want := (textStyle{width: 30}).hang("  ! ", "The bucket looks public. Fix it."), "  ! The bucket looks public.\n    Fix it."; got != want {
		t.Errorf("hang = %q, want %q", got, want)
	}
}

func TestVisibleWidthCountsColumnsNotBytes(t *testing.T) {
	t.Parallel()
	if got := visibleWidth("✓ ok"); got != 4 {
		t.Errorf("visibleWidth(✓ ok) = %d, want 4", got)
	}
	if got := visibleWidth("\x1b[32m✓\x1b[0m ok"); got != 4 {
		t.Errorf("visibleWidth of colored ✓ ok = %d, want 4", got)
	}
}

// notifyingWriter records writes and closes wrote once it has seen n.
type notifyingWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	n     int
	wrote chan struct{}
}

func (w *notifyingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n--
	if w.n == 0 {
		close(w.wrote)
	}
	return w.buf.Write(p)
}

func (w *notifyingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestSpinnerPrintsNothingWhereItCannotRedraw(t *testing.T) {
	t.Parallel()
	for _, style := range []textStyle{{}, {color: true, width: 80}} {
		var out bytes.Buffer
		sp := style.spin(&out, "Checking storage")
		time.Sleep(10 * time.Millisecond)
		sp.stop()
		sp.stop()
		if out.Len() != 0 {
			t.Errorf("spinner with style %+v wrote %q, want nothing", style, out.String())
		}
	}
}

func TestSpinnerAnimatesThenClearsItsLine(t *testing.T) {
	t.Parallel()
	out := &notifyingWriter{n: 3, wrote: make(chan struct{})}
	sp := textStyle{live: true}.spinEvery(out, "Checking storage", time.Millisecond)
	<-out.wrote
	sp.stop()
	sp.stop()
	written := out.String()
	if !strings.HasPrefix(written, "\r"+spinnerFrames[0]+" Checking storage\r"+spinnerFrames[1]+" Checking storage") {
		t.Errorf("spinner wrote %q, want successive frames on one line", written)
	}
	if !strings.HasSuffix(written, "\r\x1b[K") {
		t.Errorf("spinner wrote %q, want its line cleared when stopped", written)
	}
	if strings.Contains(written, "\n") {
		t.Errorf("spinner wrote %q, want no line breaks", written)
	}
	// Nothing is written after stop returns.
	settled := out.String()
	time.Sleep(5 * time.Millisecond)
	if out.String() != settled {
		t.Error("spinner wrote after stop returned")
	}
}

func TestSpinnerLabelFitsTheTerminal(t *testing.T) {
	t.Parallel()
	out := &notifyingWriter{n: 1, wrote: make(chan struct{})}
	sp := textStyle{live: true, width: 12}.spinEvery(out, "Checking the storage bucket", time.Hour)
	<-out.wrote
	sp.stop()
	first := strings.Split(strings.TrimPrefix(out.String(), "\r"), "\r")[0]
	if visibleWidth(first) >= 12 {
		t.Errorf("spinner line %q is %d columns, want under 12", first, visibleWidth(first))
	}
}

func TestDisplayPathShowsHomeAsTilde(t *testing.T) {
	t.Parallel()
	home := filepath.FromSlash("/Users/someone")
	cases := []struct {
		path string
		home string
		want string
	}{
		{"/Users/someone/code/app", home, "~/code/app"},
		{"/Users/someone", home, "~"},
		{"/Users/someone/", home, "~"},
		{"/Users/someoneelse/app", home, "/Users/someoneelse/app"},
		{"/Users/someone/..hidden", home, "~/..hidden"},
		{"/Users", home, "/Users"},
		{"/opt/app", home, "/opt/app"},
		{"/Users/someone/code", "", "/Users/someone/code"},
	}
	for _, c := range cases {
		if got, want := displayPath(filepath.FromSlash(c.path), c.home), filepath.FromSlash(c.want); got != want {
			t.Errorf("displayPath(%q, %q) = %q, want %q", c.path, c.home, got, want)
		}
	}
}

func TestHangingIndentKeepsSpacingItDoesNotBreakAt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		text  string
		width int
		want  string
	}{
		{"aligned columns", "bucket    my-bucket", 40, "- bucket    my-bucket"},
		{"a paragraph's own indent", "Open:\n    https://example.com", 40, "- Open:\n      https://example.com"},
		{"spacing at a break is dropped", "aaaa    bbbb", 8, "- aaaa\n  bbbb"},
	}
	for _, c := range cases {
		if got := hangingIndent("- ", c.text, c.width); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		// The same text unwrapped keeps its spacing too.
		if got, want := hangingIndent("- ", c.text, 0), "- "+strings.ReplaceAll(c.text, "\n", "\n  "); got != want {
			t.Errorf("%s unwrapped: got %q, want %q", c.name, got, want)
		}
	}
}

func TestVisibleWidthCountsWideCharactersAsTwoColumns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want int
	}{
		{"~/项目/app", 10},
		{"· ✗ !", 5},
		{"é", 1},
	}
	for _, c := range cases {
		if got := visibleWidth(c.text); got != c.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", c.text, got, c.want)
		}
	}
	if got, want := hangingIndent("- ", "项目项目 项目项目", 11), "- 项目项目\n  项目项目"; got != want {
		t.Errorf("wrapping wide text = %q, want %q", got, want)
	}
}

func TestSpinnerLabelCutKeepsColorCodesWhole(t *testing.T) {
	t.Parallel()
	label := "Checking \x1b[36maws s3 ls s3://bucket\x1b[0m"
	out := &notifyingWriter{n: 1, wrote: make(chan struct{})}
	sp := textStyle{live: true, width: 16}.spinEvery(out, label, time.Hour)
	<-out.wrote
	sp.stop()
	first := strings.Split(strings.TrimPrefix(out.String(), "\r"), "\r")[0]
	if want := spinnerFrames[0] + " Checking \x1b[36maws \x1b[0m"; first != want {
		t.Errorf("spinner line = %q, want %q", first, want)
	}
	if got := truncateVisible("项目项目", 3); got != "项" {
		t.Errorf("truncateVisible of wide text = %q, want %q", got, "项")
	}
	if got := truncateVisible(label, 100); got != label {
		t.Errorf("truncateVisible of text that fits = %q, want it unchanged", got)
	}
}
