package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
type tokenTotals struct {
	byMessage map[string]map[string]any
	anonymous []map[string]any
}

func (t *tokenTotals) observe(usage map[string]any, messageID string) {
	if messageID == "" {
		t.anonymous = append(t.anonymous, usage)
		return
	}
	if t.byMessage == nil {
		t.byMessage = map[string]map[string]any{}
	}
	t.byMessage[messageID] = usage
}

// usage sums the collected accounting. A field stays nil until some record
// reports it, so "no accounting" is never published as zero tokens.
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

func (t *tokenTotals) usage() TokenUsage {
	var out TokenUsage
	add := func(target **int, source map[string]any, keys ...string) {
		for _, key := range keys {
			value, ok := tokenCount(source[key])
			if !ok {
				continue
			}
			// Each count is at most maxTokenCount, so the sum of two fits in
			// an int; the total saturates there, so enough hostile records
			// can neither overflow it negative nor break the schema's
			// bound (parser 0.11.0; 0.10.0 overflowed after 1,025).
			total := value
			if *target != nil {
				total = min(total+**target, maxTokenCount)
			}
			*target = &total
			return
		}
	}
	sources := append([]map[string]any(nil), t.anonymous...)
	for _, source := range t.byMessage {
		sources = append(sources, source)
	}
	for _, source := range sources {
		add(&out.Input, source, "input_tokens", "prompt_tokens")
		add(&out.Output, source, "output_tokens", "completion_tokens")
		add(&out.CacheRead, source, "cache_read_input_tokens", "cached_input_tokens")
		add(&out.CacheWrite, source, "cache_creation_input_tokens")
	}
	return out
}

// accumulateTokens records one record's token accounting: Claude stamps
// `message.usage` on each assistant record and Codex writes `turn_token_usage`
// on each token_usage_record. Cumulative (`total_token_usage`) and thread-wide
// figures are deliberately ignored, so the sum stays additive across records.
// The accounting is attributed to the `id` of the object that carries it
// (Claude's `message.id`), which is what lets repeated streamed records of one
// message count once.
func accumulateTokens(record map[string]any, totals *tokenTotals) {
	usage, owner := firstMapDeepOwner(record, "usage")
	if usage == nil {
		usage, owner = firstMapDeepOwner(record, "turn_token_usage")
	}
	if usage == nil {
		return
	}
	totals.observe(usage, firstString(owner, "id"))
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
	if err := validateBundle(bundle); err != nil {
		return Metadata{}, err
	}
	if strings.TrimSpace(machineID) == "" || startedAt.IsZero() || derivedAt.IsZero() {
		return Metadata{}, errors.New("machine ID, start time, and derivation time are required")
	}
	if strings.TrimSpace(reference.Key) == "" || len(reference.SHA256) != 64 || reference.CompressedBytes < 0 {
		return Metadata{}, errors.New("verified source reference is required")
	}
	if parser.Name == "" {
		parser.Name = bundle.Capture.AdapterName
	}
	if parser.Version == "" {
		parser.Version = DefaultParserVersion
	}
	if parser.Status == "" {
		parser.Status = ParserStatusPartial
	}
	view, parseErr := ParseNormalized(bundle)
	if parseErr != nil {
		parser.Status = ParserStatusFailed
	}
	state, outcome := deriveLifecycle(bundle.SupplementalEvidence)
	metadata := Metadata{
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
	if parseErr != nil {
		return metadata, parseErr
	}
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
	prompts, messages, shellCommands := 0, 0, 0
	assistantMessages := map[string]bool{}
	models := map[string]*ModelSummary{}
	for _, turn := range view.Turns {
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
		//lint:ignore LV1001 roles are copied from native records, an external and open vocabulary
		if turn.Role != "user" && turn.Role != "assistant" {
			continue
		}
		modelName, attribute, responseStatus := turn.Model, "gen_ai.request.model", ResponseModelStatusNotExposed
		if modelName == "" && turn.ResponseModel != "" {
			modelName, attribute, responseStatus = turn.ResponseModel, "gen_ai.response.model", ResponseModelStatusObserved
		}
		if modelName == "" {
			continue
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
	// Native text is retained precisely because its structure is not proven.
	// Counts derived only from the structured subset would look complete, so
	// leave all structure-dependent totals unknown whenever text is present.
	if len(bundle.NativeText) == 0 {
		toolCalls, toolResults := len(view.ToolCalls), len(view.ToolResults)
		metadata.Counts.Turns = &prompts
		metadata.Counts.Messages = &messages
		metadata.Counts.ToolCalls = &toolCalls
		metadata.Counts.ToolResults = &toolResults
		metadata.Counts.UserShellCommands = &shellCommands
		metadata.Counts.InputTokens, metadata.Counts.OutputTokens = view.Tokens.Input, view.Tokens.Output
		metadata.Counts.CacheReadTokens, metadata.Counts.CacheWriteTokens = view.Tokens.CacheRead, view.Tokens.CacheWrite
		if compactionsObservable(bundle) {
			// Each compaction writes one boundary and one summary. Count
			// boundaries; fall back to summaries only when no boundary was
			// retained at all, so the same compaction is never counted twice.
			compactions := view.CompactBoundaries
			if compactions == 0 {
				compactions = view.CompactSummaries
			}
			metadata.Counts.Compactions = &compactions
		}
	}
	modelKeys := make([]string, 0, len(models))
	for key := range models {
		modelKeys = append(modelKeys, key)
	}
	sort.Strings(modelKeys)
	for _, key := range modelKeys {
		metadata.Models = append(metadata.Models, *models[key])
	}
	deriveHookModels(bundle, &metadata)
	deriveSkills(bundle, view.NativeSkillUses, &metadata)
	feedback := 0
	for _, e := range bundle.SupplementalEvidence {
		if e.Kind == EvidenceKindExplicitFeedback {
			feedback++
		}
	}
	metadata.Counts.ExplicitFeedback = &feedback
	return metadata, nil
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
