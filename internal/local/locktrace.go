//go:build locktrace

package local

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// A locktrace build (go test -tags locktrace) checks, as the program runs,
// that no goroutine does slow work while it holds a lock that hooks wait on
// with a short deadline. Slow work is an fsync (every durable write makes two;
// F_FULLFSYNC on macOS has taken over two seconds under load) or a
// NamedLockWait, which may wait out its whole timeout.
//
// The hot locks are the per-session request and subagent-candidate locks
// under request-locks/: a hook waits one second for them. A holder that also
// holds hooks.lock is exempt: hooks take hooks.lock first, so none can be
// waiting behind it (hooks themselves, setup, and backfill hold both).
//
// A lock a test takes itself, to stand in for a busy holder, is not traced.
//
// A violation panics with the holder's stack, unless LOCKTRACE_LOG names a
// file, which then collects one line per distinct stack for a survey.

func hotLock(name string) bool { return strings.HasPrefix(name, "request-locks/") }

const exemptingLock = "hooks.lock"

var (
	traceMu sync.Mutex
	held    = map[uint64][]string{}
	logged  = map[string]bool{}
)

func traceHold(name string, release func()) func() {
	if takenByTest() {
		return release
	}
	g := goroutineID()
	traceMu.Lock()
	held[g] = append(held[g], name)
	traceMu.Unlock()
	return func() {
		traceMu.Lock()
		names := held[g]
		for i := len(names) - 1; i >= 0; i-- {
			if names[i] == name {
				names = append(names[:i], names[i+1:]...)
				break
			}
		}
		if len(names) == 0 {
			delete(held, g)
		} else {
			held[g] = names
		}
		traceMu.Unlock()
		release()
	}
}

func traceSlow(what string) {
	g := goroutineID()
	traceMu.Lock()
	names := append([]string(nil), held[g]...)
	traceMu.Unlock()
	hot := ""
	for _, name := range names {
		if name == exemptingLock {
			return
		}
		if hotLock(name) {
			hot = name
		}
	}
	if hot == "" {
		return
	}
	stack := callers()
	msg := fmt.Sprintf("locktrace: %s while holding %s (held: %s)\n%s", what, hot, strings.Join(names, ", "), stack)
	path := os.Getenv("LOCKTRACE_LOG")
	if path == "" {
		panic(msg)
	}
	key := firstLine(what) + "\n" + stack
	traceMu.Lock()
	defer traceMu.Unlock()
	if logged[key] {
		return
	}
	logged[key] = true
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		panic(err)
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s\n", msg)
}

// takenByTest reports a lock taken directly by test code: the first caller
// outside this package is in a _test.go file.
func takenByTest() bool {
	pcs := make([]uintptr, 16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		frame, more := frames.Next()
		if !strings.Contains(frame.Function, "/internal/local.") {
			return strings.HasSuffix(frame.File, "_test.go")
		}
		if !more {
			return false
		}
	}
}

func firstLine(s string) string {
	kind, _, _ := strings.Cut(s, " ")
	return kind
}

// callers is the calling stack outside this package's lock and write
// plumbing, one function per line, without test framework frames.
func callers() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.Function, "testing.") || strings.HasPrefix(frame.Function, "runtime.") {
			break
		}
		fmt.Fprintf(&b, "    %s (%s:%d)\n", strings.TrimPrefix(frame.Function, "github.com/wangjohn/agent-archive/internal/"), shortFile(frame.File), frame.Line)
		if !more {
			break
		}
	}
	return b.String()
}

func shortFile(path string) string {
	if i := strings.Index(path, "/internal/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// goroutineID parses the current goroutine's ID from its stack header,
// "goroutine 123 [running]:". Only a locktrace build pays for it.
func goroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	id, _ := strconv.ParseUint(string(buf[:bytes.IndexByte(buf, ' ')]), 10, 64)
	return id
}
