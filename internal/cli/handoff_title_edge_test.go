package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An argument of only spaces is a usage error, not the no-selector case that
// lets --to take the calling agent's own session.
func TestHandoffBlankTitleIsAUsageError(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.env.LookupEnv = agentEnv(map[string]string{"CODEX_THREAD_ID": "native-new"})
	document := recordLaunch(t, &f.env)
	for _, args := range [][]string{{" "}, {"\t\n"}, {" ", "--to", "claude"}} {
		out, errOut, code := runHandoff(t, f.env, args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "is empty") {
			t.Errorf("%q: code=%d stdout=%q stderr=%s", args, code, out, errOut)
		}
	}
	if *document != "" {
		t.Fatalf("a blank title launched a handoff:\n%s", *document)
	}
}

// A query is matched as titles are stored, one line of single spaces.
func TestHandoffTitleQueryIsMatchedAsOneLine(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	for _, query := range []string{"not  uploaded", "Not\nuploaded\tyet", "  not uploaded  "} {
		if out, errOut, code := runHandoff(t, f.env, query); code != 0 || !strings.Contains(out, "Not uploaded yet") {
			t.Errorf("%q: code=%d stderr=%s", query, code, errOut)
		}
	}
}

// A pasted paragraph, or hostile text, is not echoed back in full.
func TestHandoffTitleEchoesAtMostALine(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	long := strings.Repeat("word ", 2000)
	_, errOut, code := runHandoff(t, f.env, long)
	if code != 1 || len(errOut) > 400 || !strings.Contains(errOut, "no session matches") || !strings.Contains(errOut, "…") {
		t.Fatalf("no match: code=%d, %d bytes: %s", code, len(errOut), errOut)
	}
	f.addSession(t, "codex", "native-a", "Twin one "+long, f.env.now().Add(3*time.Hour))
	f.addSession(t, "codex", "native-b", "Twin two", f.env.now().Add(4*time.Hour))
	_, errOut, code = runHandoff(t, f.env, "twin")
	if code != 1 || !strings.Contains(errOut, "matches 2 sessions") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	// The query itself, repeated in the header, is bounded too.
	_, errOut, code = runHandoff(t, f.env, "twin "+strings.Repeat("o", 3000))
	if code != 1 || len(errOut) > 400 {
		t.Fatalf("long query: code=%d, %d bytes", code, len(errOut))
	}
}

// A title that happens to be 32 characters long is not read from the archive
// as an ID; only 32 hexadecimal digits are.
func TestHandoffLongTitleIsNotAnArchiveID(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	title := "abcdefghijklmnopqrstuvwxyz-fixed"
	if len(title) != archiveSessionIDLength {
		t.Fatalf("test title is %d long", len(title))
	}
	f.addSession(t, "codex", "native-long", title, f.env.now().Add(3*time.Hour))
	opens := takeArchiveOffline(&f.env)
	if out, errOut, code := runHandoff(t, f.env, title); code != 0 || !strings.Contains(out, title) {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if *opens != 0 {
		t.Fatalf("opened the archive %d times for a title", *opens)
	}
}

// The agent session running the command is not offered for a title, so it
// does not make the title ambiguous; its ID and --to still name it.
func TestHandoffTitleSkipsTheCallingSession(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	// native-new is the fixture's "Not uploaded yet"; this one shares "yet".
	f.addSession(t, "codex", "native-other", "Other work not finished yet", f.env.now().Add(3*time.Hour))
	if _, errOut, code := runHandoff(t, f.env, "yet"); code != 1 || !strings.Contains(errOut, "matches 2 sessions") {
		t.Fatalf("outside an agent: code=%d stderr=%s", code, errOut)
	}
	env := f.env
	env.LookupEnv = agentEnv(map[string]string{"CODEX_THREAD_ID": "native-other"})
	out, errOut, code := runHandoff(t, env, "yet")
	if code != 0 || !strings.Contains(out, "Not uploaded yet") || strings.Contains(out, "Other work") {
		t.Fatalf("inside the other session: code=%d stderr=%s\n%s", code, errOut, out)
	}
	// Its own ID still names it.
	if out, errOut, code := runHandoff(t, env, f.notUploaded); code != 0 || !strings.Contains(out, "Not uploaded yet") {
		t.Fatalf("id: code=%d stderr=%s", code, errOut)
	}
	// With --to the caller is the source, as it is for --to with no selector.
	env.LookupEnv = agentEnv(map[string]string{"CODEX_THREAD_ID": "native-new"})
	document := recordLaunch(t, &env)
	if _, errOut, code := runHandoff(t, env, "not uploaded", "--to", "claude"); code != 0 || !strings.Contains(*document, "Not uploaded yet") {
		t.Fatalf("--to: code=%d stderr=%s", code, errOut)
	}
	if _, errOut, code := runHandoff(t, env, "not uploaded"); code != 1 || !strings.Contains(errOut, "no session matches") {
		t.Fatalf("the only match is the caller: code=%d stderr=%s", code, errOut)
	}
}

// A registration whose transcript is gone offers nothing to title from, so the
// session is found in the archive, once, and read from there; with no
// archived copy there is no match, not an error about the transcript.
func TestHandoffTitleWithAStaleRegistration(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	must(t, os.Remove(filepath.Join(f.project, "codex.jsonl")))
	must(t, os.Remove(filepath.Join(f.project, "native-new.jsonl")))
	out, errOut, code := runHandoff(t, f.env, "flaky widget")
	if code != 0 || !strings.Contains(out, "source: archive") || !strings.Contains(out, "flaky widget") {
		t.Fatalf("archived: code=%d stderr=%s\n%s", code, errOut, out)
	}
	out, errOut, code = runHandoff(t, f.env, "not uploaded")
	if code != 1 || out != "" || !strings.Contains(errOut, "no session matches") {
		t.Fatalf("not archived: code=%d stdout=%q stderr=%s", code, out, errOut)
	}
}

// A registration that spells its harness as an alias ("claude-code") is the
// same app as --harness claude, for an ID as for a title.
func TestHandoffHarnessAliasInARegistration(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	id := f.addSession(t, "claude", "native-alias", "An aliased Claude task", f.env.now().Add(3*time.Hour))
	path := filepath.Join(f.home, "registrations", id+".json")
	data, err := os.ReadFile(path)
	must(t, err)
	var reg map[string]any
	must(t, json.Unmarshal(data, &reg))
	reg["harness"].(map[string]any)["name"] = "claude-code"
	data, err = json.Marshal(reg)
	must(t, err)
	must(t, os.WriteFile(path, data, 0o600))
	takeArchiveOffline(&f.env)
	for _, args := range [][]string{{id, "--harness", "claude"}, {"aliased claude", "--harness", "claude"}, {id}} {
		if out, errOut, code := runHandoff(t, f.env, args...); code != 0 || !strings.Contains(out, "An aliased Claude task") {
			t.Errorf("%v: code=%d stderr=%s", args, code, errOut)
		}
	}
}
