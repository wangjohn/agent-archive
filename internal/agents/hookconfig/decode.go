package hookconfig

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"path/filepath"
	"slices"
	"strings"
)

// DecoderSpec supplies native event and source rules from an integration.
type DecoderSpec struct {
	Agent              agentmeta.ID
	Events             map[string]agentapi.EventKind
	PromptStarts       bool
	NullPathFresh      bool
	OwnedFilename      bool
	NativeObservations bool
	ChildTranscript    bool
	Followups          map[string]bool
	StatusFields       map[string]string
	StatusValues       map[string][]string
}

// Decoder is a pure native payload interpreter with injected format declarations.
type Decoder struct{ Spec DecoderSpec }

func first(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func root(payload map[string]any) string {
	if v := first(payload, "cwd"); v != "" {
		return v
	}
	if roots, ok := payload["workspace_roots"].([]any); ok {
		for _, v := range roots {
			if root, ok := v.(string); ok && root != "" {
				return root
			}
		}
	}
	return ""
}

func (d Decoder) freshness(payload map[string]any) agentapi.StartEvidence {
	if d.Spec.NullPathFresh {
		value, present := payload["transcript_path"]
		if !present || value == nil || value == "" {
			return agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: agentapi.FreshnessEmptyPath}
		}
		path, ok := value.(string)
		if !ok {
			return agentapi.StartEvidence{Reason: agentapi.FreshnessInvalidPath}
		}
		return agentapi.StartEvidence{Kind: agentapi.FreshStat, Reason: agentapi.FreshnessInspectPath, Path: path}
	}
	switch strings.ToLower(strings.TrimSpace(first(payload, "source"))) {
	case "startup", "clear":
		return agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: agentapi.FreshnessExplicitStart}
	case "resume", "compact":
		return agentapi.StartEvidence{Kind: agentapi.FreshContinuation, Reason: agentapi.FreshnessContinuation}
	case "":
		return agentapi.StartEvidence{Kind: agentapi.FreshStat, Reason: agentapi.FreshnessInspectPath, Path: first(payload, "transcript_path")}
	default:
		return agentapi.StartEvidence{Reason: agentapi.FreshnessUnknownSource}
	}
}

func (d Decoder) locator(payload map[string]any, id string) string {
	path := first(payload, "transcript_path")
	if !d.Spec.OwnedFilename {
		return path
	}
	if path == "" || id == "" || !filepath.IsAbs(path) {
		return ""
	}
	path = filepath.Clean(path)
	if filepath.Base(path) != id+".jsonl" {
		return ""
	}
	return path
}

// DiagnosticProject interprets native project facts independently of event decoding.
func (d Decoder) DiagnosticProject(input agentapi.HookInput) string {
	return root(input.Payload)
}

// Decode interprets only the events declared by the native integration.
func (d Decoder) Decode(ctx context.Context, input agentapi.HookInput) ([]agentapi.LifecycleEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload := input.Payload
	name := first(payload, "hook_event_name")
	kind, ok := d.Spec.Events[name]
	if !ok {
		return nil, nil
	}
	id := first(payload, "session_id", "conversation_id")
	if id == "" {
		return nil, errors.New("hook identity unavailable")
	}
	version := ""
	mode := agentapi.NativeModeUnspecified
	if d.Spec.NativeObservations {
		version = first(payload, "cursor_version")
		mode = agentapi.NativeMode(first(payload, "composer_mode"))
	}
	locator := agentapi.LocatorNone
	if d.Spec.OwnedFilename && (kind == agentapi.EventStart || kind == agentapi.EventTurnStart || d.Spec.Followups[name]) {
		locator = agentapi.LocatorFillFile
	} else if !d.Spec.OwnedFilename && kind == agentapi.EventStart {
		locator = agentapi.LocatorReplaceFile
	}
	proof := agentapi.StartEvidence{}
	deferred := agentapi.DeferredNone
	if kind == agentapi.EventStart || kind == agentapi.EventTurnStart && d.Spec.PromptStarts {
		proof = d.freshness(payload)
		deferred = agentapi.DeferredStart
	}
	source := agentapi.SourceRef{Path: d.locator(payload, id)}
	if d.Spec.Followups[name] && source.Path != "" {
		deferred = agentapi.DeferredFollowup
	}
	var child *agentapi.ChildObservation
	if kind == agentapi.EventSubagent {
		child = &agentapi.ChildObservation{ID: first(payload, "agent_id"), Path: first(payload, "agent_transcript_path"), Type: archive.SanitizeSubagentType(first(payload, "agent_type")), CaptureTranscript: d.Spec.ChildTranscript, MissingDetail: "SubagentStop omitted agent_id"}
	}
	session := agentapi.NativeSession{Agent: d.Spec.Agent, NativeID: id, Version: version, Mode: mode}
	reason := strings.ToLower(name)
	event := agentapi.LifecycleEvent{Kind: kind, Session: session, ProjectRoot: root(payload), Reason: reason, NativeEvent: name, Source: source, Locator: locator, Start: proof, Deferred: deferred, Child: child}
	if kind == agentapi.EventSubagent {
		return []agentapi.LifecycleEvent{event}, nil
	}
	lifecycle := kind == agentapi.EventStart || kind == agentapi.EventTurnStart || kind == agentapi.EventStop
	final := kind == agentapi.EventStop || kind == agentapi.EventResponse
	count := 0
	if lifecycle {
		count++
	}
	if final {
		count++
	}
	if count > 0 {
		event.Evidence = make([]archive.SupplementalEvidence, 0, count)
	}
	provenance := "hook:" + string(d.Spec.Agent) + ":" + reason
	if lifecycle {
		event.Evidence = append(event.Evidence, d.evidence(archive.EvidenceKindLifecycleHook, name, provenance, payload, false, input))
	}
	if final {
		event.Evidence = append(event.Evidence, d.evidence(archive.EvidenceKindFinalResponse, name, provenance, payload, true, input))
	}
	if kind == agentapi.EventTurnStart && d.Spec.PromptStarts {
		candidate := event
		candidate.Kind = agentapi.EventStart
		candidate.NewOnly = true
		candidate.Evidence = nil
		event.Start = agentapi.StartEvidence{}
		event.Deferred = agentapi.DeferredNone
		return []agentapi.LifecycleEvent{candidate, event}, nil
	}
	return []agentapi.LifecycleEvent{event}, nil
}

func (d Decoder) evidence(kind archive.SupplementalEvidenceKind, event, provenance string, payload map[string]any, final bool, input agentapi.HookInput) archive.SupplementalEvidence {
	out := map[string]any{"event_name": first(payload, "hook_event_name")}
	for _, key := range []string{"message_id", "turn_id", "agent_id", "model", "model_id"} {
		if v := first(payload, key); v != "" {
			out[key] = v
		}
	}
	if out["turn_id"] == nil {
		if v := first(payload, "generation_id"); v != "" {
			out["turn_id"] = v
		}
	}
	if params, ok := payload["model_params"].([]any); ok {
		var retained []any
		for _, raw := range params {
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, value := first(p, "id"), first(p, "value")
			if id != "" && value != "" {
				retained = append(retained, map[string]any{"id": id, "value": value})
			}
		}
		if len(retained) > 0 {
			out["model_params"] = retained
		}
	}
	if final {
		if v := first(payload, "last_assistant_message", "text"); v != "" {
			out["text"] = v
		}
	}
	if kind == archive.EvidenceKindLifecycleHook {
		field := d.Spec.StatusFields[event]
		v := first(payload, field)
		if slices.Contains(d.Spec.StatusValues[event], v) {
			out["status"] = v
		}
	}
	return archive.SupplementalEvidence{Kind: kind, ObservedAt: input.ObservedAt, Provenance: provenance, Payload: out}
}

// DecodeLegacy preserves the old stored proof, without inspecting the grown source again.
func (d Decoder) DecodeLegacy(old agentapi.AdmissionIntent) ([]agentapi.LifecycleEvent, error) {
	payload := map[string]any{"hook_event_name": old.Event, "session_id": old.NativeSessionID, "cwd": old.ProjectRoot, "transcript_path": old.TranscriptPath, "cursor_version": old.CursorVersion, "composer_mode": old.ComposerMode}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: old.ObservedAt})
	if err != nil {
		return nil, err
	}
	var retained []agentapi.LifecycleEvent
	for _, event := range events {
		switch event.Deferred {
		case agentapi.DeferredStart:
			event.Start = agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: agentapi.FreshnessRetainedProof}
			retained = append(retained, event)
		case agentapi.DeferredFollowup:
			retained = append(retained, event)
		case agentapi.DeferredNone:
		}
	}
	return retained, nil
}
