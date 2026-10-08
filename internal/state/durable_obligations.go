package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// DurableStorageNamespace identifies an accounting root, never content ownership.
type DurableStorageNamespace string

const (
	// PendingPublicationStorage holds selecting records and anonymous atomic copies.
	PendingPublicationStorage DurableStorageNamespace = "pending-publication"
	// GenerationRecoveryStorage holds the existing generation recovery journal.
	GenerationRecoveryStorage DurableStorageNamespace = "generation-recovery"
	// PendingHistoryStorage holds current history stages, including source-only leftovers.
	PendingHistoryStorage DurableStorageNamespace = "pending-history"
	// PublicationEvidenceStorage holds opaque original publication evidence.
	PublicationEvidenceStorage DurableStorageNamespace = "publication-evidence"
	// UnavailableStorage holds obligations whose runtime owner is not installed.
	UnavailableStorage DurableStorageNamespace = "unavailable-owner"
)

// DurableStorageObligation is a visible local obligation independent of registration.
type DurableStorageObligation struct {
	SessionID string
	Namespace DurableStorageNamespace
}

func rootHasEntries(root *os.Root, path string) (owed bool, err error) {
	dir, err := privateDirectory(root, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	defer func() {
		err = errors.Join(err, dir.Close())
		if err != nil {
			owed = true
		}
	}()
	d, err := dir.Open(".")
	if err != nil {
		return true, err
	}
	defer func() {
		err = errors.Join(err, d.Close())
		if err != nil {
			owed = true
		}
	}()
	entries, err := d.ReadDir(1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return len(entries) > 0 || err != nil, err
}
func (s *Store) hasStorageEntries(path string) (owed bool, err error) {
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() {
		err = errors.Join(err, home.Close())
		if err != nil {
			owed = true
		}
	}()
	return rootHasEntries(home.Root, path)
}
func (s *Store) hasPendingHistorySources(id string) (bool, error) {
	return s.hasStorageEntries(filepath.Join("sessions", id, "pending-sources"))
}
func (s *Store) hasPublicationEvidence(id string) (bool, error) {
	return s.hasStorageEntries(filepath.Join("publication-evidence", id))
}
func (s *Store) protectedStorage(id string) (bool, error) {
	history, herr := s.hasPendingHistorySources(id)
	evidence, eerr := s.hasPublicationEvidence(id)
	return history || evidence || herr != nil || eerr != nil, errors.Join(herr, eerr)
}

// DurableStorageObligations probes bounded private roots and small completed
// generation receipts without inventing registrations. Unknown roots remain
// recovery work, never deletion permission.
func (s *Store) DurableStorageObligations() (out []DurableStorageObligation, err error) {
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return []DurableStorageObligation{{Namespace: UnavailableStorage}}, errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	cfg, found, e := s.loadDurableInspectionConfig(home)
	if e != nil {
		return []DurableStorageObligation{{Namespace: UnavailableStorage}}, errors.Join(ErrDurableStorageRecovery, e)
	}
	pending, _, used, e := s.inspectPendingRoots(home, found && !cfg.DurableStorageProtection)
	for _, obligation := range pending {
		if found && obligation.Namespace == GenerationRecoveryStorage && obligation.SessionID != "" {
			complete, receiptErr := s.completedGenerationReceipt(home, obligation.SessionID)
			if receiptErr != nil {
				out = append(out, obligation)
				return out, errors.Join(ErrDurableStorageRecovery, receiptErr)
			}
			if complete {
				continue
			}
		}
		out = append(out, obligation)
	}
	if e != nil {
		return append(out, DurableStorageObligation{Namespace: UnavailableStorage}), errors.Join(ErrDurableStorageRecovery, e)
	}
	remaining := durableEntryLimit - used
	for _, base := range []string{"sessions", "publication-evidence"} {
		dir, e := privateDirectory(home.Root, base, false)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			out = append(out, DurableStorageObligation{Namespace: UnavailableStorage})
			err = errors.Join(err, e)
			continue
		}
		d, e := dir.Open(".")
		if e != nil {
			_ = dir.Close()
			return out, e
		}
		for {
			entries, e := d.ReadDir(128)
			if e != nil && !errors.Is(e, io.EOF) {
				err = errors.Join(err, e)
				break
			}
			for _, entry := range entries {
				remaining--
				if remaining < 0 {
					err = errors.Join(err, ErrDurableStorageRecovery)
					break
				}
				info, x := dir.Lstat(entry.Name())
				if x != nil {
					err = errors.Join(err, x)
					continue
				}
				if base == "sessions" && info.Mode().IsRegular() {
					continue
				}
				if !safeFileComponent(entry.Name()) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					out = append(out, DurableStorageObligation{Namespace: UnavailableStorage})
					err = errors.Join(err, ErrDurableStorageRecovery)
					continue
				}
				path := filepath.Join(base, entry.Name())
				namespace := PublicationEvidenceStorage
				if base == "sessions" {
					path = filepath.Join(path, "pending-sources")
					namespace = PendingHistoryStorage
				}
				present, x := rootHasEntries(home.Root, path)
				err = errors.Join(err, x)
				if present {
					remaining--
					if remaining < 0 {
						err = errors.Join(err, ErrDurableStorageRecovery)
						break
					}
					out = append(out, DurableStorageObligation{SessionID: entry.Name(), Namespace: namespace})
				}
			}
			if remaining < 0 || errors.Is(e, io.EOF) {
				break
			}
		}
		err = errors.Join(err, d.Close(), dir.Close())
		if remaining < 0 {
			return out, errors.Join(err, ErrDurableStorageRecovery)
		}
	}
	unavailable, e := unavailableStorageObligations(home.Root, &remaining)
	out = append(out, unavailable...)
	err = errors.Join(err, e)
	if err != nil {
		err = errors.Join(ErrDurableStorageRecovery, err)
	}
	return out, errors.Join(err, home.Check())
}

func unavailableStorageObligations(root *os.Root, remaining *int) (out []DurableStorageObligation, err error) {
	for _, dir := range []string{"admission-stages", "temporary-reservations", "temporary-scratch"} {
		present, e := rootHasEntries(root, dir)
		err = errors.Join(err, e)
		if present {
			*remaining--
			if *remaining < 0 {
				return out, ErrDurableStorageRecovery
			}
			out = append(out, DurableStorageObligation{Namespace: UnavailableStorage})
		}
	}
	return out, err
}
