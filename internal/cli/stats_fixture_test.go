package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// statsNow is the clock the stats tests run at: noon UTC, so the window's
// days are UTC days and the tests need no time zone.
var statsNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// statsDay is a moment on a day of the fixture's calendar.
func statsDay(month time.Month, day, hour int) time.Time {
	return time.Date(2026, month, day, hour, 0, 0, 0, time.UTC)
}

// syntheticSession is a session sidecar the stats tests publish: all
// content is invented, and only metadata is set.
type syntheticSession struct {
	id, harness, project string
	captured             time.Time
	parent               string
	origin               archive.SessionOrigin
	models               []string
	turns, messages      int
	toolResults, errors  int
	compactions          int
	perModel             []modelTokenSpec
	skills               []string
	mcp                  map[string]int
	noTokens             bool
	parser               string
}

type modelTokenSpec struct {
	model                           string
	input, output, cacheRead, write int
}

// positive is a count that is reported only when it is above zero.
func positive(n int) *int {
	if n > 0 {
		return intPtr(n)
	}
	return nil
}

// build turns the spec into the sidecar's metadata.
func (s syntheticSession) build() archive.Metadata {
	parser := s.parser
	if parser == "" {
		parser = "0.14.0"
	}
	var models []archive.ModelSummary
	for _, model := range s.models {
		models = append(models, archive.ModelSummary{
			Attributes: map[string]string{"gen_ai.request.model": model}, Source: archive.ModelSummarySourceNativeTranscript,
			ResponseModelStatus: archive.ResponseModelStatusNotExposed, TurnCount: intPtr(s.turns),
		})
	}
	var input, output, read, write int
	var modelTokens []archive.ModelTokens
	for _, t := range s.perModel {
		input, output, read, write = input+t.input, output+t.output, read+t.cacheRead, write+t.write
		if parser != "0.13.0" {
			modelTokens = append(modelTokens, archive.ModelTokens{
				Model: t.model, InputTokens: intPtr(t.input), OutputTokens: intPtr(t.output),
				CacheReadTokens: intPtr(t.cacheRead), CacheWriteTokens: intPtr(t.write),
			})
		}
	}
	var toolErrors, inputTokens, outputTokens, cacheRead, cacheWrite *int
	if s.toolResults > 0 && s.harness != "codex" && parser != "0.13.0" {
		toolErrors = intPtr(s.errors)
	}
	if len(s.perModel) > 0 {
		inputTokens, outputTokens, cacheRead, cacheWrite = intPtr(input), intPtr(output), intPtr(read), intPtr(write)
	}
	counts := archive.Counts{
		Turns: positive(s.turns), Messages: positive(s.messages), Compactions: positive(s.compactions),
		ToolResults: positive(s.toolResults), ToolErrors: toolErrors,
		InputTokens: inputTokens, OutputTokens: outputTokens, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
	}
	var skills []archive.SkillUse
	for _, name := range s.skills {
		skills = append(skills, archive.SkillUse{Name: name, Evidence: archive.SkillUseEvidenceNativeInvocation})
	}
	var mcp []archive.ToolUsage
	for _, server := range slices.Sorted(maps.Keys(s.mcp)) {
		mcp = append(mcp, archive.ToolUsage{Name: server, Count: s.mcp[server]})
	}
	return archive.Metadata{
		SchemaVersion: archive.MetadataSchemaVersion, SessionID: s.id, NativeSessionID: s.id,
		ProjectName: s.project, StartedAt: s.captured, CapturedAt: s.captured, MetadataDerivedAt: s.captured,
		Harness:         archive.Harness{Name: s.harness},
		Parser:          archive.ParserInfo{Name: s.harness, Version: parser, Status: archive.ParserStatusPartial},
		ParentSessionID: s.parent, Origin: s.origin,
		SourceBundle: archive.SourceReference{Key: "sessions/" + s.harness + "/" + s.id + "/source.jsonl.gz", SHA256: strings.Repeat("a", 64)},
		Counts:       counts, Models: models, ModelTokens: modelTokens, SkillsUsed: skills, MCPCalls: mcp,
	}
}

// publish writes the sidecar to the store.
func (s syntheticSession) publish(t testing.TB, mem *storagetest.MemoryStore) {
	t.Helper()
	data, err := json.Marshal(s.build())
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey(s.harness, s.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
}

// statsEnv is an Env with a configured (empty) archive whose store is mem
// and whose clock is statsNow.
func statsEnv(t *testing.T) (Env, *storagetest.MemoryStore) {
	t.Helper()
	home := t.TempDir()
	setUpTestConfig(t, home, t.TempDir(), statsNow)
	mem := storagetest.NewMemoryStore()
	env := testEnv(t, home, statsNow)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	return env, mem
}

// publishStatsFixture publishes a multi-agent, multi-model archive: Claude
// Code sessions across two projects with opus and sonnet, two subagents, a
// long-context session and skills and MCP calls; Codex sessions with a priced
// and an unpriced model; Cursor sessions with no tokens; sessions in the
// previous period; and sessions in earlier months for the month rank.
func publishStatsFixture(t testing.TB, mem *storagetest.MemoryStore) {
	t.Helper()
	opus, sonnet := "claude-opus-5", "claude-sonnet-5"
	var sessions []syntheticSession
	add := func(s syntheticSession) { sessions = append(sessions, s) }

	// The window: Aug 31 to Sep 29.
	for i := 0; i < 12; i++ {
		day := statsDay(time.September, 2+i*2, 9+i%6)
		project := "agent-archive"
		if i%3 == 0 {
			project = "proj-api"
		}
		add(syntheticSession{
			id: fmt.Sprintf("claude-%02d", i), harness: "claude", project: project, captured: day,
			models: []string{opus, sonnet}, turns: 8 + i, messages: 60 + 5*i, toolResults: 100, errors: 4 + i%3,
			perModel: []modelTokenSpec{
				{opus, 40_000 + i*2_000, 120_000, 2_500_000 + i*100_000, 300_000},
				{sonnet, 20_000, 60_000, 900_000, 100_000},
			},
			skills: []string{"review-pr"}, mcp: map[string]int{"github": 3 + i},
		})
	}
	add(syntheticSession{
		id: "claude-big", harness: "claude", project: "proj-api", captured: statsDay(time.September, 17, 14),
		models: []string{opus}, turns: 40, messages: 200, toolResults: 400, errors: 30, compactions: 2,
		perModel: []modelTokenSpec{{opus, 3_000_000, 900_000, 12_000_000, 2_000_000}},
		skills:   []string{"create-skill", "review-pr"}, mcp: map[string]int{"linear": 12},
	})
	for i := 0; i < 2; i++ {
		add(syntheticSession{
			id: fmt.Sprintf("claude-sub-%d", i), harness: "claude", project: "proj-api", captured: statsDay(time.September, 17, 15),
			parent: "claude-big", models: []string{sonnet}, turns: 5, toolResults: 50, errors: 2,
			perModel: []modelTokenSpec{{sonnet, 500_000, 200_000, 4_000_000, 300_000}},
		})
	}
	for i := 0; i < 5; i++ {
		add(syntheticSession{
			id: fmt.Sprintf("codex-%02d", i), harness: "codex", project: "dotfiles", captured: statsDay(time.September, 5+i*4, 11),
			models: []string{"gpt-5"}, turns: 6, messages: 40, toolResults: 30,
			// Codex's input includes the cached input.
			perModel: []modelTokenSpec{{"gpt-5", 900_000, 80_000, 700_000, 0}},
		})
	}
	add(syntheticSession{
		id: "codex-review", harness: "codex", project: "dotfiles", captured: statsDay(time.September, 24, 11),
		models: []string{"codex-auto-review"}, turns: 2, messages: 10, toolResults: 5,
		perModel: []modelTokenSpec{{"codex-auto-review", 100_000, 10_000, 50_000, 0}},
	})
	for i := 0; i < 3; i++ {
		add(syntheticSession{
			id: fmt.Sprintf("cursor-%02d", i), harness: "cursor", project: "agent-archive", captured: statsDay(time.September, 10+i*5, 16),
			models: []string{"cursor-auto"}, turns: 4, noTokens: true,
		})
	}
	// The previous period: Aug 1 to Aug 30.
	for i := 0; i < 6; i++ {
		add(syntheticSession{
			id: fmt.Sprintf("claude-prev-%02d", i), harness: "claude", project: "agent-archive", captured: statsDay(time.August, 3+i*4, 10),
			models: []string{opus}, turns: 7, messages: 50, toolResults: 80, errors: 3,
			perModel: []modelTokenSpec{{opus, 30_000, 90_000, 2_000_000, 200_000}},
		})
	}
	// Earlier months, for the month rank.
	for i, month := range []time.Month{time.April, time.May, time.June, time.July} {
		add(syntheticSession{
			id: fmt.Sprintf("claude-old-%d", i), harness: "claude", project: "agent-archive", captured: statsDay(month, 10, 10),
			models: []string{opus}, turns: 5, messages: 30, toolResults: 40, errors: 1,
			perModel: []modelTokenSpec{{opus, 20_000 + i*1_000_000, 60_000, 1_000_000, 100_000}},
		})
	}
	for _, s := range sessions {
		s.publish(t, mem)
	}
}
