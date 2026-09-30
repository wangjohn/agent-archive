package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

// addSession registers a session through the hook path, writes its
// transcript (no prompt when prompt is empty), and dates the transcript
// active. It returns the archive session ID.
func (f handoffFixture) addSession(t *testing.T, harness, native, prompt string, active time.Time) string {
	t.Helper()
	transcript := filepath.Join(f.project, native+".jsonl")
	must(t, os.WriteFile(transcript, nil, 0o600))
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": native, "cwd": f.project, "transcript_path": transcript}
	if err := capture.HandleEvent(f.home, harness, payload, f.env.now()); err != nil {
		t.Fatal(err)
	}
	var content string
	switch {
	case harness == "claude" && prompt != "":
		content = fmt.Sprintf(`{"type":"user","uuid":"u1","sessionId":%q,"timestamp":"2026-01-02T00:00:00Z","cwd":%q,"message":{"role":"user","content":%q}}`+"\n", native, f.project, prompt)
	case harness == "codex":
		content = fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-01-02T00:00:00Z","payload":{"id":%q,"cwd":%q}}`+"\n", native, f.project)
		if prompt != "" {
			content += fmt.Sprintf(`{"type":"response_item","timestamp":"2026-01-02T00:00:02Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`+"\n", prompt)
		}
	}
	must(t, os.WriteFile(transcript, []byte(content), 0o600))
	must(t, os.Chtimes(transcript, active, active))
	regs, err := state.OpenReadOnly(f.home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range regs {
		if reg.NativeSessionID == native {
			return reg.ArchiveSessionID
		}
	}
	t.Fatalf("no registration for %s", native)
	return ""
}

func (f handoffFixture) sync(t *testing.T) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := runSyncCommand(nil, &out, &errOut, f.env); code != 0 {
		t.Fatalf("sync code=%d stderr=%s", code, errOut.String())
	}
}

func (f handoffFixture) unregister(t *testing.T, id string) {
	t.Helper()
	must(t, os.Remove(filepath.Join(f.home, "registrations", id+".json")))
}

// pickerFixture has, newest first: a local session not yet uploaded, an
// archived session no longer registered here, and the fixture's session,
// both registered and archived. A newer session with no prompt is hidden.
type pickerFixture struct {
	handoffFixture
	notUploaded string
	archiveOnly string
	both        string
	noPrompt    string
}

func newPickerFixture(t *testing.T) pickerFixture {
	t.Helper()
	f := newHandoffFixture(t, true)
	base := f.env.now()
	must(t, os.Chtimes(filepath.Join(f.project, "codex.jsonl"), base.Add(-2*time.Hour), base.Add(-2*time.Hour)))
	archiveOnly := f.addSession(t, "codex", "native-archived", "Archived elsewhere", base.Add(-time.Hour))
	f.sync(t)
	f.unregister(t, archiveOnly)
	notUploaded := f.addSession(t, "codex", "native-new", "Not uploaded yet", base.Add(time.Hour))
	noPrompt := f.addSession(t, "codex", "native-empty", "", base.Add(2*time.Hour))
	return pickerFixture{handoffFixture: f, notUploaded: notUploaded, archiveOnly: archiveOnly, both: f.id, noPrompt: noPrompt}
}

// runPicker runs handoff on a terminal with answer as the input.
func runPicker(t *testing.T, env Env, answer string, args ...string) (string, string, int) {
	t.Helper()
	stdin := strings.NewReader(answer)
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&out) }
	if env.RunPager == nil {
		var pager string
		env.RunPager = copyPager(&pager)
	}
	code := Run(append([]string{"handoff"}, args...), stdin, &out, &errOut, env)
	return out.String(), errOut.String(), code
}

// pickerLine returns the one picker line naming id, failing when there is
// not exactly one.
func pickerLine(t *testing.T, out, id string) string {
	t.Helper()
	var found []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, id[:minShortSessionID]) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d picker lines name %s:\n%s", len(found), id, out)
	}
	return found[0]
}

func TestHandoffPickerMergesLocalAndArchivedSessionsByActivity(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if strings.Contains(out, f.noPrompt[:minShortSessionID]) {
		t.Fatalf("a session with no prompt is listed:\n%s", out)
	}
	newest, archived, oldest := pickerLine(t, out, f.notUploaded), pickerLine(t, out, f.archiveOnly), pickerLine(t, out, f.both)
	if strings.Index(out, newest) >= strings.Index(out, archived) || strings.Index(out, archived) >= strings.Index(out, oldest) {
		t.Fatalf("not ordered by activity:\n%s", out)
	}
	if !strings.Contains(newest, "Not uploaded yet · not yet uploaded") || !strings.HasPrefix(newest, "1 ") {
		t.Fatalf("local-only row: %q", newest)
	}
	if strings.Contains(archived, "not yet uploaded") || strings.Contains(oldest, "not yet uploaded") {
		t.Fatalf("an archived session is marked not uploaded:\n%s", out)
	}
	// The local session's activity, not its older capture, dates the row.
	if !strings.Contains(oldest, "2 hours ago") {
		t.Fatalf("registered row is not dated by local activity: %q", oldest)
	}
	if !strings.Contains(out, "3 session(s).") {
		t.Fatalf("footer:\n%s", out)
	}
}

func TestHandoffPickerHandsOffASessionNotYetUploaded(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	out, errOut, code := runPicker(t, f.env, "1\np\n")
	if code != 0 || !strings.Contains(out, "source: local") || !strings.Contains(out, "Not uploaded yet") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
}

func TestHandoffPickerSourceNarrowsRows(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	out, _, _ := runPicker(t, f.env, "q\n", "--source", "archive")
	if strings.Contains(out, f.notUploaded[:minShortSessionID]) || !strings.Contains(out, f.archiveOnly[:minShortSessionID]) {
		t.Fatalf("--source archive lists a local-only session:\n%s", out)
	}
	out, _, _ = runPicker(t, f.env, "q\n", "--source", "local")
	if strings.Contains(out, f.archiveOnly[:minShortSessionID]) || !strings.Contains(out, f.both[:minShortSessionID]) || !strings.Contains(out, f.notUploaded[:minShortSessionID]) {
		t.Fatalf("--source local lists an archive-only session:\n%s", out)
	}
}

func TestHandoffPickerRespectsHarness(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	claude := f.addSession(t, "claude", "claude-native", "A Claude task", f.env.now().Add(3*time.Hour))
	out, _, _ := runPicker(t, f.env, "q\n", "--harness", "codex")
	if strings.Contains(out, claude[:minShortSessionID]) || !strings.Contains(out, f.notUploaded[:minShortSessionID]) {
		t.Fatalf("--harness codex:\n%s", out)
	}
	out, _, _ = runPicker(t, f.env, "q\n", "--harness", "claude")
	if !strings.Contains(out, claude[:minShortSessionID]) || strings.Contains(out, f.notUploaded[:minShortSessionID]) || strings.Contains(out, f.archiveOnly[:minShortSessionID]) {
		t.Fatalf("--harness claude:\n%s", out)
	}
}

// With the archive unreachable, the picker still offers this machine's
// sessions, marks none as not uploaded (it cannot know), and hands one off.
func TestHandoffPickerWorksWithoutTheArchive(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("offline") }
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 || !strings.Contains(errOut, "archive could not be read") || !strings.Contains(errOut, "offline") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	pickerLine(t, out, f.notUploaded)
	pickerLine(t, out, f.both)
	if strings.Contains(out, f.archiveOnly[:minShortSessionID]) || strings.Contains(out, "not yet uploaded") {
		t.Fatalf("offline picker:\n%s", out)
	}
	out, errOut, code = runPicker(t, f.env, "1\np\n")
	if code != 0 || !strings.Contains(out, "Not uploaded yet") {
		t.Fatalf("offline handoff: code=%d stderr=%s", code, errOut)
	}
}

func TestHandoffPickerBeforeSetup(t *testing.T) {
	t.Parallel()
	out, errOut, code := runPicker(t, testEnv(t, t.TempDir(), time.Now()), "1\n")
	if code != 1 || out != "" || errOut != notSetUpMessage+"\n" {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, errOut)
	}
}

// recordLaunch makes LaunchHandoff record the handoff document it was given.
func recordLaunch(t *testing.T, env *Env) *string {
	t.Helper()
	document := new(string)
	env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		data, err := os.ReadFile(spec.HandoffFile)
		if err != nil {
			t.Fatal(err)
		}
		*document = string(data)
		return nil
	}
	// Off a terminal, as when an agent runs it, the agent opens in a new
	// window.
	env.OpenTerminal = func(spec termlaunch.Spec) (string, error) {
		data, err := os.ReadFile(filepath.Join(spec.ScriptDir, launchHandoffName))
		if err != nil {
			t.Fatal(err)
		}
		*document = string(data)
		return "a new tmux window", nil
	}
	return document
}

func agentEnv(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// Run by an agent, --to with no selector hands off that agent's session,
// even when another session is newer.
func TestHandoffToUsesTheCallingSession(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	claude := f.addSession(t, "claude", "claude-native", "A Claude task", f.env.now().Add(-5*time.Hour))
	for _, tc := range []struct {
		variable string
		native   string
		want     string
		prompt   string
	}{
		{"CLAUDE_CODE_SESSION_ID", "claude-native", claude, "A Claude task"},
		{"CODEX_THREAD_ID", "native-1", f.both, "Fix the flaky widget test."},
	} {
		env := f.env
		env.LookupEnv = agentEnv(map[string]string{tc.variable: tc.native})
		document := recordLaunch(t, &env)
		_, errOut, code := runHandoff(t, env, "--to", "cursor")
		if code != 0 || !strings.Contains(errOut, "runs in, "+tc.want) || !strings.Contains(*document, tc.prompt) {
			t.Fatalf("%s: code=%d stderr=%s\n%s", tc.variable, code, errOut, *document)
		}
	}
}

// A session variable names a session of its own harness only, and --harness
// narrows which variable counts.
func TestHandoffToIgnoresASessionVariableOfAnotherHarness(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("launched")
		return nil
	}
	f.env.LookupEnv = agentEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": "native-1"})
	if _, errOut, code := runHandoff(t, f.env, "--to", "claude"); code != 2 || !strings.Contains(errOut, noCurrentSessionMessage) {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	f.env.LookupEnv = agentEnv(map[string]string{"CODEX_THREAD_ID": "native-1"})
	if _, errOut, code := runHandoff(t, f.env, "--to", "claude", "--harness", "claude"); code != 2 {
		t.Fatalf("--harness claude used a Codex session: code=%d stderr=%s", code, errOut)
	}
}

// In Cursor, which names no session, --to is --latest --harness cursor.
func TestHandoffToInCursorUsesLatestCursorSession(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.env.LookupEnv = agentEnv(map[string]string{"CURSOR_AGENT": "1"})
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("launched without a Cursor session")
		return nil
	}
	_, errOut, code := runHandoff(t, f.env, "--to", "claude")
	if code != 1 || !strings.Contains(errOut, "no session for "+f.project) || !strings.Contains(errOut, "(harness cursor)") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	// --harness naming another agent overrides the fallback.
	if _, errOut, code := runHandoff(t, f.env, "--to", "claude", "--harness", "codex"); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

func TestHandoffToWithoutSelectorPicksOnATerminal(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	document := recordLaunch(t, &f.env)
	out, errOut, code := runPicker(t, f.env, "1\n", "--to", "claude")
	if code != 0 || !strings.Contains(out, "to hand off") || !strings.Contains(*document, "Not uploaded yet") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	*document = ""
	out, errOut, code = runHandoff(t, f.env, "--to", "claude")
	if code != 2 || out != "" || *document != "" || !strings.Contains(errOut, noCurrentSessionMessage) {
		t.Fatalf("off a terminal: code=%d stderr=%s", code, errOut)
	}
}

// A session only the archive has is handed off from the archive, told how to
// read more from the archive rather than from a local transcript.
func TestHandoffToLaunchesAnArchiveOnlySession(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	document := recordLaunch(t, &f.env)
	_, errOut, code := runHandoff(t, f.env, f.archiveOnly, "--to", "codex")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"source: archive", "Archived elsewhere", "handoff " + f.archiveOnly + " --harness codex --max-bytes 0"} {
		if !strings.Contains(*document, want) {
			t.Errorf("document missing %q:\n%s", want, *document)
		}
	}
	if strings.Contains(*document, "--source local") {
		t.Errorf("archive handoff points to a local transcript:\n%s", *document)
	}
}

// addSubagent registers a subagent of parent under native, sharing parent's
// transcript, as newer than everything else.
func (f handoffFixture) addSubagent(t *testing.T, parent, native string) string {
	t.Helper()
	reg, found, err := state.OpenReadOnly(f.home).LoadRegistration(parent)
	if err != nil || !found {
		t.Fatalf("load %s: found=%v err=%v", parent, found, err)
	}
	id := "ffffffff" + parent[8:]
	reg.ArchiveSessionID, reg.NativeSessionID = id, native
	reg.ParentSessionID, reg.ParentNativeSessionID, reg.SubagentID = parent, "native-new", "agent-1"
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(f.home, "registrations", id+".json"), data, 0o600))
	return id
}

// A subagent is never offered on its own, nor taken as the calling session.
func TestHandoffPickerAndToSkipSubagents(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	sub := f.addSubagent(t, f.notUploaded, "native-sub")
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 || strings.Contains(out, sub[:minShortSessionID]) {
		t.Fatalf("code=%d stderr=%s; subagent listed:\n%s", code, errOut, out)
	}
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("launched a subagent")
		return nil
	}
	f.env.LookupEnv = agentEnv(map[string]string{"CODEX_THREAD_ID": "native-sub"})
	if _, errOut, code := runHandoff(t, f.env, "--to", "claude"); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// Picker titles come from the filtered record, never the raw transcript.
func TestHandoffPickerTitleIsFiltered(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	id := f.addSession(t, "codex", "native-secret", "Use key sk-abcdefghijklmnopqrstuv please", f.env.now().Add(3*time.Hour))
	out, errOut, code := runPicker(t, f.env, "q\n")
	line := pickerLine(t, out, id)
	if code != 0 || strings.Contains(out, "sk-abcdefghijklmnopqrstuv") || !strings.Contains(line, "[REDACTED]") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
}

// Past the limit, the footer counts only sessions that can be offered: an
// exact count when every one left is archived, "or more" when some not yet
// uploaded were not read to see whether they have a prompt.
func TestHandoffPickerFooterCountsOfferableSessions(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	for i := range defaultListLimit {
		f.addSession(t, "codex", fmt.Sprintf("native-many-%d", i), fmt.Sprintf("Task %d", i), f.env.now().Add(3*time.Hour+time.Duration(i)*time.Minute))
	}
	f.sync(t)
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 || !strings.Contains(out, fmt.Sprintf("Showing %d of %d session(s).", defaultListLimit, defaultListLimit+4)) {
		t.Fatalf("all archived: code=%d stderr=%s\n%s", code, errOut, out)
	}
	f.addSession(t, "codex", "native-old", "", f.env.now().Add(-3*time.Hour))
	out, errOut, code = runPicker(t, f.env, "q\n")
	// handoff takes neither --limit nor --since, so the footer doesn't offer them.
	if code != 0 || !strings.Contains(out, fmt.Sprintf("Showing %d or more session(s). Narrow with --harness", defaultListLimit)) || strings.Contains(out, "--limit") {
		t.Fatalf("one not uploaded: code=%d stderr=%s\n%s", code, errOut, out)
	}
	out, errOut, code = runPicker(t, f.env, "q\n", "--source", "archive")
	if code != 0 || !strings.Contains(out, fmt.Sprintf("Showing %d of %d session(s).", defaultListLimit, defaultListLimit+4)) {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
}
