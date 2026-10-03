package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// defaultEvalMaxBytes bounds one exported record, like handoff's default.
const defaultEvalMaxBytes = 120000

// runEvalCommand implements `agent-archive eval`, whose one action is export.
func runEvalCommand(args []string, stdout, stderr io.Writer, env Env) int {
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		fs := env.newCommandFlags("eval", stderr)
		if !fs.parseFlagsOnly(args) {
			return 2
		}
	}
	if len(args) == 0 {
		terminal.Println(stderr, "agent-archive: eval: choose export; run agent-archive eval --help")
		return 2
	}
	switch args[0] {
	case "export":
		return runEvalExport(args[1:], stdout, stderr, env)
	default:
		terminal.Printf(stderr, "agent-archive: eval: unknown action %q; run agent-archive eval --help\n", args[0])
		return 2
	}
}

// evalExportOptions is the validated command line of eval export.
type evalExportOptions struct {
	ids      []string
	harness  string
	detail   archive.EvalExportDetail
	maxBytes int
}

// runEvalExport implements `agent-archive eval export SESSION_ID...`: one
// JSON Lines record per session, in the order given, each built only from
// filtered data (dev/specs/eval-export.md). It never prompts, pages, or
// writes anything but stdout and stderr, so an agent or a script can run it
// headless. A session that cannot be exported is an error record on its own
// line; the others are still written, and the exit code is 1.
func runEvalExport(args []string, stdout, stderr io.Writer, env Env) int {
	opts, code := evalExportOptionsFromArgs(args, stderr, env)
	if code != 0 {
		return code
	}
	store, _, found, err := openReadOnlyStore(env)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: eval export: %v\n", err)
		return 1
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	ctx := context.Background()
	failed := false
	for _, id := range opts.ids {
		record := exportArchivedSession(ctx, store, id, opts)
		if _, isError := record.(archive.EvalExportError); isError {
			failed = true
		}
		if err := writeEvalRecord(stdout, record); err != nil {
			terminal.Printf(stderr, "agent-archive: eval export: %v\n", err)
			return 1
		}
	}
	if failed {
		return 1
	}
	return 0
}

// evalExportOptionsFromArgs parses eval export's flags, which may come before
// or after the session IDs.
func evalExportOptionsFromArgs(args []string, stderr io.Writer, env Env) (evalExportOptions, int) {
	fs := env.newCommandFlags("eval export", stderr)
	harness := fs.String("harness", "", "the harness the sessions were captured with (codex, claude, cursor); needed only when an ID exists under more than one")
	detail := fs.String("detail", string(archive.EvalExportDetailFull), "metadata (no conversation text; reads only metadata sidecars) or full (adds prompts, final response, edited files, feedback)")
	maxBytes := fs.Int("max-bytes", defaultEvalMaxBytes, "cut each record's longest texts to fit this many bytes (0 for no limit)")
	var ids []string
	rest := args
	for {
		if !fs.parse(rest) {
			return evalExportOptions{}, 2
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(ids) == 0 {
		return evalExportOptions{}, fs.usageError("name at least one SESSION_ID")
	}
	if *maxBytes < 0 {
		return evalExportOptions{}, fs.usageError("--max-bytes must be 0 or more")
	}
	canonical, ok := harnessFlagWithCatalog(fs.catalog, *harness)
	if !ok {
		return evalExportOptions{}, fs.usageError("%s", harnessFlagError(*harness))
	}
	d := archive.EvalExportDetail(*detail)
	if d != archive.EvalExportDetailMetadata && d != archive.EvalExportDetailFull {
		return evalExportOptions{}, fs.usageError("--detail must be metadata or full, not %q", *detail)
	}
	return evalExportOptions{ids: ids, harness: canonical, detail: d, maxBytes: *maxBytes}, 0
}

// exportArchivedSession is the record of one archived session: an
// archive.EvalExport, or an archive.EvalExportError saying why not. Only a
// full archive session ID is accepted, never a title or a prefix, so an
// export is always of exactly the session named. At metadata detail only the
// sidecar is read; at full detail the source bundle is read and verified
// against it, as show --transcript does.
func exportArchivedSession(ctx context.Context, store storage.ObjectStore, id string, opts evalExportOptions) any {
	fail := func(code archive.EvalErrorCode, message string) any {
		record := archive.NewEvalExportError(archive.EvalExportSourceArchive, id, code, message)
		if isArchiveSessionID(id) {
			record.SessionID = id
		}
		return record
	}
	if !isArchiveSessionID(id) {
		return fail(archive.EvalErrorNotFound, "not an archive session ID (32 lowercase hex digits); see agent-archive list --json")
	}
	key, err := locateMetadataKey(ctx, store, opts.harness, id)
	if err != nil {
		if strings.Contains(err.Error(), "more than one harness") {
			return fail(archive.EvalErrorAmbiguous, err.Error())
		}
		if strings.Contains(err.Error(), "no archived session") {
			return fail(archive.EvalErrorNotFound, "no archived session with this ID")
		}
		return fail(archive.EvalErrorReadFailed, "the session metadata could not be read or verified")
	}
	if opts.detail == archive.EvalExportDetailMetadata {
		metadata, err := reader.ReadMetadata(ctx, store, key)
		if errors.Is(err, storage.ErrNotFound) {
			return fail(archive.EvalErrorNotFound, "no archived session with this ID")
		}
		if err != nil {
			return fail(archive.EvalErrorReadFailed, "the session metadata could not be read or verified")
		}
		if actualKey, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID); err != nil || actualKey != key {
			return fail(archive.EvalErrorReadFailed, "the session metadata does not match the requested identity")
		}
		return archive.FitEvalExport(archive.EvalExportFromMetadata(metadata, archive.EvalExportSourceArchive), opts.maxBytes)
	}
	metadata, bundle, err := reader.RefreshAndLoad(ctx, store, key, reader.Limits{})
	if errors.Is(err, storage.ErrNotFound) {
		return fail(archive.EvalErrorNotFound, "no archived session with this ID")
	}
	if err != nil {
		return fail(archive.EvalErrorReadFailed, "the session metadata or filtered source could not be read or verified")
	}
	if actualKey, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID); err != nil || actualKey != key {
		return fail(archive.EvalErrorReadFailed, "the session metadata does not match the requested identity")
	}
	record, err := archive.BuildEvalExport(bundle, metadata, archive.EvalExportSourceArchive, archive.EvalExportDetailFull)
	if err != nil {
		return fail(archive.EvalErrorParseFailed, "the filtered source could not be parsed")
	}
	return archive.FitEvalExport(record, opts.maxBytes)
}

// writeEvalRecord writes one record as a line of JSON. Its strings come from
// filtered archive data, so the line goes through archive.DisplayJSON: a C1
// control or bidi override is written as a \u escape, never raw.
func writeEvalRecord(w io.Writer, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	data = append(archive.DisplayJSON(data), '\n')
	n, err := w.Write(data)
	if err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("write record: %w", io.ErrShortWrite)
	}
	return nil
}
