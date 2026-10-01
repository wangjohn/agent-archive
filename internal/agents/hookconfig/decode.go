package hookconfig

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"path/filepath"
	"strings"
)

// DecoderSpec supplies native event and source rules from an integration.
type DecoderSpec struct {
	Agent                                                                           agentmeta.ID
	Events                                                                          map[string]agentapi.EventKind
	PromptStarts, NullPathFresh, OwnedFilename, NativeObservations, ChildTranscript bool
	Followups                                                                       map[string]bool
	StatusFields                                                                    map[string]string
	StatusValues                                                                    map[string][]string
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
			return agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: "empty_native_path"}
		}
		path, ok := value.(string)
		if !ok {
			return agentapi.StartEvidence{Reason: "invalid_native_path"}
		}
		return agentapi.StartEvidence{Kind: agentapi.FreshStat, Reason: "inspect_native_path", Path: path}
	}
	switch strings.ToLower(strings.TrimSpace(first(payload, "source"))) {
	case "startup", "clear":
		return agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: "explicit_start"}
	case "resume", "compact":
		return agentapi.StartEvidence{Kind: agentapi.FreshContinuation, Reason: "explicit_continuation"}
	case "":
		return agentapi.StartEvidence{Kind: agentapi.FreshStat, Reason: "inspect_native_path", Path: first(payload, "transcript_path")}
	default:
		return agentapi.StartEvidence{Reason: "unknown_native_source"}
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
	session := agentapi.NativeSession{Agent: d.Spec.Agent, NativeID: id}
	if d.Spec.NativeObservations {
		session.Version = first(payload, "cursor_version")
		session.Mode = first(payload, "composer_mode")
	}
	event := agentapi.LifecycleEvent{Kind: kind, Session: session, ProjectRoot: root(payload), Reason: strings.ToLower(name), NativeEvent: name, Source: agentapi.SourceRef{Path: d.locator(payload, id)}}
	if d.Spec.OwnedFilename && (kind == agentapi.EventStart || kind == agentapi.EventTurnStart || d.Spec.Followups[name]) {
		event.Locator = agentapi.LocatorFillFile
	} else if !d.Spec.OwnedFilename && kind == agentapi.EventStart {
		event.Locator = agentapi.LocatorReplaceFile
	}
	if kind == agentapi.EventStart || kind == agentapi.EventTurnStart && d.Spec.PromptStarts {
		event.Start = d.freshness(payload)
		event.Deferred = agentapi.DeferredStart
	}
	if d.Spec.Followups[name] && event.Source.Path != "" {
		event.Deferred = agentapi.DeferredFollowup
	}
	if kind == agentapi.EventSubagent {
		event.Child = &agentapi.ChildObservation{ID: first(payload, "agent_id"), Path: first(payload, "agent_transcript_path"), Type: archive.SanitizeSubagentType(first(payload, "agent_type")), CaptureTranscript: d.Spec.ChildTranscript, MissingDetail: "SubagentStop omitted agent_id"}
		return []agentapi.LifecycleEvent{event}, nil
	}
	lifecycle := kind == agentapi.EventStart || kind == agentapi.EventTurnStart || kind == agentapi.EventStop
	if lifecycle {
		event.Evidence = append(event.Evidence, d.evidence(archive.EvidenceKindLifecycleHook, name, payload, false, input))
	}
	if kind == agentapi.EventStop || kind == agentapi.EventResponse {
		event.Evidence = append(event.Evidence, d.evidence(archive.EvidenceKindFinalResponse, name, payload, true, input))
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
func (d Decoder) evidence(kind archive.SupplementalEvidenceKind, event string, payload map[string]any, final bool, input agentapi.HookInput) archive.SupplementalEvidence {
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
		for _, allowed := range d.Spec.StatusValues[event] {
			if v == allowed {
				out["status"] = v
				break
			}
		}
	}
	provenance := "hook:" + string(d.Spec.Agent) + ":" + strings.ToLower(event)
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
		if event.Deferred == agentapi.DeferredStart {
			event.Start = agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: "retained_hook_proof"}
			retained = append(retained, event)
		} else if event.Deferred == agentapi.DeferredFollowup {
			retained = append(retained, event)
		}
	}
	return retained, nil
}
