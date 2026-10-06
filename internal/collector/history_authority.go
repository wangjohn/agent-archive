package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// recoverReferenceAuthority runs only on a scan already requiring work. A
// missing local baseline does not mean an empty remote history. Cache authority
// only after every bounded reference decodes and a final sidecar read agrees.
func (s *sessionScan) recoverReferenceAuthority() error {
	if s.reg.Harness.Name != "codex" {
		return nil
	}
	needed, err := s.requiresReferenceRecovery()
	if err != nil || !needed {
		return err
	}
	return s.restoreReferenceAuthority()
}

// restoreReferenceAuthority bypasses only the ordinary compatibility shortcut.
func (s *sessionScan) restoreReferenceAuthority() error {
	var metadata archive.Metadata
	key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	if err != nil {
		return err
	}
	raw, err := historyLimitedGet(s.ctx, s.remote, key, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		if _, _, acknowledged := s.published.LastPublished(); acknowledged {
			return errors.New("acknowledged remote source authority is missing")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	if err := s.validateAuthorityIdentity(metadata); err != nil {
		return err
	}
	refs, err := metadata.SourceReferences()
	if err != nil {
		return err
	}
	const maximumRecoveryBytes = 128 << 20
	total := 0
	for _, ref := range refs {
		if ref.CompressedBytes <= 0 || ref.CompressedBytes > int(historyCompressedLimit) || ref.CompressedBytes > maximumRecoveryBytes-total {
			return storage.ErrObjectTooLarge
		}
		total += ref.CompressedBytes
	}
	// Decode and release preserved alternatives before retaining the active view.
	if metadata.History != nil {
		for _, revision := range metadata.History.Preserved {
			data, err := historyLimitedGet(s.ctx, s.remote, revision.Source.Key, int64(revision.Source.CompressedBytes))
			if err != nil {
				return err
			}
			if _, err := reader.DecodeRevisionSource(s.ctx, metadata, revision.RevisionID, data, reader.Limits{}); err != nil {
				return err
			}
		}
	}
	data, err := historyLimitedGet(s.ctx, s.remote, metadata.SourceBundle.Key, int64(metadata.SourceBundle.CompressedBytes))
	if err != nil {
		return err
	}
	bundle, err := reader.DecodeReferencedSource(s.ctx, metadata, data, reader.Limits{})
	if err != nil {
		return err
	}
	current, err := historyLimitedGet(s.ctx, s.remote, key, historyMetadataLimit)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, current) {
		return errHistoryMetadataConflict
	}
	return s.published.RestorePublication(bundle, metadata.SourceBundle, raw, metadata.MetadataDerivedAt)
}

func (s *sessionScan) validateAuthorityIdentity(metadata archive.Metadata) error {
	if _, err := metadata.SourceReferences(); err != nil {
		return err
	}
	if metadata.SessionID != s.id() || metadata.NativeSessionID != s.reg.NativeSessionID || metadata.ProjectID != s.reg.ProjectID || metadata.Harness.Name != s.reg.Harness.Name || metadata.CapturedAt.IsZero() || (s.opts.MachineID != "" && metadata.MachineID != s.opts.MachineID) {
		return errors.New("remote source set does not belong to the current registration")
	}
	return nil
}

func (s *sessionScan) requiresReferenceRecovery() (bool, error) {
	metadata, found, err := s.published.LastPublishedMetadata()
	if err == nil && found {
		return false, s.validateAuthorityIdentity(metadata)
	}
	// A legacy ordinary cache can require privacy maintenance even when its
	// filter provenance differs from the acknowledged sidecar. Recovering over
	// that cache would erase the maintenance input and settle the upgrade. This
	// lane cannot certify history authority: transitions explicitly restore the
	// complete remote set, and publication still checks the remote history fence.
	if s.ordinaryPrivacyMaintenance() {
		return false, nil
	}
	// Existing ordinary snapshots predating cached metadata keep their legacy
	// migration lane. They cannot authorize any history mutation; publication
	// still validates the remote schema and history fence before writing.
	if len(s.published.Metadata()) == 0 {
		previous, _, acknowledged := s.published.LastPublished()
		if acknowledged && previous.History == nil && previous.SchemaVersion < archive.HistorySourceSchemaVersion {
			return false, nil
		}
		// A current settled/gap token with no acknowledged publication already
		// checked remote absence before recording this local outcome. Parser
		// changes do not require probing that absence again. The upload preflight
		// still checks any sidecar appearing since then before its first write.
		if !acknowledged && s.published.Found() {
			signature, present, err := s.local.LoadScanSignature(s.id())
			if err != nil {
				return false, err
			}
			if present && signature.SourceSetVersion == sourceSetVersion(s.reg) {
				return false, nil
			}
		}
	}
	// Never downgrade a local document written under an unsupported schema.
	if raw := s.published.Metadata(); len(raw) != 0 {
		var header struct {
			SchemaVersion int `json:"schema_version"`
		}
		if json.Unmarshal(raw, &header) == nil && header.SchemaVersion != archive.MetadataSchemaVersion && header.SchemaVersion != archive.HistoryMetadataSchemaVersion {
			return false, fmt.Errorf("unsupported acknowledged metadata schema version %d", header.SchemaVersion)
		}
	}
	return true, nil
}

func (s *sessionScan) ordinaryPrivacyMaintenance() bool {
	bundle, _, found := s.published.LastPublished()
	if !found || bundle.SchemaVersion != archive.SourceSchemaVersion || bundle.History != nil || bundle.Capture.FilterVersion == archive.FilterVersion {
		return false
	}
	var metadata archive.Metadata
	if json.Unmarshal(s.published.Metadata(), &metadata) != nil || metadata.SchemaVersion != archive.MetadataSchemaVersion || metadata.History != nil || s.validateAuthorityIdentity(metadata) != nil {
		return false
	}
	if metadata.NativeSessionID != bundle.NativeSessionID || metadata.ProjectID != bundle.ProjectID || metadata.Harness != bundle.Capture.Harness || metadata.ParentSessionID != bundle.ParentSessionID || !metadata.CapturedAt.Equal(bundle.Capture.CapturedAt) {
		return false
	}
	ref, known := s.published.LastPublishedSource()
	return known && metadata.SourceBundle == ref
}
