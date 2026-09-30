package gitremote

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Errorf("OriginURL took %v, want about %v", elapsed, Timeout)
	}
}

func TestOriginURLReturnsInBoundedTimeWhenGitLeavesAChildHoldingTheOutput(t *testing.T) {
	// The shell exits, but a background sleep keeps standard output open.
	fakeGitOnPath(t, `sleep 30 &
echo "https://example.test/acme/widget.git"`)
	start := time.Now()
	OriginURL(t.Context(), t.TempDir(), nil)
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
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

func TestUsableGitRefusesTheMacStubWithoutDeveloperTools(t *testing.T) {
	t.Parallel()
	none := func(string) bool { return false }
	only := func(want string) func(string) bool { return func(p string) bool { return p == want } }
	for _, tc := range []struct {
		name   string
		path   string
		goos   string
		exists func(string) bool
		want   bool
	}{
		{"stub without the tools", "/usr/bin/git", "darwin", none, false},
		{"stub with the command line tools", "/usr/bin/git", "darwin", only("/Library/Developer/CommandLineTools/usr/bin/git"), true},
		{"stub with Xcode", "/usr/bin/git", "darwin", only("/Applications/Xcode.app/Contents/Developer/usr/bin/git"), true},
		{"homebrew git", "/opt/homebrew/bin/git", "darwin", none, true},
		{"linux /usr/bin/git", "/usr/bin/git", "linux", none, true},
	} {
		if got := usableGit(tc.path, tc.goos, tc.exists); got != tc.want {
			t.Errorf("%s: usableGit = %t, want %t", tc.name, got, tc.want)
		}
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
