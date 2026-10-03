package collector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func discoveryProviderRegistration(t *testing.T, root string, n string) archive.SessionRegistration {
	t.Helper()
	id := "00000000-0000-0000-0000-" + n
	path := filepath.Join(root, "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "timestamp": at.Format(time.RFC3339Nano), "payload": map[string]any{"id": id, "timestamp": at.Format(time.RFC3339Nano), "cwd": root, "source": "cli", "originator": "synthetic", "cli_version": "test"}})
	task, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": at.Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": id, "root_turn_id": id, "started_at": at.Format(time.RFC3339Nano)}})
	if err := os.WriteFile(path, append(append(meta, '\n'), append(task, '\n')...), 0600); err != nil {
		t.Fatal(err)
	}
	return archive.SessionRegistration{Harness: archive.Harness{Name: "codex", Version: "test"}, Origin: archive.SessionOriginDiscovery, TranscriptPath: path, NativeSessionID: id, SessionStartedAt: at, DiscoveryRoot: root, DiscoveryCwd: root, DiscoveryProducerOriginator: "synthetic", DiscoveryProducerSource: "cli"}
}

func TestDiscoveryProviderPassesSeparateApprovedHomesAndLegacyFiles(t *testing.T) {
	t.Parallel()
	regs := []archive.SessionRegistration{discoveryProviderRegistration(t, t.TempDir(), "000000000001"), discoveryProviderRegistration(t, t.TempDir(), "000000000002")}
	legacy := regs[0]
	legacy.Origin = archive.SessionOriginHook
	regs = append(regs, legacy)
	opts := Options{Sources: testSources}
	closePass := openCursorPass(regs, &opts)
	defer func() { _ = closePass() }()
	for _, reg := range regs {
		reader, _ := newSourceReader(reg, opts)
		if _, err := reader.Signature(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reader.Filter(t.Context(), codex.Filter{}, DefaultMaxTranscriptBytes); err != nil {
			t.Fatal(err)
		}
	}
	if len(opts.sourcePasses.passes) != 3 {
		t.Fatalf("got %d provider passes, want separate capabilities for two approved homes and legacy", len(opts.sourcePasses.passes))
	}
	for _, reg := range regs {
		if _, ok := LastActivity(t.Context(), reg, "", testSources); !ok {
			t.Fatal("approved-home activity unavailable")
		}
	}
}

type discoveryImmutableFact string

const (
	discoveryNativeIDFact   discoveryImmutableFact = "native_id"
	discoveryStartFact      discoveryImmutableFact = "start"
	discoveryCWDFact        discoveryImmutableFact = "cwd"
	discoveryVersionFact    discoveryImmutableFact = "version"
	discoveryOriginatorFact discoveryImmutableFact = "originator"
	discoverySourceFact     discoveryImmutableFact = "source"
)

func TestDiscoveryImmutableFactsRejectBeforeSelectedFilter(t *testing.T) {
	t.Parallel()
	for _, field := range []discoveryImmutableFact{discoveryNativeIDFact, discoveryStartFact, discoveryCWDFact, discoveryVersionFact, discoveryOriginatorFact, discoverySourceFact} {
		t.Run(string(field), func(t *testing.T) {
			t.Parallel()
			reg := discoveryProviderRegistration(t, t.TempDir(), "000000000001")
			switch field {
			case discoveryNativeIDFact:
				reg.NativeSessionID = "other"
			case discoveryStartFact:
				reg.SessionStartedAt = reg.SessionStartedAt.Add(time.Second)
			case discoveryCWDFact:
				reg.DiscoveryCwd = reg.DiscoveryCwd + "-other"
			case discoveryVersionFact:
				reg.Harness.Version = "other"
			case discoveryOriginatorFact:
				reg.DiscoveryProducerOriginator = "other"
			case discoverySourceFact:
				reg.DiscoveryProducerSource = "exec"
			}
			filter := &discoveryObservedFilter{TranscriptFilter: codex.Filter{}}
			reader, _ := newSourceReader(reg, Options{Sources: testSources})
			if _, _, err := reader.Filter(t.Context(), filter, DefaultMaxTranscriptBytes); err == nil || filter.called {
				t.Fatalf("immutable %s reached filtering: called=%v err=%v", field, filter.called, err)
			}
		})
	}
}

type discoveryObservedFilter struct {
	agentapi.TranscriptFilter
	called bool
}

func (f *discoveryObservedFilter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	f.called = true
	return f.TranscriptFilter.Filter(ctx, in, c)
}
