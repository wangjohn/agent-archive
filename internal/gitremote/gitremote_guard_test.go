package gitremote

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hangSlack is how long a call may take against a git that never answers. The
// fakes sleep 30 seconds, so any bound well under that tells a kill at the
// timeout from a wait for the process; the extra second is for a loaded runner.
const hangSlack = Timeout + time.Second

func TestTimeoutIsShortEnoughForAHook(t *testing.T) {
	t.Parallel()
	if Timeout > 500*time.Millisecond {
		t.Errorf("Timeout = %v, want at most 500ms: a hook has about two seconds in all", Timeout)
	}
}

// fakeGitOnPath puts an executable named git, with body as its script, first
// on PATH. Not for parallel tests: it changes the environment.
func fakeGitOnPath(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
}

func TestOriginURLReadsWhatARealExecutableNamedGitPrints(t *testing.T) {
	fakeGitOnPath(t, `echo "https://user:token@example.test/acme/widget.git"`)
	if got := OriginURL(t.Context(), t.TempDir(), nil); got != "https://user:token@example.test/acme/widget.git" {
		t.Errorf("OriginURL = %q", got)
	}
}

// A git that hangs is killed at the timeout, whatever it left running: the
// call returns in about Timeout, not when the process finishes. This also
// fails if the command loses its context or its WaitDelay.
func TestOriginURLReturnsInBoundedTimeWhenGitHangs(t *testing.T) {
	fakeGitOnPath(t, `exec sleep 30`)
	start := time.Now()
	if got := OriginURL(t.Context(), t.TempDir(), nil); got != "" {
		t.Errorf("OriginURL = %q, want empty from a git that never answered", got)
	}
	if elapsed := time.Since(start); elapsed > hangSlack {
		t.Errorf("OriginURL took %v, want about %v", elapsed, Timeout)
	}
}

func TestOriginURLReturnsInBoundedTimeWhenGitLeavesAChildHoldingTheOutput(t *testing.T) {
	// The shell exits, but a background sleep keeps standard output open.
	fakeGitOnPath(t, `sleep 30 &
echo "https://example.test/acme/widget.git"`)
	start := time.Now()
	OriginURL(t.Context(), t.TempDir(), nil)
	if elapsed := time.Since(start); elapsed > hangSlack {
		t.Errorf("OriginURL took %v with a child holding the pipe, want about %v", elapsed, Timeout)
	}
}

// Output past the cap is an error, not a URL cut short and then hashed.
func TestOriginURLIsEmptyWhenGitPrintsMoreThanAURL(t *testing.T) {
	fakeGitOnPath(t, `head -c 100000 /dev/zero | tr '\0' a`)
	if got := OriginURL(t.Context(), t.TempDir(), nil); got != "" {
		t.Errorf("OriginURL returned %d characters from an oversized output, want none", len(got))
	}
}

func TestLimitedBufferKeepsOnlyTheCapAndSaysItOverflowed(t *testing.T) {
	t.Parallel()
	var b limitedBuffer
	if n, err := b.Write([]byte(strings.Repeat("a", maxOutput))); n != maxOutput || err != nil || b.over {
		t.Fatalf("filling to the cap: n=%d err=%v over=%t", n, err, b.over)
	}
	if n, err := b.Write([]byte("b")); n != 1 || err != nil || !b.over || b.buf.Len() != maxOutput {
		t.Fatalf("past the cap: n=%d err=%v over=%t len=%d", n, err, b.over, b.buf.Len())
	}
	var viaReadFrom limitedBuffer
	if _, ok := any(&viaReadFrom).(io.ReaderFrom); ok {
		t.Error("limitedBuffer has a ReadFrom, which io.Copy would use to skip the cap")
	}
}

// The stub wiring: on macOS, /usr/bin/git is used only when the developer
// tools stand behind it, found at a usual place or through the link
// xcode-select reads, wherever Xcode was installed.
func TestLocatorRefusesTheMacStubWithoutDeveloperTools(t *testing.T) {
	t.Parallel()
	const xcodeGit = "/Applications/Xcode-15.4.app/Contents/Developer/usr/bin/git"
	present := func(paths ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(paths, p) }
	}
	link := func(target string) func(string) (string, error) {
		return func(p string) (string, error) {
			if p == xcodeSelectLink && target != "" {
				return target, nil
			}
			return "", os.ErrNotExist
		}
	}
	for _, tc := range []struct {
		name     string
		found    string
		goos     string
		exists   func(string) bool
		readlink func(string) (string, error)
		want     bool
	}{
		{"stub without the tools", macGitStub, "darwin", present(), link(""), false},
		{"stub with the command line tools", macGitStub, "darwin", present("/Library/Developer/CommandLineTools/usr/bin/git"), link(""), true},
		{"stub with the default Xcode", macGitStub, "darwin", present("/Applications/Xcode.app/Contents/Developer/usr/bin/git"), link(""), true},
		{"stub with Xcode elsewhere, by the link", macGitStub, "darwin", present(xcodeGit), link("/Applications/Xcode-15.4.app/Contents/Developer"), true},
		{"link to a directory with no git", macGitStub, "darwin", present(), link("/Applications/Xcode-15.4.app/Contents/Developer"), false},
		{"link that is not absolute", macGitStub, "darwin", present("usr/bin/git"), link("."), false},
		{"homebrew git", "/opt/homebrew/bin/git", "darwin", present(), link(""), true},
		{"linux /usr/bin/git", macGitStub, "linux", present(), link(""), true},
	} {
		l := locator{
			lookPath: func(string) (string, error) { return tc.found, nil },
			exists:   tc.exists, readlink: tc.readlink, goos: tc.goos,
		}
		got, err := l.find()
		if (err == nil) != tc.want || (err == nil && got != tc.found) {
			t.Errorf("%s: find = %q, %v; want usable=%t", tc.name, got, err, tc.want)
		}
	}
	missing := locator{lookPath: func(string) (string, error) { return "", exec.ErrNotFound }, goos: "darwin"}
	if _, err := missing.find(); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("find without git on PATH = %v, want exec.ErrNotFound", err)
	}
}

// Run under -race: a sweep can ask from several goroutines, and each root is
// still asked about once.
func TestResolverIsSafeForConcurrentUseAndAsksOncePerRoot(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	r := &Resolver{Run: func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		time.Sleep(time.Millisecond)
		return []byte("https://example.test/acme/widget.git"), nil
	}}
	roots := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	var wg sync.WaitGroup
	keys := make([][]string, len(roots))
	var mu sync.Mutex
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			root := i % len(roots)
			key := r.Key(roots[root])
			mu.Lock()
			keys[root] = append(keys[root], key)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != int32(len(roots)) {
		t.Errorf("git ran %d times for %d roots", got, len(roots))
	}
	for _, perRoot := range keys {
		for _, key := range perRoot {
			if key == "" || key != perRoot[0] {
				t.Errorf("keys differ within a root: %v", perRoot)
				break
			}
		}
	}
}
