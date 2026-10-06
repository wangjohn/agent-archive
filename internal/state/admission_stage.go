package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

const admissionStageDir = "admission-stages"

// AdmissionStageQuota bounds conservative aggregate stage and publication disk reservations.
const AdmissionStageQuota int64 = 1 << 30

// AdmissionStageFilteredLimit bounds one encoded filtered source.
const AdmissionStageFilteredLimit int64 = 64 << 20

// AdmissionStageCompressedLimit bounds one compressed filtered source.
const AdmissionStageCompressedLimit int64 = 128 << 20
const stageManifestLimit int64 = 1 << 20

// ErrAdmissionStageCapacity leaves the import resumable after admitted groups publish.
var ErrAdmissionStageCapacity = errors.New("durable import capacity exhausted; publish admitted groups before resuming")

// ErrAdmissionStageRecovery preserves missing or corrupt admitted evidence without substitution.
var ErrAdmissionStageRecovery = errors.New("durable import evidence requires recovery; native substitution is forbidden")

// AdmissionStage is private prepared evidence, not permission to admit. Only a
// registration with its immutable manifest digest grants collector ownership.
// Reservation includes the reviewed destination and frozen attribution facts.
type AdmissionStage struct {
	Children      []archive.SessionRegistration `json:"children,omitempty"`
	Version       int                           `json:"version"`
	Reservation   archive.SessionRegistration   `json:"reservation"`
	SkillEvidence string                        `json:"skill_evidence"`
	SHA256        string                        `json:"sha256"`
	Bytes         int64                         `json:"bytes"`
	// ReservedBytes includes worst-case atomic stage, pending and publication
	// duplicates. It is capacity accounting, not a measure of SQLite page I/O.
	ReservedBytes int64     `json:"reserved_bytes"`
	PreparedAt    time.Time `json:"prepared_at"`
}

func stageDigest(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }
func validStageID(id string) bool {
	return safeFileComponent(id)
}
func (s *Store) stagePath(id, suffix string) (string, error) {
	if !validStageID(id) {
		return "", ErrAdmissionStageRecovery
	}
	dir := filepath.Join(s.home, admissionStageDir)
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", ErrAdmissionStageRecovery
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Join(dir, id+suffix), nil
}

// PrepareAdmissionStage durably writes immutable filtered bytes before the
// manifest. The caller owns collector.lock; hooks never prepare stages. Failed
// writes remain conservative capacity consumers until explicit recovery.
func (s *Store) PrepareAdmissionStage(reg archive.SessionRegistration, bundle archive.SourceBundle, skill string, at time.Time, children ...archive.SessionRegistration) (string, error) {
	if reg.AdmissionStage != "" || reg.Validate() != nil || reg.Origin != archive.SessionOriginImport {
		return "", ErrAdmissionStageRecovery
	}
	if err := archive.CheckHistoryMutation(bundle, archive.Metadata{}); err != nil {
		return "", err
	}
	if bundle.ArchiveSessionID != reg.ArchiveSessionID || bundle.NativeSessionID != reg.NativeSessionID || bundle.ProjectID != reg.ProjectID || bundle.ParentSessionID != reg.ParentSessionID || bundle.Capture.Harness.Name != reg.Harness.Name {
		return "", ErrAdmissionStageRecovery
	}
	var plain bytes.Buffer
	if err := archive.EncodeSource(&plain, bundle); err != nil {
		return "", err
	}
	if int64(plain.Len()) > AdmissionStageFilteredLimit {
		return "", archive.ErrSourceTooLarge
	}
	compressed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		return "", err
	}
	if int64(len(compressed.Bytes)) > AdmissionStageCompressedLimit {
		return "", archive.ErrSourceTooLarge
	}
	// JSON/base64 expansion and both old/new atomic files are covered eightfold.
	reserve := 8*(int64(plain.Len())+int64(len(compressed.Bytes))) + stageManifestLimit
	m := AdmissionStage{Children: children, Version: 1, Reservation: reg, SkillEvidence: skill, SHA256: compressed.SHA256, Bytes: int64(len(compressed.Bytes)), ReservedBytes: reserve, PreparedAt: at.UTC()}
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if int64(len(encoded)) > stageManifestLimit {
		return "", ErrAdmissionStageCapacity
	}
	manifest, err := s.stagePath(reg.ArchiveSessionID, ".json")
	if err != nil {
		return "", err
	}
	object, _ := s.stagePath(reg.ArchiveSessionID, ".source.gz")
	if old, e := readStageFile(manifest, stageManifestLimit); e == nil {
		if bytes.Equal(old, encoded) {
			if _, _, e = s.ReadAdmissionStage(reg.ArchiveSessionID, stageDigest(old)); e == nil {
				return stageDigest(old), nil
			}
		}
		return "", ErrAdmissionStageRecovery
	} else if !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	used, err := s.admissionStageUsage()
	if err != nil {
		return "", err
	}
	if reserve > AdmissionStageQuota-used {
		return "", ErrAdmissionStageCapacity
	}
	if err = os.MkdirAll(filepath.Dir(manifest), 0700); err != nil {
		return "", err
	}
	if _, e := os.Lstat(object); !errors.Is(e, os.ErrNotExist) {
		return "", ErrAdmissionStageRecovery
	}
	if err = local.WriteBytes(object, compressed.Bytes); err != nil {
		return "", err
	}
	if err = local.WriteBytes(manifest, encoded); err != nil {
		return "", err
	}
	return stageDigest(encoded), nil
}

func readStageFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, ErrAdmissionStageRecovery
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrAdmissionStageRecovery
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrAdmissionStageRecovery
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, ErrAdmissionStageRecovery
	}
	return b, nil
}

// ReadAdmissionStage verifies the admission pointer, object checksum, bounded
// decode and identity before returning evidence. Missing/corrupt is never absent.
func (s *Store) ReadAdmissionStage(id, digest string) (AdmissionStage, archive.SourceBundle, error) {
	fail := func() (AdmissionStage, archive.SourceBundle, error) {
		return AdmissionStage{}, archive.SourceBundle{}, ErrAdmissionStageRecovery
	}
	path, err := s.stagePath(id, ".json")
	if err != nil {
		return fail()
	}
	b, err := readStageFile(path, stageManifestLimit)
	if err != nil || len(digest) != 64 || stageDigest(b) != digest {
		return fail()
	}
	var m AdmissionStage
	if json.Unmarshal(b, &m) != nil || m.Version != 1 || m.Reservation.ArchiveSessionID != id || m.Reservation.AdmissionStage != "" || m.Reservation.Validate() != nil || m.Bytes <= 0 || m.Bytes > AdmissionStageCompressedLimit || m.ReservedBytes <= 0 || m.ReservedBytes > AdmissionStageQuota {
		return fail()
	}
	object, _ := s.stagePath(id, ".source.gz")
	raw, err := readStageFile(object, AdmissionStageCompressedLimit)
	if err != nil || int64(len(raw)) != m.Bytes || stageDigest(raw) != m.SHA256 {
		return fail()
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{MaxUncompressedBytes: AdmissionStageFilteredLimit})
	if err != nil || bundle.ArchiveSessionID != id || bundle.NativeSessionID != m.Reservation.NativeSessionID || bundle.ProjectID != m.Reservation.ProjectID || bundle.ParentSessionID != m.Reservation.ParentSessionID || bundle.Capture.Harness.Name != m.Reservation.Harness.Name {
		return fail()
	}
	if err = archive.CheckHistoryMutation(bundle, archive.Metadata{}); err != nil {
		return AdmissionStage{}, archive.SourceBundle{}, err
	}
	return m, bundle, nil
}

func (s *Store) admissionStageUsage() (int64, error) {
	var used int64
	entries, err := os.ReadDir(filepath.Join(s.home, admissionStageDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		path, _ := s.stagePath(id, ".json")
		b, err := readStageFile(path, stageManifestLimit)
		if err != nil {
			return 0, ErrAdmissionStageRecovery
		}
		var m AdmissionStage
		if json.Unmarshal(b, &m) != nil || m.ReservedBytes <= 0 || m.ReservedBytes > AdmissionStageQuota {
			return 0, ErrAdmissionStageRecovery
		}
		released, e := s.AdmissionStageReleased(archive.SessionRegistration{ArchiveSessionID: id, AdmissionStage: stageDigest(b)})
		if e != nil {
			return 0, e
		}
		if released {
			object, _ := s.stagePath(id, ".source.gz")
			if info, e := os.Lstat(object); e == nil {
				used += 2 * info.Size()
			} else if !errors.Is(e, os.ErrNotExist) {
				return 0, e
			}
			used += int64(len(b))
		} else {
			used += m.ReservedBytes
		}
		seen[id+".source.gz"] = true
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") || seen[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			return 0, ErrAdmissionStageRecovery
		}
		used += info.Size()
	}
	// Existing pending work is charged even when it predates durable admission.
	pending, err := os.ReadDir(filepath.Join(s.home, "pending"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	for _, e := range pending {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			return 0, ErrAdmissionStageRecovery
		}
		used += 2 * info.Size()
	}
	if used < 0 {
		return 0, fmt.Errorf("%w: invalid accounting", ErrAdmissionStageRecovery)
	}
	return used, nil
}

// CheckAdmissionStageOwnership binds immutable admission fields. Mutable hook
// observations cannot redirect staged evidence or change its frozen ownership.
func CheckAdmissionStageOwnership(reg archive.SessionRegistration, m AdmissionStage) error {
	r := m.Reservation
	left, _ := json.Marshal(reg.ProjectResolution)
	right, _ := json.Marshal(r.ProjectResolution)
	if !bytes.Equal(left, right) {
		return ErrAdmissionStageRecovery
	}
	if reg.ArchiveSessionID != r.ArchiveSessionID || reg.NativeSessionID != r.NativeSessionID || reg.ProjectID != r.ProjectID || reg.ProjectRoot != r.ProjectRoot || reg.Harness.Name != r.Harness.Name || reg.DestinationID != r.DestinationID || reg.RepoKey != r.RepoKey || !reg.AdmittedAt.Equal(r.AdmittedAt) || !reg.SessionStartedAt.Equal(r.SessionStartedAt) || reg.ParentSessionID != r.ParentSessionID || reg.ParentNativeSessionID != r.ParentNativeSessionID || reg.SubagentID != r.SubagentID || reg.Origin != r.Origin {
		return ErrAdmissionStageRecovery
	}
	return nil
}

// ReconcileAdmissionStage repairs request and child-registration links only for
// an admitted parent. A prepared manifest without that pointer is never authority.
// The caller owns collector.lock and has accepted the current parent policy.
func (s *Store) ReconcileAdmissionStage(reg archive.SessionRegistration, m AdmissionStage, accept func(archive.SessionRegistration) bool) error {
	if reg.AdmissionStage == "" || CheckAdmissionStageOwnership(reg, m) != nil {
		return ErrAdmissionStageRecovery
	}
	for _, child := range m.Children {
		if child.ParentSessionID != reg.ArchiveSessionID || child.DestinationID != reg.DestinationID || child.AdmissionStage == "" {
			return ErrAdmissionStageRecovery
		}
		existing, found, err := s.LoadRegistration(child.ArchiveSessionID)
		if err != nil {
			return err
		}
		if found && existing.AdmissionStage == child.AdmissionStage {
			released, e := s.AdmissionStageReleased(existing)
			if e != nil {
				return e
			}
			if released {
				continue
			}
		}
		cm, _, err := s.ReadAdmissionStage(child.ArchiveSessionID, child.AdmissionStage)
		if err != nil {
			return err
		}
		if CheckAdmissionStageOwnership(child, cm) != nil {
			return ErrAdmissionStageRecovery
		}
		if accept != nil && !accept(child) {
			continue
		}
		key := agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(child.Harness.Name)), NativeID: child.NativeSessionID}
		admitted, err := s.RegisterReservedSession(key, child.ArchiveSessionID, func(string) archive.SessionRegistration { return child })
		if err != nil {
			return err
		}
		if admitted.AdmissionStage != child.AdmissionStage {
			return ErrAdmissionStageRecovery
		}
		if _, found, err := s.LoadRequest(child.ArchiveSessionID); err != nil {
			return err
		} else if !found {
			if err = s.SaveAdmissionRequest(child); err != nil {
				return err
			}
		}
	}
	if _, found, err := s.LoadRequest(reg.ArchiveSessionID); err != nil {
		return err
	} else if !found {
		return s.SaveAdmissionRequest(reg)
	}
	return nil
}

// SaveAdmissionRequest joins request creation to its covered-stage token. Later
// hooks preserve that token while replacing Request.Token, so they stay queued.
func (s *Store) SaveAdmissionRequest(reg archive.SessionRegistration, evidence ...archive.SupplementalEvidence) error {
	return s.writeUnderRequestLock(reg.ArchiveSessionID, s.requestPath(reg.ArchiveSessionID), nil, func(current fileSnapshot) (any, bool, error) {
		merged, write, err := mergeRequest(reg.ArchiveSessionID, current, "backfill", reg.AdmittedAt, false, evidence)
		if err != nil {
			return nil, false, err
		}
		if !write {
			if err = json.Unmarshal(current.data, &merged); err != nil {
				return nil, false, err
			}
		}
		if merged.StageDigest != "" {
			if merged.StageDigest != reg.AdmissionStage {
				return nil, false, ErrAdmissionStageRecovery
			}
			return merged, write, nil
		}
		merged.StageDigest = reg.AdmissionStage
		if len(merged.Reasons) == 1 && merged.Reasons[0] == "backfill" {
			merged.StageToken = merged.Token
		}
		return merged, true, nil
	})
}

type stageRelease struct {
	Checksum                string `json:"checksum"`
	Digest                  string `json:"digest"`
	SourceSHA256            string `json:"source_sha256"`
	SelectingMetadataSHA256 string `json:"selecting_metadata_sha256,omitempty"`
	ReplacementSHA256       string `json:"replacement_sha256,omitempty"`
	CoveredToken            string `json:"covered_token,omitempty"`
	Complete                bool   `json:"complete"`
}

func stageReleaseChecksum(r stageRelease) string {
	r.Checksum = ""
	body, _ := json.Marshal(r)
	return stageDigest(body)
}

func writeStageRelease(path string, r stageRelease) error {
	r.Checksum = stageReleaseChecksum(r)
	return local.WriteCompact(path, r)
}

// AdmissionStageReleased is a cheap journal probe; corruption keeps work owed.
func (s *Store) AdmissionStageReleased(reg archive.SessionRegistration) (bool, error) {
	path, err := s.stagePath(reg.ArchiveSessionID, ".released")
	if err != nil {
		return false, err
	}
	b, err := readStageFile(path, stageManifestLimit)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var r stageRelease
	if json.Unmarshal(b, &r) != nil || r.Digest != reg.AdmissionStage || len(r.SourceSHA256) != 64 || r.Checksum != stageReleaseChecksum(r) {
		return false, ErrAdmissionStageRecovery
	}
	return r.Complete, nil
}

// ReleaseAdmissionStage journals last, after an exact complete-set local commit
// and acknowledgement of the token this stage covered. A newer token remains.
func (s *Store) ReleaseAdmissionStage(reg archive.SessionRegistration, m AdmissionStage, published *Published, coveredToken string) error {
	if CheckAdmissionStageOwnership(reg, m) != nil {
		return ErrAdmissionStageRecovery
	}
	if !s.AdmissionStageCommitted(reg, m, published) {
		return ErrAdmissionStageRecovery
	}
	refs, err := published.CommittedSources()
	if err != nil {
		return err
	}
	represented := false
	for _, ref := range refs {
		if ref.SHA256 == m.SHA256 && int64(ref.CompressedBytes) == m.Bytes {
			represented = true
		}
	}
	if !represented {
		if _, valid := published.privacyStageSource(reg, m); !valid {
			return ErrAdmissionStageRecovery
		}
	}
	if coveredToken != "" {
		if _, err = s.CompleteRequest(reg.ArchiveSessionID, coveredToken); err != nil {
			return err
		}
	}
	path, _ := s.stagePath(reg.ArchiveSessionID, ".released")
	receipt := stageRelease{Digest: reg.AdmissionStage, SourceSHA256: m.SHA256, CoveredToken: coveredToken}
	if replacement, valid := published.privacyStageSource(reg, m); valid {
		receipt.ReplacementSHA256 = replacement
		receipt.SelectingMetadataSHA256 = published.state.Commit.MetadataSHA256
	}
	if err = writeStageRelease(path, receipt); err != nil {
		return err
	}
	_, err = s.ResumeAdmissionStageRelease(reg, published)
	return err
}

// AdmissionStageCommitted requires the new complete-set local publication
// authority. A legacy cached source reference is insufficient for release.
func (s *Store) AdmissionStageCommitted(reg archive.SessionRegistration, m AdmissionStage, p *Published) bool {
	if p.state.Commit == nil || p.state.Commit.DestinationID != reg.DestinationID || p.state.Commit.AdmissionContext != AdmissionStageContext(reg) {
		return false
	}
	refs, err := p.CommittedSources()
	if err != nil {
		return false
	}
	for _, ref := range refs {
		if ref.SHA256 == m.SHA256 && int64(ref.CompressedBytes) == m.Bytes {
			return true
		}
	}
	_, transformed := p.privacyStageSource(reg, m)
	return transformed
}

// PreparedAdmissionStage reads a reservation only. Callers must still obtain
// confirmation and current admission policy; this method never admits it.
func (s *Store) PreparedAdmissionStage(id string) (AdmissionStage, archive.SourceBundle, string, bool, error) {
	path, err := s.stagePath(id, ".json")
	if err != nil {
		return AdmissionStage{}, archive.SourceBundle{}, "", false, err
	}
	b, err := readStageFile(path, stageManifestLimit)
	if errors.Is(err, os.ErrNotExist) {
		return AdmissionStage{}, archive.SourceBundle{}, "", false, nil
	}
	if err != nil {
		return AdmissionStage{}, archive.SourceBundle{}, "", false, err
	}
	digest := stageDigest(b)
	m, bundle, err := s.ReadAdmissionStage(id, digest)
	return m, bundle, digest, true, err
}

// AdmissionStageContext binds publication to the exact admitted manifest.
func AdmissionStageContext(reg archive.SessionRegistration) string {
	body, _ := json.Marshal(struct {
		Session   string
		Native    string
		Project   string
		Admission string
		Origin    archive.SessionOrigin
		Batch     archive.ImportBatch
		Stage     string
	}{reg.ArchiveSessionID, reg.NativeSessionID, reg.ProjectID, reg.Admitted().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), reg.Origin, reg.ImportBatch, reg.AdmissionStage})
	return stageDigest(body)
}

// ResumeAdmissionStageRelease finishes an interrupted cleanup from the private
// journal and verified local source set, even when deletion already happened.
func (s *Store) ResumeAdmissionStageRelease(reg archive.SessionRegistration, p *Published) (bool, error) {
	path, _ := s.stagePath(reg.ArchiveSessionID, ".released")
	b, err := readStageFile(path, stageManifestLimit)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	var r stageRelease
	if json.Unmarshal(b, &r) != nil || r.Digest != reg.AdmissionStage || r.Checksum != stageReleaseChecksum(r) {
		return true, ErrAdmissionStageRecovery
	}
	manifest, _ := s.stagePath(reg.ArchiveSessionID, ".json")
	raw, err := readStageFile(manifest, stageManifestLimit)
	if err != nil || stageDigest(raw) != reg.AdmissionStage {
		return true, ErrAdmissionStageRecovery
	}
	var m AdmissionStage
	if json.Unmarshal(raw, &m) != nil || m.SHA256 != r.SourceSHA256 || !s.AdmissionStageCommitted(reg, m, p) {
		return true, ErrAdmissionStageRecovery
	}
	if r.ReplacementSHA256 != "" {
		replacement, valid := p.privacyStageSource(reg, m)
		if !valid || replacement != r.ReplacementSHA256 || p.state.Commit.MetadataSHA256 != r.SelectingMetadataSHA256 {
			return true, ErrAdmissionStageRecovery
		}
	}
	if r.Complete {
		return true, nil
	}
	if s.onStageCleanup != nil {
		if err = s.onStageCleanup(); err != nil {
			return true, err
		}
	}
	object, _ := s.stagePath(reg.ArchiveSessionID, ".source.gz")
	if err = os.Remove(object); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	root, err := os.OpenRoot(filepath.Dir(object))
	if err != nil {
		return true, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return true, err
	}
	if err = errors.Join(dir.Sync(), dir.Close()); err != nil {
		return true, err
	}
	r.Complete = true
	return true, writeStageRelease(path, r)
}
