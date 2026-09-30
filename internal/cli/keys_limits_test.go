package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Back from Ctrl-Z, key mode goes on before the browser's screen is shown:
// continued in the background, setting the modes stops the process until
// it is in the foreground, so the screen is never drawn over the shell.
func TestKeysResumeBeforeShowingTheScreen(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys(string(fakeSuspend))
	keys := startKeys(fake)
	keys.hide = func() { fake.note("hide") }
	keys.show = func() { fake.note("show") }
	if k, err := keys.next(); err != nil || k.kind != keyResize {
		t.Fatalf("key %v, err %v", k, err)
	}
	keys.close()
	if got, want := fake.history(), []string{"keys", "lines", "hide", "stop", "keys", "show", "flush", "lines", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// What the burst cap cuts off is carried only when it can be a key: a
// runaway sequence is dropped, not carried and grown without end.
func TestKeysCarryOnlyAShortTail(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys("x"+"\x1b["+strings.Repeat("1", 2*maxBurst), "q")
	keys := startKeys(fake)
	defer keys.close()
	for range 3 {
		if _, err := keys.readBurst(); err != nil {
			t.Fatal(err)
		}
		if len(keys.carry) > maxCarry {
			t.Fatalf("carried %d bytes", len(keys.carry))
		}
	}
}

// Within a burst, each PgDn goes on from where the one before it left the
// summary, whose lines wrap to different heights.
func TestKeyDetailsBurstsMoveOnFromEachKey(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), summaryNow)
	var out bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(&out) }
	env.TerminalSize = fixedTerminal{60, 9}.terminalSize
	b := &sessionBrowser{env: env, prompt: newPrompter(strings.NewReader(""), &out), stdout: &out, screen: &altScreen{}, noPager: true}
	// Six rows for the summary: six short lines, then two lines of three
	// rows each, then short lines again.
	lines := []string{"a", "b", "c", "d", "e", "f", strings.Repeat("g", 150), strings.Repeat("h", 150)}
	for i := range 30 {
		lines = append(lines, strconv.Itoa(i))
	}
	screen := b.drawDetailsKeys(lines, 0, false, browseNotice{})
	if screen.budget != 6 || screen.shown != 6 {
		t.Fatalf("budget %d, shown %d", screen.budget, screen.shown)
	}
	screen.scroll(scrollPageDown)
	screen.scroll(scrollPageDown)
	if screen.top != 8 {
		t.Fatalf("PgDn PgDn went to line %d, want 8", screen.top)
	}
}

// Only the browser reading keys takes Ctrl-Z for itself; nothing else in
// the package notifies SIGTSTP (Go would then ignore Ctrl-Z for good).
func TestOnlyKeyModeTakesCtrlZ(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "keys_unix.go" {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(source, []byte("SIGTSTP")) {
			t.Errorf("%s uses SIGTSTP; only keys_unix.go may", file)
		}
	}
}
