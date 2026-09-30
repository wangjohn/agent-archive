package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// tokenTotals collects whatever token accounting the retained records expose,
// keyed by the message the accounting belongs to. Claude Code writes one JSONL
// record per content block of a single API message, and every one of those
// records repeats the same `message.id` and the same `message.usage`; adding
// each record's usage would count one response as many. The latest record seen
// for a message id replaces the earlier ones. Accounting with no message
// identity (Codex's `turn_token_usage`) is summed as it comes.
//
// Each record's accounting also remembers the model it belongs to, so the same
// records that make the session-wide totals make the per-model ones: the
// per-model sums add up to the totals by construction.
type tokenTotals struct {
	byMessage map[string]modelUsage
	anonymous []modelUsage
}

// modelUsage is one record's usage object and the model it is attributed to
// ("" when no model name could be attached).
type modelUsage struct {
	usage map[string]any
	model string
}

func (t *tokenTotals) observe(usage map[string]any, messageID, model string) {
	entry := modelUsage{usage: usage, model: model}
	if messageID == "" {
		t.anonymous = append(t.anonymous, entry)
		return
	}
	if t.byMessage == nil {
		t.byMessage = map[string]modelUsage{}
	}
	t.byMessage[messageID] = entry
}

// maxTokenCount is the largest count JSON carries exactly (2^53).
const maxTokenCount = 1 << 53

// tokenCount reads one native token count. Only a whole number from 0 to
// maxTokenCount is a count; a negative, fractional, or larger value (a
// corrupt or hostile record) is treated as absent, so it can neither make a
// total negative nor break the schema's bounds.
func tokenCount(raw any) (int, bool) {
	value, ok := raw.(float64)
	if !ok || value < 0 || value > maxTokenCount || value != math.Trunc(value) {
		return 0, false
	}
	return int(value), true
}

// reasoningTokenCount reads a usage object's reasoning tokens: Codex's
// `reasoning_output_tokens`, or the `thinking_tokens` (or OpenAI-style
// `reasoning_tokens`) inside Claude Code's `output_tokens_details`. They are
// part of the output tokens, not in addition to them.
func reasoningTokenCount(source map[string]any) (int, bool) {
	if value, ok := tokenCount(source["reasoning_output_tokens"]); ok {
		return value, true
	}
	if details, ok := source["output_tokens_details"].(map[string]any); ok {
		for _, key := range []string{"thinking_tokens", "reasoning_tokens"} {
			if value, ok := tokenCount(details[key]); ok {
				return value, true
			}
		}
	}
	return 0, false
}

// addTokenCount adds one count to a field that stays nil until some record
// reports it, so "no accounting" is never published as zero tokens. Each
// count is at most maxTokenCount, so the sum of two fits in an int; the total
// saturates there, so enough hostile records can neither overflow it negative
// nor break the schema's bound (parser 0.11.0; 0.10.0 overflowed after 1,025).
func addTokenCount(target **int, value int) {
	total := value
	if *target != nil {
		total = min(total+**target, maxTokenCount)
	}
	*target = &total
}

// addUsage adds one usage object's counts to out. Of the several names a
// field goes by, the first one present with a valid count is read.
func addUsage(out *TokenUsage, source map[string]any) {
	add := func(target **int, keys ...string) {
		for _, key := range keys {
			if value, ok := tokenCount(source[key]); ok {
				addTokenCount(target, value)
				return
			}
		}
	}
	add(&out.Input, "input_tokens", "prompt_tokens")
	add(&out.Output, "output_tokens", "completion_tokens")
	add(&out.CacheRead, "cache_read_input_tokens", "cached_input_tokens")
	add(&out.CacheWrite, "cache_creation_input_tokens", "cache_write_input_tokens")
	if value, ok := reasoningTokenCount(source); ok {
		addTokenCount(&out.Reasoning, value)
	}
}

// usage sums the collected accounting, session-wide and per model (sorted by
// model id, UnknownModel for accounting no model was named on). A field stays
// nil until some record reports it, and a record that reports no count makes
// no per-model entry.
func (t *tokenTotals) usage() (TokenUsage, []ModelTokens) {
	var total TokenUsage
	byModel := map[string]*TokenUsage{}
	sources := append([]modelUsage(nil), t.anonymous...)
	for _, source := range t.byMessage {
		sources = append(sources, source)
	}
	for _, source := range sources {
		var one TokenUsage
		addUsage(&one, source.usage)
		if one == (TokenUsage{}) {
			continue
		}
		addUsage(&total, source.usage)
		model := boundModelName(source.model)
		if model == "" {
			model = UnknownModel
		}
		if byModel[model] == nil {
			byModel[model] = &TokenUsage{}
		}
		addUsage(byModel[model], source.usage)
	}
	foldExtraModels(byModel)
	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)
	var split []ModelTokens
	for _, model := range models {
		u := byModel[model]
		split = append(split, ModelTokens{
			Model: model, InputTokens: u.Input, OutputTokens: u.Output,
			CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite, ReasoningTokens: u.Reasoning,
		})
	}
	return total, split
}

// boundModelName caps a model id at maxModelNameRunes runes, since the filter
// bounds a string only at 64 KB and a hostile transcript could name a new one
// on every record. A real model id is far shorter.
func boundModelName(name string) string {
	runes := []rune(name)
	if len(runes) <= maxModelNameRunes {
		return name
	}
	return string(runes[:maxModelNameRunes-1]) + "…"
}

// foldExtraModels keeps the per-model split to MaxModelTokens entries without
// losing a token: the models with the most tokens stay, and the rest are added
// together under OtherModels, so the split still sums to the session's counts.
// Which stay does not depend on map order (most tokens first, then name).
func foldExtraModels(byModel map[string]*TokenUsage) {
	if len(byModel) <= MaxModelTokens {
		return
	}
	// A model's weight is its tokens, reasoning excluded (it is inside
	// Output). Each count is at most maxTokenCount, so four of them cannot
	// overflow an int.
	weight := func(u *TokenUsage) int {
		sum := 0
		for _, count := range []*int{u.Input, u.Output, u.CacheRead, u.CacheWrite} {
			if count != nil {
				sum += *count
			}
		}
		return sum
	}
	names := make([]string, 0, len(byModel))
	for name := range byModel {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if wi, wj := weight(byModel[names[i]]), weight(byModel[names[j]]); wi != wj {
			return wi > wj
		}
		return names[i] < names[j]
	})
	var folded TokenUsage
	for _, name := range names[MaxModelTokens-1:] {
		addTokenUsage(&folded, *byModel[name])
		delete(byModel, name)
	}
	if byModel[OtherModels] == nil {
		byModel[OtherModels] = &TokenUsage{}
	}
	addTokenUsage(byModel[OtherModels], folded)
}

// addTokenUsage adds every count src reports to dst, saturating like
// addTokenCount.
func addTokenUsage(dst *TokenUsage, src TokenUsage) {
	for _, pair := range []struct {
		to   **int
		from *int
	}{{&dst.Input, src.Input}, {&dst.Output, src.Output}, {&dst.CacheRead, src.CacheRead}, {&dst.CacheWrite, src.CacheWrite}, {&dst.Reasoning, src.Reasoning}} {
		if pair.from != nil {
			addTokenCount(pair.to, *pair.from)
		}
	}
}

// tokenModel is the model a record's token accounting belongs to: for Codex
// the model of the latest turn_context (the token record itself names none),
// for any other harness the model the record names. "" when there is none, or
// only a placeholder.
func tokenModel(bundle SourceBundle, record map[string]any, codexModel string) string {
	if bundle.harness() == HarnessCodex && codexModel != "" && !isPlaceholderModel(codexModel) {
		return codexModel
	}
	return recordModel(record)
}

// accumulateTokens records one record's token accounting: Claude stamps
// `message.usage` on each assistant record and Codex writes `turn_token_usage`
// on each token_usage_record. Cumulative (`total_token_usage`) and thread-wide
// figures are deliberately ignored, so the sum stays additive across records.
// The accounting is attributed to the `id` of the object that carries it
// (Claude's `message.id`), which is what lets repeated streamed records of one
// message count once, and to the model given.
func accumulateTokens(record map[string]any, model string, totals *tokenTotals) {
	usage, owner := firstMapDeepOwner(record, "usage")
	if usage == nil {
		usage, owner = firstMapDeepOwner(record, "turn_token_usage")
	}
	if usage == nil {
		return
	}
	totals.observe(usage, firstString(owner, "id"), model)
}

// nativeTurnEnd reads Cursor's own end-of-turn record. Cursor has no lifecycle
// hook for this, so without it a Cursor session's outcome stays unknown even
// though the transcript states it.
func nativeTurnEnd(bundle SourceBundle) (MetadataState, TurnOutcome, bool) {
	if bundle.harness() != "cursor" {
		return "", "", false
	}
	state, outcome, found := MetadataStateUnknown, TurnOutcomeUnknown, false
	for _, record := range bundle.NativeRecords {
		if strings.ToLower(strings.TrimSpace(firstString(record, "type"))) != "turn_ended" {
			continue
		}
		// The turn is over whatever it reported; a later record wins.
		state, found = MetadataStateIdle, true
		switch strings.ToLower(strings.TrimSpace(firstString(record, "status"))) {
		case "completed":
			outcome = TurnOutcomeCompleted
		case "aborted", "cancelled", "canceled", "interrupted":
			outcome = TurnOutcomeInterrupted
		case "error", "failed":
			outcome = TurnOutcomeError
		default:
			outcome = TurnOutcomeUnknown
		}
	}
	return state, outcome, found
}

// BuildMetadata derives a replaceable metadata sidecar. If parsing fails, it
// returns failed minimal metadata together with a ParseError; callers should
// still publish the verified filtered source and retry a parser upgrade later.
func BuildMetadata(bundle SourceBundle, machineID string, startedAt, derivedAt time.Time, reference SourceReference, parser ParserInfo) (Metadata, error) {
	if err := validateMetadataInputs(bundle, machineID, startedAt, derivedAt, reference); err != nil {
		return Metadata{}, err
	}
	parser = defaultMetadataParser(bundle, parser)
	view, parseErr := ParseNormalized(bundle)
	if parseErr != nil {
		parser.Status = ParserStatusFailed
	}
	metadata := baseMetadata(bundle, machineID, startedAt, derivedAt, reference, parser)
	if parseErr != nil {
		return metadata, parseErr
	}
	return assembleParsedMetadata(bundle, view, metadata), nil
}

func validateMetadataInputs(bundle SourceBundle, machineID string, startedAt, derivedAt time.Time, reference SourceReference) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	if strings.TrimSpace(machineID) == "" || startedAt.IsZero() || derivedAt.IsZero() {
		return errors.New("machine ID, start time, and derivation time are required")
	}
	if strings.TrimSpace(reference.Key) == "" || len(reference.SHA256) != 64 || reference.CompressedBytes < 0 {
		return errors.New("verified source reference is required")
	}
	return nil
}

func defaultMetadataParser(bundle SourceBundle, parser ParserInfo) ParserInfo {
	if parser.Name == "" {
		parser.Name = bundle.Capture.AdapterName
	}
	if parser.Version == "" {
		parser.Version = DefaultParserVersion
	}
	if parser.Status == "" {
		parser.Status = ParserStatusPartial
	}
	return parser
}

func baseMetadata(bundle SourceBundle, machineID string, startedAt, derivedAt time.Time, reference SourceReference, parser ParserInfo) Metadata {
	state, outcome := deriveLifecycle(bundle.SupplementalEvidence)
	return Metadata{
		SchemaVersion: MetadataSchemaVersion, SessionID: bundle.ArchiveSessionID, NativeSessionID: bundle.NativeSessionID,
		MachineID: machineID, ProjectID: bundle.ProjectID, StartedAt: startedAt.UTC(), CapturedAt: bundle.Capture.CapturedAt.UTC(),
		MetadataDerivedAt: derivedAt.UTC(), Harness: bundle.Capture.Harness,
		Adapter: AdapterInfo{Name: bundle.Capture.AdapterName, Version: bundle.Capture.AdapterVersion}, Parser: parser,
		FilterVersion: bundle.Capture.FilterVersion, State: state, TurnOutcome: outcome,
		SemanticConventions: &SemanticConventionsInfo{Name: "OpenTelemetry GenAI semantic conventions", Revision: OpenTelemetryGenAIRevision},
		SkillDetection:      SkillDetectionUnavailable,
		CaptureGaps:         append([]CaptureGap(nil), bundle.Capture.Gaps...), SourceBundle: reference,
		ParentSessionID: bundle.ParentSessionID,
		LinkedSessions:  append([]LinkedSessionReference(nil), bundle.LinkedSessions...),
	}
}

func assembleParsedMetadata(bundle SourceBundle, view NormalizedView, metadata Metadata) Metadata {
	// A native end-of-turn record fills in only what the hook evidence could
	// not establish: an observed hook stop, interrupt, or closure still wins.
	if state, outcome, found := nativeTurnEnd(bundle); found {
		if metadata.State == MetadataStateUnknown {
			metadata.State = state
		}
		if metadata.TurnOutcome == TurnOutcomeUnknown {
			metadata.TurnOutcome = outcome
		}
	}
	prompts, messages, shellCommands, models := summarizeTurns(view.Turns)
	// Native text has unproven structure, so structured counts remain unknown.
	if len(bundle.NativeText) == 0 {
		metadata.Counts = structuredCounts(bundle, view, prompts, messages, shellCommands)
		metadata.ToolsUsed = deriveToolsUsed(view.ToolCalls, workspaceRoot(bundle))
		metadata.MCPCalls = deriveMCPCalls(view.ToolCalls, workspaceRoot(bundle))
		metadata.ModelTokens = view.ModelTokens
	}
	metadata.EndedAt = deriveEndedAt(view, metadata.StartedAt)
	metadata.Models = models
	deriveHookModels(bundle, &metadata)
	deriveSkills(bundle, view.NativeSkillUses, &metadata)
	metadata.Title = deriveSessionTitle(view, bundle.NativeText)
	feedback := 0
	for _, e := range bundle.SupplementalEvidence {
		if e.Kind == EvidenceKindExplicitFeedback {
			feedback++
		}
	}
	metadata.Counts.ExplicitFeedback = &feedback
	return metadata
}

func summarizeTurns(turns []NormalizedTurn) (prompts, messages, shellCommands int, summaries []ModelSummary) {
	assistantMessages := map[string]bool{}
	models := map[string]*ModelSummary{}
	for _, turn := range turns {
		switch turn.Kind {
		case TurnKindHumanPrompt:
			prompts++
			messages++
		case TurnKindAssistant:
			// One streamed response is several records sharing message.id:
			// it is one message, and one turn for its model.
			if turn.MessageID != "" {
				if assistantMessages[turn.MessageID] {
					continue
				}
				assistantMessages[turn.MessageID] = true
			}
			messages++
		case TurnKindShellCommand:
			shellCommands++
			continue
		case TurnKindToolResult, TurnKindHarnessMeta, TurnKindCommandOutput, TurnKindLocalCommand,
			TurnKindCompactSummary, TurnKindHarnessNotification:
			// A tool result, command output, an unanswered slash command, or a
			// harness-written record is not a message any author sent.
			continue
		default:
			// A kind added later is not counted either.
			continue
		}
		addTurnModel(models, turn)
	}
	modelKeys := make([]string, 0, len(models))
	for key := range models {
		modelKeys = append(modelKeys, key)
	}
	sort.Strings(modelKeys)
	for _, key := range modelKeys {
		summaries = append(summaries, *models[key])
	}
	return prompts, messages, shellCommands, summaries
}

func addTurnModel(models map[string]*ModelSummary, turn NormalizedTurn) {
	//lint:ignore LV1001 roles are copied from native records, an external and open vocabulary
	if turn.Role != "user" && turn.Role != "assistant" {
		return
	}
	modelName, attribute, responseStatus := turn.Model, "gen_ai.request.model", ResponseModelStatusNotExposed
	if modelName == "" && turn.ResponseModel != "" {
		modelName, attribute, responseStatus = turn.ResponseModel, "gen_ai.response.model", ResponseModelStatusObserved
	}
	if modelName == "" {
		return
	}
	key := attribute + "\x00" + turn.Provider + "\x00" + modelName + "\x00" + turn.Reasoning
	model := models[key]
	if model == nil {
		attributes := map[string]string{attribute: modelName}
		if turn.Provider != "" {
			attributes["gen_ai.provider.name"] = turn.Provider
		}
		if turn.Reasoning != "" {
			attributes["agent_archive.request.reasoning_level"] = turn.Reasoning
		}
		model = &ModelSummary{Attributes: attributes, Source: ModelSummarySourceNativeTranscript, ResponseModelStatus: responseStatus}
		models[key] = model
	}
	if model.TurnCount == nil {
		zero := 0
		model.TurnCount = &zero
	}
	*model.TurnCount++
}

func structuredCounts(bundle SourceBundle, view NormalizedView, prompts, messages, shellCommands int) Counts {
	toolCalls, toolResults := len(view.ToolCalls), len(view.ToolResults)
	filesTouched := len(sessionFilesTouched(view.ToolCalls, workspaceRoot(bundle)))
	var compactions *int
	if compactionsObservable(bundle) {
		// Count boundaries; use summaries only when no boundary was retained.
		count := view.CompactBoundaries
		if count == 0 {
			count = view.CompactSummaries
		}
		compactions = &count
	}
	var toolErrors *int
	if toolErrorsObservable(bundle) {
		count := 0
		for _, result := range view.ToolResults {
			if result.IsError {
				count++
			}
		}
		toolErrors = &count
	}
	return Counts{
		Turns: &prompts, Messages: &messages, ToolCalls: &toolCalls, ToolResults: &toolResults,
		UserShellCommands: &shellCommands, Compactions: compactions, FilesTouched: &filesTouched,
		InputTokens: view.Tokens.Input, OutputTokens: view.Tokens.Output,
		CacheReadTokens: view.Tokens.CacheRead, CacheWriteTokens: view.Tokens.CacheWrite,
		ReasoningTokens: view.Tokens.Reasoning, ToolErrors: toolErrors,
	}
}

// toolErrorsObservable reports whether a bundle's tool results say whether
// they failed: Claude Code writes is_error on a tool_result, and Cursor's
// adapter sets it on a tool that reported an error. Codex writes no such flag,
// so its failed calls cannot be told from the rest and the count is unknown,
// not zero.
func toolErrorsObservable(bundle SourceBundle) bool {
	switch bundle.harness() {
	case HarnessClaude, HarnessCursor:
		return true
	default:
		return false
	}
}

// MaxToolsUsed is the most entries Metadata.ToolsUsed holds: the session's
// most-called tools. The schema's tools_used maxItems matches it.
const MaxToolsUsed = 10

// toolNameLimit is the maximum rune length of a ToolUsage name, ellipsis
// included. The schema's maxLength matches it.
const toolNameLimit = 128

// deriveToolsUsed counts the calls handoff lists (listedCall), under the
// names it lists them by, and keeps the MaxToolsUsed most-called, by count
// descending and then name ascending. A nameless call is counted under its
// record type (a Codex local_shell_call, or a CommandExecution completion no
// invocation reported); a nameless completion with nothing to show is left
// out, as handoff leaves it out, though it is still one of counts.tool_calls.
// An invocation and its completion echo are one call (dedupeToolCalls).
func deriveToolsUsed(calls []NormalizedToolCall, root string) []ToolUsage {
	counts := map[string]int{}
	for _, call := range calls {
		listedName, _, listed := listedCall(call, root)
		if !listed {
			continue
		}
		if name := metadataToolName(listedName); name != "" {
			counts[name]++
		}
	}
	return rankToolUsage(counts, MaxToolsUsed)
}

// MaxMCPCalls is the most entries Metadata.MCPCalls holds. The schema's
// mcp_calls maxItems matches it.
const MaxMCPCalls = 50

// mcpToolPrefix and mcpNameSeparator frame an MCP tool's harness-side name,
// mcp__<server>__<tool>.
const (
	mcpToolPrefix    = "mcp__"
	mcpNameSeparator = "__"
)

// mcpServer returns the server an MCP tool name (mcp__<server>__<tool>)
// belongs to, and false for any other name or one with no server or no tool.
func mcpServer(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, mcpToolPrefix)
	if !ok {
		return "", false
	}
	server, tool, ok := strings.Cut(rest, mcpNameSeparator)
	if !ok || server == "" || tool == "" {
		return "", false
	}
	return server, true
}

// deriveMCPCalls counts the same calls as deriveToolsUsed (each counted once,
// echoes included) by the MCP server they went to, by count descending and
// then name ascending, at most MaxMCPCalls. Only a name of the form
// mcp__<server>__<tool> is an MCP call: Codex's McpToolCall completion names
// the tool but not the server in what the filter retains, so those are not
// attributed.
func deriveMCPCalls(calls []NormalizedToolCall, root string) []ToolUsage {
	counts := map[string]int{}
	for _, call := range calls {
		listedName, _, listed := listedCall(call, root)
		if !listed {
			continue
		}
		if server, ok := mcpServer(listedName); ok {
			if name := metadataToolName(server); name != "" {
				counts[name]++
			}
		}
	}
	return rankToolUsage(counts, MaxMCPCalls)
}

// rankToolUsage orders counted names by count descending, then name
// ascending, and keeps the first limit. Nil when nothing was counted.
func rankToolUsage(counts map[string]int, limit int) []ToolUsage {
	if len(counts) == 0 {
		return nil
	}
	out := make([]ToolUsage, 0, len(counts))
	for name, count := range counts {
		out = append(out, ToolUsage{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// metadataToolName makes a harness-reported tool name safe to publish in
// metadata. The name was read from filtered source, so its credentials are
// already redacted; it is redacted again anyway (a no-op on filtered text),
// stripped of control characters, collapsed onto one line, and capped at
// toolNameLimit runes, since the filter bounds a string only at 64 KB. An
// MCP name such as mcp__server__tool is kept whole when it fits.
func metadataToolName(name string) string {
	fields := strings.Fields(displayText(RedactText(name)))
	if len(fields) == 0 {
		return ""
	}
	line := strings.Join(fields, " ")
	runes := []rune(line)
	if len(runes) <= toolNameLimit {
		return line
	}
	return string(runes[:toolNameLimit-1]) + "…"
}

// deriveEndedAt is the latest record timestamp, raised to startedAt when the
// records' clock ran behind the one that set it (a hook's registration
// time), so ended_at is never before started_at. Nil when no record carried a
// timestamp: an end time is never invented.
func deriveEndedAt(view NormalizedView, startedAt time.Time) *time.Time {
	if view.LatestRecordAt.IsZero() {
		return nil
	}
	ended := view.LatestRecordAt
	if ended.Before(startedAt) {
		ended = startedAt
	}
	ended = ended.UTC()
	return &ended
}

// sessionTitleLimit is the maximum rune length of Metadata.Title.
const sessionTitleLimit = 72

// deriveSessionTitle returns a one-line preview of the first human prompt:
// from normalized JSONL turns when present, otherwise from the first user
// section of a filtered NativeText transcript (Cursor text sessions).
func deriveSessionTitle(view NormalizedView, texts []TextTranscript) string {
	for _, turn := range view.Turns {
		if turn.Kind != TurnKindHumanPrompt {
			continue
		}
		if title := collapseSessionTitle(turn.Text); title != "" {
			return title
		}
	}
	for _, transcript := range texts {
		parsed, _ := parseTextSections(transcript.Content)
		for _, section := range parsed.sections {
			if section.role != textRoleUser {
				continue
			}
			parts := make([]string, 0, len(section.lines))
			if header := strings.TrimSpace(section.header); header != "" {
				parts = append(parts, header)
			}
			if len(section.lines) > 1 {
				parts = append(parts, section.lines[1:]...)
			}
			if title := collapseSessionTitle(strings.Join(parts, "\n")); title != "" {
				return title
			}
		}
	}
	return ""
}

// collapseSessionTitle flattens whitespace to a single line and caps length.
func collapseSessionTitle(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	line := strings.Join(fields, " ")
	runes := []rune(line)
	if len(runes) <= sessionTitleLimit {
		return line
	}
	return string(runes[:sessionTitleLimit]) + "…"
}

// ApplyProjectName sets ProjectName from the basename of projectRoot.
// Callers pass the registration's ProjectRoot at publish time.
func (m *Metadata) ApplyProjectName(projectRoot string) {
	base := filepath.Base(filepath.Clean(strings.TrimSpace(projectRoot)))
	if base == "" {
		return
	}
	if base == "." {
		return
	}
	if base == string(filepath.Separator) {
		return
	}
	m.ProjectName = base
}

// ApplyRepoKey sets RepoKey from key, the registration's or the one the
// collector derived from the project's origin remote. A key that is not the
// shape RepoKey returns is dropped, so nothing else (a URL, say) can reach
// the sidecar through this field.
func (m *Metadata) ApplyRepoKey(key string) {
	if !IsRepoKey(key) {
		return
	}
	m.RepoKey = key
}

type lifecycleObservation struct {
	at         time.Time
	event      lifecycleEvent
	status     lifecycleStatus
	provenance string
	payloadKey string
}

// lifecycleEvent is a hook event name, lowercased with underscores removed.
// Only the events below mean anything to deriveLifecycle; any other name is
// kept as observed and ignored.
type lifecycleEvent string

const (
	lifecycleSessionStart       lifecycleEvent = "sessionstart"
	lifecycleUserPromptSubmit   lifecycleEvent = "userpromptsubmit"
	lifecycleBeforeSubmitPrompt lifecycleEvent = "beforesubmitprompt"
	lifecycleStop               lifecycleEvent = "stop"
	lifecycleInterrupt          lifecycleEvent = "interrupt"
	lifecycleStopFailure        lifecycleEvent = "stopfailure"
	lifecycleSessionEnd         lifecycleEvent = "sessionend"
	lifecycleSubagentStop       lifecycleEvent = "subagentstop"
)

// lifecycleStatus is a hook payload's lowercased status. Only Cursor's
// documented values below are read.
type lifecycleStatus string

const (
	lifecycleStatusCompleted lifecycleStatus = "completed"
	lifecycleStatusAborted   lifecycleStatus = "aborted"
	lifecycleStatusError     lifecycleStatus = "error"
)

// deriveLifecycle applies conservative rules to filtered hook evidence.
// Sorting makes duplicate, delayed, and out-of-order delivery deterministic.
// Evidence captured before event_name was retained stays unknown rather than
// being reconstructed from local operational request state.
func deriveLifecycle(evidence []SupplementalEvidence) (MetadataState, TurnOutcome) {
	var observations []lifecycleObservation
	for _, item := range evidence {
		if item.Kind != EvidenceKindLifecycleHook {
			continue
		}
		event := lifecycleEvent(strings.ToLower(strings.ReplaceAll(firstString(item.Payload, "event_name"), "_", "")))
		if event == "" {
			continue
		}
		payload, _ := json.Marshal(item.Payload)
		observations = append(observations, lifecycleObservation{
			at: item.ObservedAt, event: event, status: lifecycleStatus(strings.ToLower(firstString(item.Payload, "status"))),
			provenance: item.Provenance, payloadKey: string(payload),
		})
	}
	sort.Slice(observations, func(i, j int) bool {
		if !observations[i].at.Equal(observations[j].at) {
			return observations[i].at.Before(observations[j].at)
		}
		if lifecycleRank(observations[i].event) != lifecycleRank(observations[j].event) {
			return lifecycleRank(observations[i].event) < lifecycleRank(observations[j].event)
		}
		left := string(observations[i].event) + "\x00" + string(observations[i].status) + "\x00" + observations[i].provenance + "\x00" + observations[i].payloadKey
		right := string(observations[j].event) + "\x00" + string(observations[j].status) + "\x00" + observations[j].provenance + "\x00" + observations[j].payloadKey
		return left < right
	})
	state, outcome := MetadataStateUnknown, TurnOutcomeUnknown
	for _, observation := range observations {
		switch observation.event {
		case lifecycleSessionStart, lifecycleUserPromptSubmit, lifecycleBeforeSubmitPrompt:
			state, outcome = MetadataStateActive, TurnOutcomeUnknown
		case lifecycleStop:
			state, outcome = MetadataStateIdle, documentedLifecycleOutcome(observation.event, observation.status, observation.provenance)
		case lifecycleInterrupt:
			state, outcome = MetadataStateIdle, TurnOutcomeInterrupted
		case lifecycleStopFailure:
			state, outcome = MetadataStateIdle, TurnOutcomeError
		case lifecycleSessionEnd:
			state = MetadataStateClosed
			// Closing the session does not undo a previously observed turn outcome.
			if observed := documentedLifecycleOutcome(observation.event, observation.status, observation.provenance); observed != TurnOutcomeUnknown {
				outcome = observed
			}
		case lifecycleSubagentStop:
			// A subagent finishing says nothing about the parent session, which
			// is still running: its state and outcome are left as observed.
		}
	}
	return state, outcome
}

func lifecycleRank(event lifecycleEvent) int {
	switch event {
	case lifecycleSessionStart, lifecycleUserPromptSubmit, lifecycleBeforeSubmitPrompt:
		return 1
	case lifecycleStop, lifecycleInterrupt, lifecycleStopFailure:
		return 2
	case lifecycleSessionEnd, lifecycleSubagentStop:
		return 3
	default:
		return 0
	}
}

func documentedLifecycleOutcome(event lifecycleEvent, status lifecycleStatus, provenance string) TurnOutcome {
	if event == lifecycleStop && provenance != "hook:cursor:stop" {
		return TurnOutcomeUnknown
	}
	if event == lifecycleSessionEnd && provenance != "hook:cursor:sessionend" {
		return TurnOutcomeUnknown
	}
	switch status {
	case lifecycleStatusCompleted:
		return TurnOutcomeCompleted
	case lifecycleStatusAborted:
		return TurnOutcomeInterrupted
	case lifecycleStatusError:
		return TurnOutcomeError
	default:
		return TurnOutcomeUnknown
	}
}

func deriveHookModels(bundle SourceBundle, metadata *Metadata) {
	seen := map[string]bool{}
	for _, evidence := range bundle.SupplementalEvidence {
		if evidence.Kind != EvidenceKindLifecycleHook && evidence.Kind != EvidenceKindFinalResponse {
			continue
		}
		id, label := firstString(evidence.Payload, "model_id"), firstString(evidence.Payload, "model")
		if id == "" {
			continue
		}
		key := id + "\x00" + label
		if seen[key] {
			continue
		}
		seen[key] = true
		attrs := map[string]string{"gen_ai.request.model": id}
		if label != "" {
			attrs["agent_archive.request.model_label"] = label
		}
		if params, ok := evidence.Payload["model_params"].([]any); ok {
			for _, raw := range params {
				if p, ok := raw.(map[string]any); ok {
					if name, value := firstString(p, "id"), firstString(p, "value"); name != "" {
						attrs["agent_archive.request.setting."+name] = value
					}
				}
			}
		}
		metadata.Models = append(metadata.Models, ModelSummary{Attributes: attrs, Source: ModelSummarySourceHook, ResponseModelStatus: ResponseModelStatusNotExposed})
	}
}

func deriveSkills(bundle SourceBundle, nativeSkillUses []SkillUse, metadata *Metadata) {
	available := map[string]SkillSnapshot{}
	used := map[string]SkillUse{}
	recordUse := func(entry SkillUse) {
		if entry.SHA256 == "" {
			for _, existing := range used {
				if existing.Name == entry.Name && existing.SHA256 != "" {
					return
				}
			}
		} else {
			delete(used, entry.Name+"\x00")
		}
		key := entry.Name + "\x00" + entry.SHA256
		if existing, ok := used[key]; ok && existing.Evidence == SkillUseEvidenceNativeInvocation {
			return
		}
		used[key] = entry
	}
	for _, evidence := range bundle.SupplementalEvidence {
		name := firstString(evidence.Payload, "name")
		if name == "" && evidence.Kind != EvidenceKindSkillInventory {
			continue
		}
		hash := firstString(evidence.Payload, "sha256")
		switch evidence.Kind {
		case EvidenceKindSkillInventory, EvidenceKindSkillDiscovered, EvidenceKindSkillSnapshot:
			coverage := SkillCoverage(firstString(evidence.Payload, "coverage"))
			if skills, ok := evidence.Payload["skills"].([]any); ok {
				for _, raw := range skills {
					if skill, ok := raw.(map[string]any); ok {
						n, h := firstString(skill, "name"), firstString(skill, "sha256")
						if n != "" {
							available[n+"\x00"+h] = SkillSnapshot{Name: n, SHA256: h, Coverage: coverage}
						}
					}
				}
			} else {
				available[name+"\x00"+hash] = SkillSnapshot{Name: name, SHA256: hash, Coverage: coverage}
			}
		case EvidenceKindSkillInvocation:
			recordUse(SkillUse{Name: name, SHA256: hash, Evidence: SkillUseEvidenceNativeInvocation})
		case EvidenceKindSkillRead:
			recordUse(SkillUse{Name: name, SHA256: hash, Evidence: SkillUseEvidenceReadInference})
		case EvidenceKindLifecycleHook, EvidenceKindFinalResponse, EvidenceKindExplicitFeedback,
			EvidenceKindLinkedSession, EvidenceKindCaptureGap:
		}
	}
	// Native invocation/read-inference evidence is collected once, in
	// toolCalls()'s per-record walk (see ParseNormalized), rather than a
	// second traversal of bundle.NativeRecords here.
	for _, entry := range nativeSkillUses {
		recordUse(entry)
	}
	for _, entry := range available {
		metadata.SkillsAvailable = append(metadata.SkillsAvailable, entry)
	}
	for _, entry := range used {
		metadata.SkillsUsed = append(metadata.SkillsUsed, entry)
	}
	if len(metadata.SkillsUsed) > 0 {
		metadata.SkillDetection = SkillDetectionObserved
	} else {
		for _, entry := range metadata.SkillsAvailable {
			if entry.Coverage == SkillCoverageEligible || entry.Coverage == SkillCoverageDiscovered {
				metadata.SkillDetection = SkillDetectionPartial
				break
			}
		}
	}
	sort.Slice(metadata.SkillsAvailable, func(i, j int) bool {
		return metadata.SkillsAvailable[i].Name+"\x00"+metadata.SkillsAvailable[i].SHA256 < metadata.SkillsAvailable[j].Name+"\x00"+metadata.SkillsAvailable[j].SHA256
	})
	sort.Slice(metadata.SkillsUsed, func(i, j int) bool {
		return metadata.SkillsUsed[i].Name+"\x00"+metadata.SkillsUsed[i].SHA256 < metadata.SkillsUsed[j].Name+"\x00"+metadata.SkillsUsed[j].SHA256
	})
}

func skillNameFromPath(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	marker := "/SKILL.md"
	index := strings.Index(value, marker)
	if index < 1 {
		return ""
	}
	before := strings.TrimSuffix(value[:index], "/")
	parts := strings.Split(before, "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return ""
	}
	return parts[len(parts)-1]
}

// ValidateSourceReference checks that metadata has the supported schema
// version and names its source bundle: an object key and a 64-character
// SHA-256.
func (m *Metadata) ValidateSourceReference() error {
	if m.SchemaVersion != MetadataSchemaVersion {
		return fmt.Errorf("unsupported metadata schema version %d", m.SchemaVersion)
	}
	if m.SourceBundle.Key == "" || len(m.SourceBundle.SHA256) != 64 {
		return errors.New("metadata has no verified source reference")
	}
	return nil
}
