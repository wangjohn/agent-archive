package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

const pendingHistoryVersion = 1
const maxPendingHistoryBytes = 128 << 20

var stagedSourceName = regexp.MustCompile(`^[0-9a-f]{64}\.gz$`)

// PendingHistory freezes one complete reference-set replacement. Source payloads
// are staged individually before this descriptor, outside the journal JSON.
type PendingHistory struct {
	// Preparing performs private sequential privacy work before any remote write.
	PreparedAt             time.Time       `json:"prepared_at,omitzero"`
	Preparing              bool            `json:"preparing,omitempty"`
	PrivacyCursor          int             `json:"privacy_cursor,omitempty"`
	Inputs                 []HistoryInput  `json:"inputs,omitempty"`
	Version                int             `json:"version"`
	ExpectedMetadataSHA256 string          `json:"expected_metadata_sha256,omitempty"`
	Sources                []PendingSource `json:"sources,omitempty"`
	Retired                []RetiredSource `json:"retired,omitempty"`
}

// HistoryInput freezes the capture facts needed to read an earlier reference
// while the next metadata document is being prepared under newer privacy rules.
type HistoryInput struct {
	Reference     archive.SourceReference `json:"reference"`
	RevisionID    string                  `json:"revision_id"`
	CapturedAt    time.Time               `json:"captured_at"`
	FilterVersion string                  `json:"filter_version"`
}

// PendingSource identifies an immutable owned stage by checksum-derived name.
type PendingSource struct {
	Reference archive.SourceReference `json:"reference"`
	Name      string                  `json:"name"`
}

// RetiredSource retains every cleanup obligation until acknowledgement succeeds.
type RetiredSource struct {
	RetiredAt        time.Time               `json:"retired_at"`
	Reference        archive.SourceReference `json:"reference"`
	PrivacySensitive bool                    `json:"privacy_sensitive,omitempty"`
}

// ValidateHistory refuses future journals and references outside this session.
func (p PendingPublication) ValidateHistory(id string) error {
	if p.History == nil {
		return nil
	}
	if p.History.Version != pendingHistoryVersion {
		return errors.New("pending history requires a newer writer")
	}
	if err := validatePredecessorSHA(p.History.ExpectedMetadataSHA256); err != nil {
		return err
	}

	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return err
	}
	expectedKey, keyErr := archive.MetadataObjectKey(m.Harness.Name, id)
	if keyErr != nil || expectedKey != p.MetadataKey {
		return errors.New("pending history metadata key mismatch")
	}
	if m.SessionID != id || m.SourceBundle != p.SourceReference() {
		return errors.New("pending history identity mismatch")
	}
	known := map[archive.SourceReference]bool{}
	for _, r := range refs {
		known[r] = true
	}
	if err := p.validateHistoryInputs(m); err != nil {
		return err
	}
	if len(p.History.Sources) > archive.MaxHistorySpans || len(p.History.Retired) > archive.MaxHistorySpans+1 {
		return errors.New("pending history exceeds source limit")
	}
	size := len(p.SourceBytes)
	seen := map[string]bool{}
	for _, stage := range p.History.Sources {
		r := stage.Reference
		if !stagedSourceName.MatchString(stage.Name) || stage.Name != r.SHA256+".gz" || !known[r] || seen[stage.Name] {
			return errors.New("invalid pending history stage")
		}
		seen[stage.Name] = true
		if r.CompressedBytes > maxPendingHistoryBytes-size {
			return errors.New("pending history exceeds byte limit")
		}
		size += r.CompressedBytes
	}
	for _, retired := range p.History.Retired {
		prior := m
		prior.SchemaVersion = archive.MetadataSchemaVersion
		prior.History = nil
		prior.SourceBundle = retired.Reference
		if prior.ValidateSourceReference() != nil || known[retired.Reference] || (retired.RetiredAt.IsZero() && !p.History.Preparing) {
			return errors.New("invalid retired history reference")
		}
	}
	return nil
}

func (s *Store) checkPendingHistoryVersion(id string) error {
	var header struct {
		History *struct {
			Version int `json:"version"`
		} `json:"history"`
	}
	err := local.Read(s.pendingPath(id), &header)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return nil
	} // Ordinary damage follows the established quarantine policy.
	if header.History != nil && header.History.Version != pendingHistoryVersion {
		return errors.New("pending history requires a newer writer")
	}
	return nil
}

func (s *Store) stagePath(id, name string) (string, error) {
	if !safeFileComponent(id) || !stagedSourceName.MatchString(name) {
		return "", errors.New("invalid history stage identity")
	}
	return filepath.Join(s.home, "sessions", id, "pending-sources", name), nil
}

// StagePendingSource durably freezes bytes before the journal references them.
func (s *Store) StagePendingSource(id string, ref archive.SourceReference, data []byte) (PendingSource, error) {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != ref.SHA256 || len(data) != ref.CompressedBytes || len(data) > maxPendingHistoryBytes {
		return PendingSource{}, errors.New("invalid staged source size")
	}
	name := ref.SHA256 + ".gz"
	path, err := s.stagePath(id, name)
	if err != nil {
		return PendingSource{}, err
	}
	if err := local.WriteBytes(path, data); err != nil {
		return PendingSource{}, err
	}
	return PendingSource{Reference: ref, Name: name}, nil
}

// ReadPendingSource reads only the journal's bounded private checksum stage.
func (s *Store) ReadPendingSource(id string, stage PendingSource) ([]byte, error) {
	path, err := s.stagePath(id, stage.Name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(stage.Reference.CompressedBytes) || info.Size() > maxPendingHistoryBytes {
		return nil, fmt.Errorf("invalid history stage size or type")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("history stage changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(stage.Reference.CompressedBytes)+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if len(data) != stage.Reference.CompressedBytes || hex.EncodeToString(sum[:]) != stage.Reference.SHA256 {
		return nil, errors.New("history stage checksum mismatch")
	}
	return data, nil
}

func validatePredecessorSHA(value string) error {
	if value == "" {
		return nil
	}
	checksum, err := hex.DecodeString(value)
	if err != nil || len(checksum) != sha256.Size {
		return errors.New("invalid frozen predecessor checksum")
	}
	return nil
}

func (p PendingPublication) validateHistoryInputs(m archive.Metadata) error {
	if p.History.PrivacyCursor < 0 || p.History.PrivacyCursor > len(p.History.Inputs) || len(p.History.Inputs) > archive.MaxHistorySpans || p.History.Preparing && p.Attempted {
		return errors.New("invalid history preparation progress")
	}
	for _, input := range p.History.Inputs {
		prior := m
		prior.History = &archive.RevisionHistory{CurrentRevision: input.RevisionID}
		prior.SourceBundle = input.Reference
		prior.CapturedAt = input.CapturedAt
		prior.FilterVersion = input.FilterVersion
		if prior.ValidateSourceReference() != nil || input.CapturedAt.IsZero() || input.FilterVersion == "" {
			return errors.New("invalid history preparation input")
		}
	}
	return nil
}
