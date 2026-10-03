package sourcefacts

import (
	"encoding/json"
	"testing"
)

func TestSupportedProducerTuplesAreExactAndExcludeInheritedRecords(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"0.159.3", "0.160.0"} {
		for _, producer := range []struct {
			source     string
			originator string
		}{{"cli", "codex-tui"}, {"exec", "codex_exec"}, {"vscode", "Codex Desktop"}} {
			t.Run(version+"/"+producer.source, func(t *testing.T) {
				t.Parallel()
				raw, err := json.Marshal(producer.source)
				if err != nil {
					t.Fatal(err)
				}
				supported := CodexMeta{Source: raw, Originator: producer.originator, Version: version}
				if !SupportedCodexProducer(supported) {
					t.Fatal("inspected tuple unsupported")
				}
				for _, reject := range []struct {
					name  string
					alter func(*CodexMeta)
				}{
					{"unknown-version", func(m *CodexMeta) { m.Version = "0.999.0" }},
					{"prerelease", func(m *CodexMeta) { m.Version = version + "-alpha" }},
					{"wrong-originator", func(m *CodexMeta) { m.Originator = "codex_cli_rs" }},
					{"mismatched-approved-originator", func(m *CodexMeta) {
						if producer.source == "vscode" {
							m.Originator = "codex-tui"
						} else {
							m.Originator = "Codex Desktop"
						}
					}},
					{"remote-source", func(m *CodexMeta) { m.Source = json.RawMessage(`"remote"`) }},
					{"subagent-source", func(m *CodexMeta) { m.Source = json.RawMessage(`{"subagent":"review"}`) }},
					{"fork", func(m *CodexMeta) { m.ForkedFrom = json.RawMessage(`"parent"`) }},
					{"history-base", func(m *CodexMeta) { m.HistoryBase = json.RawMessage(`{}`) }},
					{"internal-thread", func(m *CodexMeta) { m.ThreadSource = json.RawMessage(`"memory_consolidation"`) }},
					{"spawned-agent", func(m *CodexMeta) { m.AgentPath = json.RawMessage(`"/agent/child"`) }},
				} {
					t.Run(reject.name, func(t *testing.T) {
						m := supported
						reject.alter(&m)
						if SupportedCodexProducer(m) {
							t.Fatal("unsupported/inherited tuple admitted")
						}
					})
				}
			})
		}
	}
}
