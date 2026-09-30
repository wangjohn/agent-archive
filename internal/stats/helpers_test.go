package stats

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

func ip(n int) *int { return &n }

// A metadata builder: meta("id", harness, capturedAt, options...).
type option func(*archive.Metadata)

func meta(id, harness string, captured time.Time, opts ...option) archive.Metadata {
	m := archive.Metadata{
		SchemaVersion: 1, SessionID: id, NativeSessionID: id, ProjectName: "proj",
		StartedAt: captured, CapturedAt: captured, MetadataDerivedAt: captured,
		Harness: archive.Harness{Name: harness},
		Parser:  archive.ParserInfo{Name: harness, Version: "0.14.0", Status: archive.ParserStatusPartial},
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

func project(name string) option { return func(m *archive.Metadata) { m.ProjectName = name } }

func parentOf(id string) option { return func(m *archive.Metadata) { m.ParentSessionID = id } }

func parserVersion(v string) option {
	return func(m *archive.Metadata) { m.Parser.Version = v }
}

func turns(n int) option { return func(m *archive.Metadata) { m.Counts.Turns = ip(n) } }

func messages(n int) option {
	return func(m *archive.Metadata) { m.Counts.Messages = ip(n) }
}

func compactions(n int) option {
	return func(m *archive.Metadata) { m.Counts.Compactions = ip(n) }
}

func toolResults(results, errors int) option {
	return func(m *archive.Metadata) { m.Counts.ToolResults, m.Counts.ToolErrors = ip(results), ip(errors) }
}

func toolResultsOnly(results int) option {
	return func(m *archive.Metadata) { m.Counts.ToolResults = ip(results) }
}

func skill(names ...string) option {
	return func(m *archive.Metadata) {
		for _, name := range names {
			m.SkillsUsed = append(m.SkillsUsed, archive.SkillUse{Name: name, Evidence: archive.SkillUseEvidenceNativeInvocation})
		}
	}
}

func mcp(server string, calls int) option {
	return func(m *archive.Metadata) {
		m.MCPCalls = append(m.MCPCalls, archive.ToolUsage{Name: server, Count: calls})
	}
}

// usedModel records a model the session used, with its turn count.
func usedModel(id string, turnCount int) option {
	return func(m *archive.Metadata) {
		m.Models = append(m.Models, archive.ModelSummary{
			Attributes: map[string]string{"gen_ai.request.model": id}, Source: archive.ModelSummarySourceNativeTranscript,
			ResponseModelStatus: archive.ResponseModelStatusNotExposed, TurnCount: ip(turnCount),
		})
	}
}

// tokens sets the session-wide counts, in the harness's own meaning: for
// Codex, input includes cached input. Zero values are set as reported zeros.
func tokens(input, output, read, write int) option {
	return func(m *archive.Metadata) {
		m.Counts.InputTokens, m.Counts.OutputTokens = ip(input), ip(output)
		m.Counts.CacheReadTokens, m.Counts.CacheWriteTokens = ip(read), ip(write)
	}
}

// modelTokens adds a per-model entry and adds its counts to the session-wide
// counts, as the parser does.
func modelTokens(model string, input, output, read, write int) option {
	return func(m *archive.Metadata) {
		m.ModelTokens = append(m.ModelTokens, archive.ModelTokens{
			Model: model, InputTokens: ip(input), OutputTokens: ip(output), CacheReadTokens: ip(read), CacheWriteTokens: ip(write),
		})
		add := func(target **int, n int) {
			if *target == nil {
				*target = ip(0)
			}
			**target += n
		}
		add(&m.Counts.InputTokens, input)
		add(&m.Counts.OutputTokens, output)
		add(&m.Counts.CacheReadTokens, read)
		add(&m.Counts.CacheWriteTokens, write)
	}
}

// day is a moment in 2026 in New York, the zone the tests count days in.
func day(month time.Month, d, hour int) time.Time {
	return time.Date(2026, month, d, hour, 0, 0, 0, newYork)
}

func f64(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%.6f", *p)
}

func i64(p *int64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatInt(*p, 10)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func mustJSON(tb testing.TB, v any) string {
	tb.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		tb.Fatal(err)
	}
	return string(data)
}

func agentByName(tb testing.TB, s Stats, harness string) Agent {
	tb.Helper()
	for _, a := range s.Agents {
		if a.Harness == harness {
			return a
		}
	}
	tb.Fatalf("no agent %q in %+v", harness, s.Agents)
	return Agent{}
}

func modelRow(tb testing.TB, s Stats, label string) ModelRow {
	tb.Helper()
	for _, r := range s.Models {
		if r.Label == label {
			return r
		}
	}
	tb.Fatalf("no model row %q in %+v", label, s.Models)
	return ModelRow{}
}
