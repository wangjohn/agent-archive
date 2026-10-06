package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/rolloutcatalog"
)

func TestPendingCatalogIsLazySharedAndDoesNotAdmitHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = "11111111-1111-4111-8111-111111111111"
		const other = "22222222-2222-4222-8222-222222222222"
		home := t.TempDir()
		dir := filepath.Join(home, "sessions")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw := `{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/synthetic/project","timestamp":"2026-10-01T12:00:00Z","cli_version":"0.160.0","originator":"codex_cli_rs","source":"cli"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":"synthetic"}}` + "\n"
		ordinary := writeTranscript(t, dir, "rollout-"+id+".jsonl", raw)
		related := writeTranscript(t, dir, "rollout-"+other+".jsonl", raw)
		c := rolloutcatalog.New([]string{home}, rolloutcatalog.Limits{PrefixBytes: 1})
		calls := 0
		opts := Options{Sources: testSources, PendingCodexRollouts: func() agentapi.CodexRolloutLookup { calls++; return c }}
		reg := archive.SessionRegistration{Harness: archive.Harness{Name: "codex"}, TranscriptPath: ordinary}
		_, filter, _ := testSources.LookupSources("codex")
		reader, ok := newSourceReader(reg, opts)
		if !ok {
			t.Fatal("source reader missing")
		}
		if _, err := reader.Signature(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reader.Filter(t.Context(), filter, DefaultMaxTranscriptBytes); err != nil {
			t.Fatal(err)
		}
		if calls != 0 || c.Counters().Headers != 0 {
			t.Fatalf("ordinary capture enumerated history: calls %d counters %#v", calls, c.Counters())
		}
		reg.TranscriptPath = related
		reader, ok = newSourceReader(reg, opts)
		if !ok {
			t.Fatal("source reader missing")
		}
		for range 2 {
			if _, err := reader.Signature(t.Context()); !errors.Is(err, archive.ErrRelatedHistory) || !strings.Contains(err.Error(), "candidate rollout locators") {
				t.Fatalf("history fence lost: %v", err)
			}
		}
		if calls != 2 || c.Counters().Headers != 2 || c.Counters().PrefixBytes != 0 {
			t.Fatalf("pending views not shared metadata observation: calls %d counters %#v", calls, c.Counters())
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := reader.(providerReader).observePendingHistory(ctx, archive.ErrRelatedHistory)
		if !errors.Is(err, archive.ErrRelatedHistory) || !strings.Contains(err.Error(), "locator evidence unavailable") {
			t.Fatalf("cancelled fence lost: %v", err)
		}
	})
}
