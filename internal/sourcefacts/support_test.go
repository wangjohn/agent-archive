package sourcefacts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/nativesessions"
)

func TestCodexCompatibilityUsesFormatNotReleaseOrClientName(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"0.140.0", "0.150.0", "0.155.0", "0.155.0-alpha.9.2", "0.159.3", "0.160.0", "0.999.0", "1.0.0-rc.1+build.2", "dev"} {
		for _, source := range []string{"cli", "exec", "vscode"} {
			for _, originator := range []string{"codex-tui", "codex_exec", "Codex Desktop", "codex_cli_rs", "new-client"} {
				m := CodexMeta{Source: json.RawMessage(`"` + source + `"`), Originator: originator, Version: version}
				if !SupportedCodexProducer(m) || CodexFormatProfile(m) != CodexLegacyJSONL {
					t.Fatalf("compatible metadata rejected: %+v", m)
				}
				m.HistoryMode = nativesessions.CodexHistoryPaginated
				if CodexFormatProfile(m) != CodexPaginatedJSONL {
					t.Fatal("paginated profile lost")
				}
			}
		}
	}
}

func TestCodexCompatibilityRejectsMissingOrUnsupportedEvidence(t *testing.T) {
	t.Parallel()
	withHistoryMode := func(raw string) func(*CodexMeta) {
		t.Helper()
		var decoded CodexMeta
		if err := json.Unmarshal([]byte(raw), &decoded.HistoryMode); err != nil {
			t.Fatal(err)
		}
		return func(m *CodexMeta) { m.HistoryMode = decoded.HistoryMode }
	}
	for _, tc := range []struct {
		name  string
		alter func(*CodexMeta)
	}{
		{"missing-version", func(m *CodexMeta) { m.Version = "" }},
		{"blank-version", func(m *CodexMeta) { m.Version = " " }},
		{"unsafe-version", func(m *CodexMeta) { m.Version = "0.155.0\nsecret" }},
		{"large-version", func(m *CodexMeta) { m.Version = strings.Repeat("x", 129) }},
		{"missing-originator", func(m *CodexMeta) { m.Originator = "" }},
		{"blank-originator", func(m *CodexMeta) { m.Originator = " " }},
		{"unsafe-originator", func(m *CodexMeta) { m.Originator = "client\x1b" }},
		{"remote-source", func(m *CodexMeta) { m.Source = json.RawMessage(`"remote"`) }},
		{"missing-source", func(m *CodexMeta) { m.Source = nil }},
		{"subagent-source", func(m *CodexMeta) { m.Source = json.RawMessage(`{"subagent":"review"}`) }},
		{"fork", func(m *CodexMeta) { m.ForkedFrom = json.RawMessage(`"parent"`) }},
		{"history-base", func(m *CodexMeta) { m.HistoryBase = json.RawMessage(`{}`) }},
		{"internal-thread", func(m *CodexMeta) { m.ThreadSource = json.RawMessage(`"memory_consolidation"`) }},
		{"spawned-agent", func(m *CodexMeta) { m.AgentPath = json.RawMessage(`"/agent/child"`) }},
		{"compressed", withHistoryMode(`"compressed"`)},
		{"referenced", withHistoryMode(`"referenced"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := CodexMeta{Source: json.RawMessage(`"cli"`), Originator: "codex-tui", Version: "0.999.0"}
			tc.alter(&m)
			if SupportedCodexProducer(m) || CodexFormatProfile(m) != "" {
				t.Fatal("unsupported metadata admitted")
			}
		})
	}
}

func TestCodexEvidenceLabelsNeverGateCompatibleVersions(t *testing.T) {
	t.Parallel()
	for version, want := range map[string]CodexEvidence{"0.159.3": CodexRuntimeTested, "0.160.0": CodexSourceInspected, "0.155.0": CodexSourceInspected, "0.999.0": CodexCompatibleUntested, "0.160.0-alpha.1": CodexCompatibleUntested} {
		m := CodexMeta{Source: json.RawMessage(`"cli"`), Originator: "codex-tui", Version: version}
		if !SupportedCodexProducer(m) || CodexProducerEvidence(m) != want {
			t.Fatalf("%s: %s", version, CodexProducerEvidence(m))
		}
		m.Originator = "new-client"
		if !SupportedCodexProducer(m) || CodexProducerEvidence(m) != CodexCompatibleUntested {
			t.Fatal("uninspected client was treated as runtime evidence or blocked")
		}
	}
}
