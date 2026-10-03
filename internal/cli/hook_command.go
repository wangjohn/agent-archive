package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runHookCommand implements the hidden `_hook` entry point hooks.Merge
// installs into each harness's own hook configuration. A hook has a short
// timeout (2s, per hooks.Merge) and must never block the user's turn, so
// this always exits 0; a problem is reported to stderr only, matching the
// spec's failure table ("Hook cannot write a request: task continues,
// diagnostic is available outside model context"). That includes a panic:
// Go exits 2 on one, which Claude Code treats as a blocking error (it
// erases the prompt on UserPromptSubmit and feeds the stack trace to the
// model on Stop), so a panic is recovered, recorded, and exits 0 too.
func runHookCommand(args []string, stdin io.Reader, stderr io.Writer, env Env) (code int) {
	var (
		home       string
		harness    = new(string)
		payload    map[string]any
		batch      []agentapi.LifecycleEvent
		diagnostic agentapi.HookDiagnosticDecoder
	)
	defer func() {
		if r := recover(); r != nil {
			terminal.Printf(stderr, "agent-archive: hook: internal error: %v\n", r)
			root := ""
			if diagnostic != nil {
				func() {
					defer func() { _ = recover() }()
					candidate := diagnostic.DiagnosticProject(agentapi.HookInput{Payload: payload})
					if len(candidate) <= 16<<20 && utf8.ValidString(candidate) && filepath.IsAbs(candidate) {
						root = candidate
					}
				}()
			}
			if root == "" && len(batch) > 0 {
				root = batch[0].ProjectRoot
			}
			capture.RecordFailure(home, *harness, root)
			code = 0
		}
	}()
	fs := flag.NewFlagSet("_hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness = fs.String("harness", "", "harness name (codex, claude, cursor)")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	// A hook that sends no or malformed JSON is treated as a no-op, not an
	// error: some hook events (per the harness's own docs) carry no useful
	// fields at all, and we must never fail loudly on the harness's input.
	limited := &io.LimitedReader{R: stdin, N: (16 << 20) + 1}
	err := json.NewDecoder(limited).Decode(&payload)
	if err != nil || payload == nil || limited.N <= 0 {
		return 0
	}

	// Resolved without creating it: a hook left behind after the data
	// directory was deleted has nothing to record and must not recreate it.
	home, err = env.readHome()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: hook: resolve home: %v\n", err)
		return 0
	}
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return 0
	}
	registry := env.agentRegistry()
	decoder, ok := registry.LookupDecoder(*harness)
	if !ok {
		return 0
	}
	*harness = agentmeta.Canonical(registry.Catalog(), *harness)
	diagnostic, _ = decoder.(agentapi.HookDiagnosticDecoder)
	var now time.Time
	var clockFailure any
	func() {
		defer func() { clockFailure = recover() }()
		now = env.now()
	}()
	if clockFailure != nil {
		// Decode only diagnostic facts when the clock fails. Never admit this
		// batch, and preserve the original failure if the decoder also panics.
		func() {
			defer func() { _ = recover() }()
			batch, _ = decoder.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Now()})
		}()
		panic(clockFailure)
	}
	batch, err = decoder.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: now})
	if err != nil {
		terminal.Printf(stderr, "agent-archive: hook: %v\n", err)
		return 0
	}
	if err := capture.HandleBatch(home, *harness, batch, now, capture.WithRepoKey(env.repoKeyResolver()), capture.WithGitHead(env.gitHeadResolver())); err != nil {
		terminal.Printf(stderr, "agent-archive: hook: %v\n", err)
	}

	return 0
}
