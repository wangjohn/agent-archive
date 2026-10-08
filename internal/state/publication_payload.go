package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// PublicationSelection binds the payload to its exact metadata role and age.
type PublicationSelection struct {
	Role                string    `json:"role"`
	RevisionID          string    `json:"revision_id,omitempty"`
	CapturedAt          time.Time `json:"captured_at"`
	SourceSchemaVersion int       `json:"source_schema_version"`
}

// ImmutableStageHandle has no caller-selected filesystem path.
type ImmutableStageHandle struct {
	Version          int                  `json:"version"`
	Kind             string               `json:"kind"`
	Namespace        string               `json:"namespace"`
	SessionID        string               `json:"session_id"`
	OwnerSHA256      string               `json:"owner_sha256"`
	DestinationID    string               `json:"destination_id"`
	AdmissionContext string               `json:"admission_context"`
	SourceSHA256     string               `json:"source_sha256"`
	CompressedBytes  int                  `json:"compressed_bytes"`
	Selection        PublicationSelection `json:"selection"`
}

// PublicationPayload is the closed single source-byte authority.
type PublicationPayload struct {
	Kind   string                `json:"kind"`
	Inline []byte                `json:"inline,omitempty"`
	Stage  *ImmutableStageHandle `json:"stage,omitempty"`
}

func publicationOwner(m archive.Metadata, destination, admission string) string {
	b, _ := json.Marshal(struct{ Session, Native, Project, Machine, Harness, Destination, Admission string }{m.SessionID, m.NativeSessionID, m.ProjectID, m.MachineID, m.Harness.Name, destination, admission})
	return publicationSHA256(append([]byte("publication-owner/v1\x00"), b...))
}

func selectedPublicationSources(p PendingPublication, destination, admission string) ([]PublicationSource, error) {
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return nil, err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return nil, err
	}
	prior := make(map[string]PublicationSource, len(p.Sources))
	for _, s := range p.Sources {
		if _, ok := prior[s.Reference.Key]; ok {
			return nil, errors.New("duplicate publication payload")
		}
		prior[s.Reference.Key] = s
	}
	out := make([]PublicationSource, 0, len(refs))
	for i, ref := range refs {
		selection := PublicationSelection{Role: "current", CapturedAt: m.CapturedAt.UTC(), SourceSchemaVersion: p.Bundle.SchemaVersion}
		if m.History != nil {
			selection.RevisionID = m.History.CurrentRevision
		}
		if i > 0 {
			r := m.History.Preserved[i-1]
			selection = PublicationSelection{Role: "preserved", RevisionID: r.RevisionID, CapturedAt: r.CapturedAt.UTC(), SourceSchemaVersion: r.SourceSchemaVersion}
		}
		// Legacy metadata may omit a preserved source's producer schema. The
		// owning preparation has already decoded each exact input; bind that
		// provenance before constructing its handle, never change only one copy.
		if p.History != nil {
			for _, input := range p.History.Inputs {
				if input.Reference != ref {
					continue
				}
				if input.RevisionID != selection.RevisionID || !input.CapturedAt.Equal(selection.CapturedAt) || selection.SourceSchemaVersion != 0 && input.SourceSchemaVersion != selection.SourceSchemaVersion {
					return nil, errors.New("frozen history input disagrees with selected provenance")
				}
				selection.SourceSchemaVersion = input.SourceSchemaVersion
			}
		}
		source := PublicationSource{Reference: ref, Selection: selection, Payload: PublicationPayload{Kind: "remote"}}
		if previous, ok := prior[ref.Key]; ok {
			if previous.Reference != ref {
				return nil, errors.New("payload reference changed")
			}
			source.Payload = previous.Payload
			if source.Payload.Kind == "" && len(previous.Bytes) > 0 {
				source.Payload = PublicationPayload{Kind: "inline", Inline: previous.Bytes}
			}
			delete(prior, ref.Key)
		}
		if i == 0 && len(p.SourceBytes) > 0 {
			source.Payload = PublicationPayload{Kind: "inline", Inline: p.SourceBytes}
		}
		if p.History != nil && !(i == 0 && len(p.SourceBytes) > 0) {
			for _, stage := range p.History.Sources {
				if stage.Reference == ref {
					source.Payload = PublicationPayload{Kind: "history-stage", Stage: &ImmutableStageHandle{Version: 1, Kind: "history-stage", Namespace: "session-pending-sources", SessionID: m.SessionID, OwnerSHA256: publicationOwner(m, destination, admission), DestinationID: destination, AdmissionContext: admission, SourceSHA256: ref.SHA256, CompressedBytes: ref.CompressedBytes, Selection: selection}}
					break
				}
			}
		}
		if source.Payload.Kind == "" {
			source.Payload.Kind = "remote"
		}
		out = append(out, source)
	}
	for _, unselected := range prior {
		known, retired := false, false
		if p.Preparation != nil && p.History != nil {
			for _, input := range p.Preparation.Inputs {
				known = known || input.Reference == unselected.Reference
			}
			for _, old := range p.History.Retired {
				retired = retired || old.Reference == unselected.Reference
			}
		}
		if !known || !retired {
			return nil, errors.New("unselected payload authority")
		}
	}

	return out, nil
}

func validatePublicationPayload(source PublicationSource, m archive.Metadata, destination, admission string) error {
	p := source.Payload
	switch p.Kind {
	case "inline":
		if p.Stage != nil || len(p.Inline) != source.Reference.CompressedBytes || publicationSHA256(p.Inline) != source.Reference.SHA256 {
			return errors.New("inline publication checksum or union mismatch")
		}
	case "remote":
		if p.Stage != nil || len(p.Inline) != 0 {
			return errors.New("remote publication union mismatch")
		}
	case "history-stage":
		h := p.Stage
		if len(p.Inline) != 0 || h == nil || h.Version != 1 || h.Kind != p.Kind || h.Namespace != "session-pending-sources" || h.SessionID != m.SessionID || h.OwnerSHA256 != publicationOwner(m, destination, admission) || h.DestinationID != destination || h.AdmissionContext != admission || h.SourceSHA256 != source.Reference.SHA256 || h.CompressedBytes != source.Reference.CompressedBytes || h.Selection != source.Selection {
			return errors.New("history stage authority mismatch")
		}
	case "admission-stage":
		return errors.New("admission stage publication authority is unavailable")
	default:
		return fmt.Errorf("unsupported publication payload kind %q", p.Kind)
	}
	return nil
}

type payloadBinding struct {
	Reference    archive.SourceReference
	Selection    PublicationSelection
	Kind         string
	InlineSHA256 string
	InlineSize   int
	Stage        *ImmutableStageHandle
}

func payloadSetSHA(sources []PublicationSource) string {
	bindings := make([]payloadBinding, len(sources))
	for i, s := range sources {
		b := payloadBinding{Reference: s.Reference, Selection: s.Selection, Kind: s.Payload.Kind, Stage: s.Payload.Stage}
		if s.Payload.Kind == "inline" {
			b.InlineSHA256 = publicationSHA256(s.Payload.Inline)
			b.InlineSize = len(s.Payload.Inline)
		}
		bindings[i] = b
	}
	raw, _ := json.Marshal(bindings)
	return publicationSHA256(append([]byte("payload-map/v1\x00"), raw...))
}
