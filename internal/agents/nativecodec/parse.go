// Package nativecodec contains pure native format interpretation shared by built-in agents.
package nativecodec

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"path"
	"strconv"
	"strings"
	"time"
)

type nativeProfile string

const (
	profileClaude nativeProfile = "claude"
	profileCodex  nativeProfile = "codex"
	profileCursor nativeProfile = "cursor"
)

func parse(ctx context.Context, bundle archive.SourceBundle, agent nativeProfile) (archive.Analysis, error) {
	if err := archive.ValidateSourceBundle(bundle); err != nil {
		return archive.Analysis{}, &archive.ParseError{Reason: err.Error()}
	}
	view := archive.NormalizedView{}
	analysis := archive.Analysis{}
	setAvailability(&analysis, bundle, agent)
	isParentBundle := bundle.ParentSessionID == ""
	var codexModel, codexReasoning string
	var candidates []toolCallCandidate
	tokens := archive.TokenAccumulator{}
	for i, record := range bundle.NativeRecords {
		if err := ctx.Err(); err != nil {
			return archive.Analysis{}, err
		}
		collectFacts(&analysis.Facts, bundle, record, agent)
		if isParentBundle && isSidechainRecord(record) {
			// A subagent's records are archived as the child's own session.
			// Older Claude layouts inline them in the parent transcript; the
			// parent must not count the same messages, turns, and tool calls
			// a second time.
			continue
		}
		if at := latestRecordTime(bundle, record); at.After(view.LatestRecordAt) {
			view.LatestRecordAt = at
		}
		if agent == profileCodex && firstString(record, "type") == "turn_context" {
			codexModel, codexReasoning = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort")
			continue
		}
		if isCompactBoundary(record) {
			// A marker only: filter 5 keeps its ids and timestamp, no text.
			view.CompactBoundaries++
			continue
		}
		if !isPlaceholderModel(firstStringDeep(record, "model", "model_id")) {
			accumulateTokens(record, tokenModel(agent, record, codexModel), &tokens)
		}
		calls, results, skillUses := toolActivity(record, i, codexModel, codexReasoning)
		for j := range calls {
			calls[j].Call.RecordedBranch = archive.ValidBranch(firstString(record, "gitBranch"))
			calls[j].Call.RecordedAt = parseNativeTimestamp(record)
		}
		for j := range results {
			results[j].RecordedAt = parseNativeTimestamp(record)
		}
		candidates = append(candidates, calls...)
		view.ToolResults = append(view.ToolResults, results...)
		view.NativeSkillUses = append(view.NativeSkillUses, skillUses...)
		role, text, kind, ok := visibleMessage(record)
		if !ok {
			continue
		}
		if isHiddenRole(role) {
			return archive.Analysis{}, &archive.ParseError{Reason: "hidden role present in filtered source"}
		}
		kind = refineUserKind(record, kind, text)
		if kind == archive.TurnKindCompactSummary {
			view.CompactSummaries++
		}
		var model, responseModel, reasoning string
		var modelSource archive.TurnModelSource
		switch agent {
		case profileCodex:
			model, reasoning, modelSource = codexModel, codexReasoning, archive.TurnModelSourceTurnContext
		case profileClaude:
			responseModel, modelSource = recordModel(record), archive.TurnModelSourceNativeResponse
		default:
			model, reasoning, modelSource = recordModel(record), firstStringDeep(record, "reasoning_effort"), archive.TurnModelSourceNativeTranscript
		}
		view.Turns = append(view.Turns, archive.NormalizedTurn{
			RecordIndex:       i,
			Role:              role,
			Kind:              kind,
			MessageID:         nestedMessageID(record),
			Text:              text,
			PresentationText:  prepareTurnText(kind, text),
			PresentationKnown: true,
			Model:             model,
			ResponseModel:     responseModel,
			ModelSource:       modelSource,
			Provider:          firstStringDeep(record, "model_provider"),
			Reasoning:         reasoning,
			ID:                firstStringDeep(record, "id", "uuid"),
			ParentID:          firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"),
			TurnID:            firstStringDeep(record, "turn_id"),
			Timestamp:         firstStringDeep(record, "timestamp", "created_at"),
		})
	}
	archive.ResolveSlashCommands(view.Turns)
	view.ToolCalls = archive.FinalizeToolCalls(candidates, view.ToolResults)
	view.Tokens, view.ModelTokens = tokens.Usage()
	view.HookFinals = archive.ReconcileHookFinals(bundle, view.Turns)
	analysis.View = view
	if len(bundle.NativeText) > 0 {
		parseText(&analysis, bundle)
	}
	if agent == profileCursor && len(view.ToolResults) == 0 && len(view.ToolCalls) > 0 {
		analysis.Observability.ToolResults = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	}
	return analysis, nil
}

func latestRecordTime(bundle archive.SourceBundle, record map[string]any) time.Time {
	at := parseNativeTimestamp(record)
	if bundle.Capture.SourceFormat == "cursor-composer" {
		if completed, ok := cursorTime(record["completed_at_ms"]); ok && completed.After(at) {
			at = completed
		}
	}
	return at
}

func tokenModel(agent nativeProfile, record map[string]any, codexModel string) string {
	if agent == profileCodex && codexModel != "" && !isPlaceholderModel(codexModel) {
		return codexModel
	}
	return recordModel(record)
}

func accumulateTokens(record map[string]any, model string, totals *archive.TokenAccumulator) {
	usage, owner := firstMapDeepOwner(record, "usage")
	if usage == nil {
		usage, owner = firstMapDeepOwner(record, "turn_token_usage")
	}
	if usage == nil {
		return
	}
	totals.Observe(tokenObservation(usage), firstString(owner, "id"), model)
}

func setAvailability(a *archive.Analysis, b archive.SourceBundle, agent nativeProfile) {
	a.Observability.StructuredCounts = archive.Availability{State: archive.AvailabilityAvailable}
	a.Observability.Compactions = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	a.Observability.ToolErrors = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	a.Observability.ToolResults = archive.Availability{State: archive.AvailabilityAvailable}
	if agent == profileClaude {
		version, err := strconv.Atoi(strings.TrimSpace(b.Capture.FilterVersion))
		if err == nil && version >= 5 {
			a.Observability.Compactions = archive.Availability{State: archive.AvailabilityAvailable}
		} else {
			a.Observability.Compactions.Reason = archive.AvailabilityReasonHistoricalFilter
		}
	}
	if agent == profileClaude || agent == profileCursor {
		a.Observability.ToolErrors = archive.Availability{State: archive.AvailabilityAvailable}
	}
	if len(b.NativeText) > 0 {
		a.Facts.Text = true
		a.Observability.StructuredCounts = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonText}
	}
}

func collectFacts(f *archive.NativeFacts, b archive.SourceBundle, r map[string]any, agent nativeProfile) {
	kind := firstString(r, "type")
	var name string
	switch {
	case kind == "custom-title":
		name = firstString(r, "customTitle")
	case kind == "subagent-meta":
		name = firstString(r, "description")
	case kind == "session" && b.Capture.SourceFormat == "cursor-composer":
		name = firstString(r, "name")
	}
	if name = archive.CollapseSessionTitle(name); name != "" {
		f.Name = name
	}
	if f.WorkspaceRoot == "" {
		if cwd := firstStringDeep(r, "cwd"); cwd != "" {
			f.WorkspaceRoot = path.Clean(cwd)
		}
	}
	if branch := firstStringDeep(r, "gitBranch"); branch != "" {
		f.Branch = branch
	}
	collectPullRequest(f, r)
	if agent == profileCursor && strings.ToLower(strings.TrimSpace(kind)) == "turn_ended" {
		f.TurnEnd = archive.NativeTurnEnd{Present: true, State: archive.MetadataStateIdle, Outcome: archive.TurnOutcomeUnknown}
		switch strings.ToLower(strings.TrimSpace(firstString(r, "status"))) {
		case "completed":
			f.TurnEnd.Outcome = archive.TurnOutcomeCompleted
		case "aborted", "cancelled", "canceled", "interrupted":
			f.TurnEnd.Outcome = archive.TurnOutcomeInterrupted
		case "error", "failed":
			f.TurnEnd.Outcome = archive.TurnOutcomeError
		}
	}
	if !isSidechainRecord(r) {
		for _, key := range []string{"sessionId", "session_id"} {
			if id := firstString(r, key); id != "" && id != b.NativeSessionID {
				f.IdentityConflict = true
			}
		}
		if agent == profileCodex && kind == "session_meta" {
			payload, _ := r["payload"].(map[string]any)
			if id, alias := firstString(payload, "id"), firstString(payload, "session_id"); id != b.NativeSessionID || alias != "" && alias != b.NativeSessionID {
				f.IdentityConflict = true
			}
		}
	}
}

func ParseClaude(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	return parse(ctx, b, profileClaude)
}

func ParseCodex(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	return parse(ctx, b, profileCodex)
}

func ParseCursor(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	return parse(ctx, b, profileCursor)
}

func collectPullRequest(f *archive.NativeFacts, r map[string]any) {
	if firstString(r, "type") == "pr-link" && len(f.PullRequests) < archive.MaxPullRequests {
		repository := firstString(r, "prRepository")
		owner, name, ok := splitRepository(repository)
		number, numberOK := claudePRNumber(r["prNumber"])
		if ok && numberOK {
			var url string
			if firstString(r, "prUrl") == claudePRURL(owner, name, number) {
				url = claudePRURL(owner, name, number)
			}
			link := archive.PullRequestLink{Repository: repository, Number: number, URL: url}
			seen := false
			for _, prior := range f.PullRequests {
				if prior.Repository == link.Repository && prior.Number == link.Number {
					seen = true
					break
				}
			}
			if !seen {
				f.PullRequests = append(f.PullRequests, link)
			}
		}
	}
}
