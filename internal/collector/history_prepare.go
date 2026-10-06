package collector

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
)

// advanceHistoryPreparation filters one frozen historical input per slice.
// Until the last input is staged, no remote write or request acknowledgement
// is allowed. Each completed input checkpoints only immutable private work.
func (s *sessionScan) advanceHistoryPreparation(p *state.PendingPublication) error {
	if p.History == nil || !p.History.Preparing {
		return nil
	}
	if p.Attempted {
		return errors.New("attempted history publication cannot be prepared again")
	}
	if err := p.ValidateHistory(s.id()); err != nil {
		return err
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return err
	}
	if p.History.PrivacyCursor < len(p.History.Inputs) {
		input := p.History.Inputs[p.History.PrivacyCursor]
		bundle, err := s.loadHistoryInput(*p, metadata, input)
		if err != nil {
			return err
		}
		adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
		if err != nil {
			return err
		}
		if bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
			bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, s.opts.skillEvidence())
			filtered, err := refilterBundle(s.ctx, s.reg, adapter, bundle)
			if err != nil {
				return fmt.Errorf("filter preserved revision: %w", err)
			}
			compressed, err := archive.BuildCompressedSource(filtered)
			if err != nil {
				return err
			}
			key, err := archive.SourceObjectKey(filtered, compressed.SHA256)
			if err != nil {
				return err
			}
			next := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
			stage, err := s.local.StagePendingSource(s.id(), next, compressed.Bytes)
			if err != nil {
				return err
			}
			found := false
			for i := range metadata.History.Preserved {
				if metadata.History.Preserved[i].RevisionID == input.RevisionID && metadata.History.Preserved[i].Source == input.Reference {
					metadata.History.Preserved[i].Source = next
					metadata.History.Preserved[i].SourceSchemaVersion = filtered.SchemaVersion
					metadata.History.Preserved[i].FilterVersion = filtered.Capture.FilterVersion
					found = true
				}
			}
			if !found {
				return errors.New("history preparation input no longer belongs to its frozen set")
			}
			if next != input.Reference {
				p.History.Sources = append(p.History.Sources, stage)
				p.History.Retired = append(p.History.Retired, state.RetiredSource{Reference: input.Reference, PrivacySensitive: true})
			}
		}
		p.History.PrivacyCursor++
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		p.MetadataBytes = encoded
	}
	p.History.Preparing = p.History.PrivacyCursor < len(p.History.Inputs)
	refs, err := metadata.SourceReferences()
	if err != nil {
		return err
	}
	protected := map[archive.SourceReference]bool{}
	for _, ref := range refs {
		protected[ref] = true
	}
	stages := p.History.Sources[:0]
	for _, stage := range p.History.Sources {
		if protected[stage.Reference] {
			stages = append(stages, stage)
		}
	}
	p.History.Sources = stages
	if !p.History.Preparing {
		p.History.PreparedAt = s.now
		for i := range p.History.Retired {
			if p.History.Retired[i].RetiredAt.IsZero() {
				p.History.Retired[i].RetiredAt = s.now
			}
		}
	}
	return s.local.SavePending(s.id(), *p)
}

func (s *sessionScan) loadHistoryInput(p state.PendingPublication, identity archive.Metadata, input state.HistoryInput) (archive.SourceBundle, error) {
	selected := identity
	selected.SchemaVersion = archive.HistoryMetadataSchemaVersion
	selected.History = &archive.RevisionHistory{CurrentRevision: input.RevisionID}
	selected.SourceBundle = input.Reference
	selected.CapturedAt = input.CapturedAt
	selected.FilterVersion = input.FilterVersion
	var data []byte
	var err error
	staged := false
	for _, stage := range p.History.Sources {
		if stage.Reference == input.Reference {
			data, err = s.local.ReadPendingSource(s.id(), stage)
			staged = true
			break
		}
	}
	if !staged {
		data, err = historyLimitedGet(s.ctx, s.remote, input.Reference.Key, int64(input.Reference.CompressedBytes))
	}
	if err != nil {
		return archive.SourceBundle{}, err
	}
	return reader.DecodeReferencedSource(s.ctx, selected, data, reader.Limits{})
}
