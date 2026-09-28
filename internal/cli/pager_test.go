package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// publishNSessions returns an Env whose store holds n Codex sessions,
// newest capture first when listed, built by cloning the published fixture's
// sidecar with distinct IDs and capture times.
func publishNSessions(t *testing.T, n int) (Env, []string) {
	t.Helper()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var base archive.Metadata
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, n)
	ids[0] = id
	for i := 1; i < n; i++ {
		m := base
		m.SessionID = fmt.Sprintf("%s-%d", id, i)
		m.CapturedAt = base.CapturedAt.Add(time.Duration(i) * time.Hour)
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.MetadataObjectKey("codex", m.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Put(context.Background(), key, encoded); err != nil {
			t.Fatal(err)
		}
		ids[i] = m.SessionID
	}
	// Newest first: last inserted has the latest CapturedAt.
	newestFirst := make([]string, n)
	for i := range ids {
		newestFirst[i] = ids[n-1-i]
	}
	return env, newestFirst
}

func TestListLimitCapsNewestSessions(t *testing.T) {
	t.Parallel()
	env, newestFirst := publishNSessions(t, 5)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--limit", "2", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	listed := listedSessionIDs(out.String())
	if len(listed) != 2 || listed[0] != newestFirst[0] || listed[1] != newestFirst[1] {
		t.Fatalf("listed=%v want newest two %v\n%s", listed, newestFirst[:2], out.String())
	}
	if !strings.Contains(out.String(), "Showing 2 of 5 session(s).") || !strings.Contains(out.String(), "--limit 0") {
		t.Fatalf("missing truncated footer:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"list", "--limit", "0", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	listed = listedSessionIDs(out.String())
	if len(listed) != 5 {
		t.Fatalf("limit 0 listed %d, want 5:\n%s", len(listed), out.String())
	}
	if !strings.Contains(out.String(), "5 session(s).") || strings.Contains(out.String(), "Showing") {
		t.Fatalf("unexpected footer for full list:\n%s", out.String())
	}
}

func TestListJSONReportsLimitFields(t *testing.T) {
	t.Parallel()
	env, newestFirst := publishNSessions(t, 4)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--json", "--limit", "2"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	var doc listDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if doc.Version != 2 || doc.Limit != 2 || doc.Returned != 2 || doc.TotalMatched != 4 || !doc.Truncated {
		t.Fatalf("doc=%+v", doc)
	}
	if len(doc.Sessions) != 2 || doc.Sessions[0].SessionID != newestFirst[0] {
		t.Fatalf("sessions=%v want newest %s", doc.Sessions, newestFirst[0])
	}

	out.Reset()
	if code := Run([]string{"list", "--json", "--limit", "0"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	doc = listDocument{}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Limit != 0 || doc.Returned != 4 || doc.TotalMatched != 4 || doc.Truncated || len(doc.Sessions) != 4 {
		t.Fatalf("unlimited doc=%+v", doc)
	}
}

func TestListPagesOnTerminal(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	var paged bytes.Buffer
	var sawCommand string
	env.IsTerminal = func(stream any) bool {
		_, ok := stream.(*bytes.Buffer)
		return ok
	}
	env.RunPager = func(command string, stdin io.Reader, stdout, stderr io.Writer) error {
		sawCommand = command
		_, err := io.Copy(&paged, stdin)
		return err
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if sawCommand != "less -FRX" {
		t.Fatalf("pager command=%q", sawCommand)
	}
	if !strings.Contains(paged.String(), "session(s).") || out.Len() != 0 {
		t.Fatalf("paged=%q stdout=%q", paged.String(), out.String())
	}

	// --json never pages, even on a TTY.
	sawCommand = ""
	out.Reset()
	paged.Reset()
	if code := Run([]string{"list", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if sawCommand != "" || !strings.Contains(out.String(), `"schema_version": 2`) {
		t.Fatalf("json was paged (cmd=%q) or missing:\n%s", sawCommand, out.String())
	}

	// --no-pager and PAGER=cat skip the pager.
	sawCommand = ""
	out.Reset()
	if code := Run([]string{"list", "--no-pager"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("--no-pager: code=%d stderr=%s", code, errOut.String())
	}
	if sawCommand != "" || !strings.Contains(out.String(), "session(s).") {
		t.Fatalf("--no-pager: cmd=%q out=%q", sawCommand, out.String())
	}
	sawCommand = ""
	out.Reset()
	env.LookupEnv = func(key string) (string, bool) {
		if key == "PAGER" {
			return "cat", true
		}
		return "", false
	}
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("PAGER=cat: code=%d stderr=%s", code, errOut.String())
	}
	if sawCommand != "" || !strings.Contains(out.String(), "session(s).") {
		t.Fatalf("PAGER=cat: cmd=%q out=%q", sawCommand, out.String())
	}
}

func TestListPagerFailureFallsBack(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	env.IsTerminal = func(stream any) bool {
		_, ok := stream.(*bytes.Buffer)
		return ok
	}
	env.RunPager = func(string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("no less")
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "pager") || !strings.Contains(out.String(), "session(s).") {
		t.Fatalf("fallback missing: out=%q err=%q", out.String(), errOut.String())
	}
}

func TestResolvePagerCommand(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := Env{
		IsTerminal: func(any) bool { return true },
		LookupEnv:  func(string) (string, bool) { return "", false },
	}
	if cmd, ok := resolvePagerCommand(env, false, &stdout); !ok || cmd != "less -FRX" {
		t.Fatalf("default: %q %v", cmd, ok)
	}
	if _, ok := resolvePagerCommand(env, true, &stdout); ok {
		t.Fatal("--no-pager should disable")
	}
	env.IsTerminal = func(any) bool { return false }
	if _, ok := resolvePagerCommand(env, false, &stdout); ok {
		t.Fatal("non-TTY should disable")
	}
	env.IsTerminal = func(any) bool { return true }
	env.LookupEnv = func(key string) (string, bool) {
		if key == "AGENT_ARCHIVE_PAGER" {
			return "more", true
		}
		return "", false
	}
	if cmd, ok := resolvePagerCommand(env, false, &stdout); !ok || cmd != "more" {
		t.Fatalf("AGENT_ARCHIVE_PAGER: %q %v", cmd, ok)
	}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "AGENT_ARCHIVE_PAGER" {
			return "", true
		}
		return "less", true
	}
	if _, ok := resolvePagerCommand(env, false, &stdout); ok {
		t.Fatal("empty AGENT_ARCHIVE_PAGER should disable even when PAGER is set")
	}
}
