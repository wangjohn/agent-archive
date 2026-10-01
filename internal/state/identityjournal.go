package state

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// ErrIdentityMigrationPending defers new allocation until old state is indexed.
var ErrIdentityMigrationPending = errors.New("identity migration pending; retry collection")

type identityMigration struct {
	Phase    int   `json:"phase"`
	Offset   int64 `json:"offset"`
	Complete bool  `json:"complete"`
}
type identityRecord struct {
	Agent  string `json:"agent"`
	Native string `json:"native"`
	ID     string `json:"archive_id"`
}

func (s *Store) identityRecordPath(agent, native string) string {
	return filepath.Join(s.home, "identity-journal", filepath.Base(nativeSessionIndexPath(s.home, archive.CanonicalHarness(agent)+"\x00"+native)))
}
func (s *Store) identityReady() (bool, error) {
	var m identityMigration
	err := local.Read(filepath.Join(s.home, "identity-migration.json"), &m)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err == nil && (m.Phase < 0 || m.Phase > 2 || m.Offset < 0 || (m.Complete && (m.Phase != 2 || m.Offset != 0))) {
		return false, errors.New("invalid identity migration state")
	}
	return m.Complete, err
}
func (s *Store) journalIdentity(agent, native, id string) error {
	if archive.CanonicalHarness(agent) == "" || native == "" || len(native) > 4096 || !safeFileComponent(id) {
		return errors.New("invalid journal identity")
	}
	path := s.identityRecordPath(agent, native)
	var prior identityRecord
	err := local.Read(path, &prior)
	if err == nil {
		if prior.Agent != archive.CanonicalHarness(agent) || prior.Native != native || prior.ID != id {
			return errors.New("identity journal conflict")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errors.New("identity journal needs repair")
	}
	return local.Write(path, identityRecord{archive.CanonicalHarness(agent), native, id})
}
func (s *Store) journalLookup(agent, native string) (string, bool, error) {
	var r identityRecord
	err := local.Read(s.identityRecordPath(agent, native), &r)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil || r.Agent != archive.CanonicalHarness(agent) || r.Native != native || !safeFileComponent(r.ID) {
		return "", false, errors.New("identity journal needs repair")
	}
	reg, found, err := s.LoadRegistration(r.ID)
	if err != nil {
		return "", false, err
	}
	if found && (reg.NativeSessionID != native || archive.CanonicalHarness(reg.Harness.Name) != archive.CanonicalHarness(agent)) {
		return "", false, errors.New("identity journal ownership conflict")
	}
	return r.ID, true, nil
}

// ReconcileIdentityIndexes advances legacy repair in bounded batches. The
// collector calls it even with discovery disabled. Enumeration and initial
// decoding happen outside hooks.lock; each record is revalidated under that
// lock before its authoritative journal write. guard checks setup and protects
// config policy under the lock. New admission cannot allocate until complete.
func (s *Store) ReconcileIdentityIndexes(limit int, guard func() error) (bool, error) {
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	ready, err := s.identityReady()
	if err != nil || ready {
		return ready, err
	}
	unlock, err := local.NamedLock(s.home, "identity-migration.lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	var m identityMigration
	if err := local.Read(filepath.Join(s.home, "identity-migration.json"), &m); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	release, err := local.NamedLock(s.home, "hooks.lock")
	if err != nil {
		return false, err
	}
	if guard != nil {
		err = guard()
	} else {
		err = config.ProtectIdentityWriter(s.home)
	}
	if err == nil {
		err = local.Write(filepath.Join(s.home, "identity-migration.json"), m)
	}
	release()
	if err != nil {
		return false, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for n := 0; n < limit && !m.Complete && time.Now().Before(deadline); {
		dir := "registrations"
		if m.Phase == 1 {
			dir = "subagent-candidates"
		}
		names, next, finished, err := identityNames(filepath.Join(s.home, dir), m.Offset, limit-n)
		if err != nil {
			return false, err
		}
		for _, name := range names {
			n++
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			path := filepath.Join(s.home, dir, name)
			var records []identityRecord
			if m.Phase == 0 {
				var r archive.SessionRegistration
				if err := local.Read(path, &r); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return false, err
				}
				if err := r.Validate(); err != nil {
					return false, err
				}
				records = []identityRecord{{archive.CanonicalHarness(r.Harness.Name), r.NativeSessionID, r.ArchiveSessionID}}
			} else {
				var c SubagentCandidate
				if err := local.Read(path, &c); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return false, err
				}
				records = []identityRecord{{archive.CanonicalHarness(c.Harness.Name), c.NativeSessionID, c.ArchiveSessionID}, {archive.CanonicalHarness(c.Harness.Name), c.ParentNativeSessionID, c.ParentArchiveSessionID}}
			}
			if err := s.reconcileIdentityRecord(path, records, guard); err != nil {
				return false, err
			}
		}
		m.Offset = next
		if finished {
			m.Phase++
			m.Offset = 0
			m.Complete = m.Phase == 2
		}
	}
	if err := s.saveIdentityMigration(m, guard); err != nil {
		return false, err
	}
	return m.Complete, nil
}
func (s *Store) saveIdentityMigration(m identityMigration, guard func() error) error {
	unlock, err := local.NamedLock(s.home, "hooks.lock")
	if err != nil {
		return err
	}
	defer unlock()
	if guard != nil {
		if err := guard(); err != nil {
			return err
		}
	} else if err := config.ProtectIdentityWriter(s.home); err != nil {
		return err
	}
	return local.Write(filepath.Join(s.home, "identity-migration.json"), m)
}

func identityNames(path string, offset int64, count int) ([]string, int64, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, true, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = f.Close() }()
	// Readdir's buffered offset is not a durable cookie. ReadDirent consumes a
	// single bounded kernel batch instead (shared helper below).
	return identityDirBatch(f, offset, count)
}
func (s *Store) reconcileIdentityRecord(path string, records []identityRecord, guard func() error) error {
	unlock, err := local.NamedLock(s.home, "hooks.lock")
	if err != nil {
		return err
	}
	defer unlock()
	if guard != nil {
		if err := guard(); err != nil {
			return err
		}
	} else if err := config.ProtectIdentityWriter(s.home); err != nil {
		return err
	}
	// Re-read only this bounded record; reject changes rather than persisting a
	// stale ownership claim. Retention's durable tombstone is checked per identity.
	if len(records) == 1 {
		var r archive.SessionRegistration
		if err := local.Read(path, &r); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if r.NativeSessionID != records[0].Native || r.ArchiveSessionID != records[0].ID || archive.CanonicalHarness(r.Harness.Name) != records[0].Agent {
			return errors.New("identity changed during migration")
		}
	} else {
		var c SubagentCandidate
		if err := local.Read(path, &c); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if c.NativeSessionID != records[0].Native || c.ArchiveSessionID != records[0].ID || c.ParentNativeSessionID != records[1].Native || c.ParentArchiveSessionID != records[1].ID || archive.CanonicalHarness(c.Harness.Name) != records[0].Agent {
			return errors.New("candidate changed during migration")
		}
	}
	for _, r := range records {
		if r.Agent == "" || r.Native == "" || len(r.Native) > 4096 || !safeFileComponent(r.ID) {
			return errors.New("invalid identity migration record")
		}
		if _, removed, err := s.Removal(r.Agent, r.Native); err != nil {
			return err
		} else if removed {
			continue
		}
		if err := s.journalIdentity(r.Agent, r.Native, r.ID); err != nil {
			return fmt.Errorf("migrate identity: %w", err)
		}
	}
	return nil
}

// Empty old state can finish without enumerating historical records under a lock.
func (s *Store) ensureIdentityReady() error {
	ready, err := s.identityReady()
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.home, "identity-migration.json")); err == nil {
		return ErrIdentityMigrationPending
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, dir := range []string{"registrations", "subagent-candidates"} {
		f, err := os.Open(filepath.Join(s.home, dir))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		names, err := f.Readdirnames(1)
		_ = f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(names) > 0 {
			return ErrIdentityMigrationPending
		}
	}
	return local.Write(filepath.Join(s.home, "identity-migration.json"), identityMigration{Complete: true, Phase: 2})
}
