package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

// TemporaryOwner identifies the provider responsible for scratch cleanup.
type TemporaryOwner string

const (
	// CursorAdmission owns native Cursor admission snapshots.
	CursorAdmission TemporaryOwner = "cursor-admission"
	// PublicationPrivacy owns publication privacy scratch.
	PublicationPrivacy TemporaryOwner = "publication-privacy"
)

const temporaryReservationDir = "temporary-reservations"
const temporaryScratchDir = "temporary-scratch"
const temporaryControlBytes int64 = 64 << 10

type temporaryManifest struct {
	Owner   TemporaryOwner `json:"owner"`
	Key     string         `json:"key"`
	Token   string         `json:"token"`
	Root    string         `json:"root"`
	Charged int64          `json:"charged"`
	Version int            `json:"version"`
}

// TemporaryReservation durably charges scratch until verified removal. Failed
// or abandoned reservations remain capacity consumers across process restart.
type TemporaryReservation struct {
	mu       sync.Mutex
	store    *Store
	root     *os.Root
	manifest temporaryManifest
	bytes    int64
	err      error
	closed   bool
}

var _ agentapi.TemporaryWorkspaceBudget = (*TemporaryReservation)(nil)

// NewTemporaryReservation returns an allocated-nothing scratch handle. The
// archive home must already exist and be canonical. Reserve serializes with
// stage quota writes; callers acquiring collector.lock must acquire it first.
func NewTemporaryReservation(s *Store, owner TemporaryOwner, key string) (*TemporaryReservation, error) {
	switch owner {
	case CursorAdmission, PublicationPrivacy:
	default:
		return nil, ErrAdmissionStageRecovery
	}
	if s == nil || !safeFileComponent(key) {
		return nil, ErrAdmissionStageRecovery
	}
	canonical, err := filepath.EvalSymlinks(s.home)
	if err != nil || !filepath.IsAbs(canonical) {
		return nil, ErrAdmissionStageRecovery
	}
	token, err := local.ID()
	if err != nil {
		return nil, err
	}
	canonicalStore := *s
	canonicalStore.home = canonical
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	return &TemporaryReservation{store: &canonicalStore, root: root, manifest: temporaryManifest{Owner: owner, Key: key, Token: token, Root: filepath.Join(temporaryScratchDir, token), Version: 1}}, nil
}

// Root returns the individually owned absolute scratch root without creating it.
func (r *TemporaryReservation) Root() string { return filepath.Join(r.store.home, r.manifest.Root) }

// OpenWorkspace creates and opens only this reservation's owned scratch root
// through its held home capability, after a positive durable reservation. The
// caller closes this handle before Close cleans the owned workspace.
func (r *TemporaryReservation) OpenWorkspace() (*os.Root, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil || r.bytes <= 0 || r.manifest.Owner != CursorAdmission {
		return nil, ErrAdmissionStageRecovery
	}
	if err := r.checkDirectories(); err != nil {
		return nil, err
	}
	if err := r.root.MkdirAll(r.manifest.Root, 0700); err != nil {
		return nil, err
	}
	before, err := r.root.Lstat(r.manifest.Root)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrAdmissionStageRecovery
	}
	root, err := r.root.OpenRoot(r.manifest.Root)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.Join(ErrAdmissionStageRecovery, root.Close())
	}
	return root, nil
}

// Err returns any retained cleanup or accounting error.
func (r *TemporaryReservation) Err() error { r.mu.Lock(); defer r.mu.Unlock(); return r.err }

// Reserve durably charges the worst-case bytes before any scratch allocation.
func (r *TemporaryReservation) Reserve(n int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil || n <= 0 || n > AdmissionStageQuota-temporaryControlBytes-r.bytes {
		return ErrAdmissionStageCapacity
	}
	unlock, err := r.store.namedLockWait("temporary-quota", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	used, err := r.store.admissionStageUsage()
	if err != nil {
		return err
	}
	additional := n
	if r.bytes == 0 {
		additional += temporaryControlBytes
	}
	if used > AdmissionStageQuota-additional {
		return ErrAdmissionStageCapacity
	}
	if err = r.checkDirectories(); err != nil {
		return err
	}
	if r.bytes == 0 {
		entries, e := os.ReadDir(filepath.Join(r.store.home, temporaryReservationDir))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		for _, entry := range entries {
			b, e := readStageFile(filepath.Join(r.store.home, temporaryReservationDir, entry.Name()), temporaryControlBytes)
			var m temporaryManifest
			if e != nil || json.Unmarshal(b, &m) != nil {
				return ErrAdmissionStageRecovery
			}
			if m.Owner == r.manifest.Owner && m.Key == r.manifest.Key {
				return ErrAdmissionStageRecovery
			}
		}
	}
	if err = r.root.MkdirAll(temporaryReservationDir, 0700); err != nil {
		return err
	}
	m := r.manifest
	m.Charged = r.bytes + n + temporaryControlBytes
	if err = local.Write(filepath.Join(r.store.home, temporaryReservationDir, m.Token+".json"), m); err != nil {
		r.err = err
		return err
	}
	r.manifest = m
	r.bytes += n
	return nil
}

func (r *TemporaryReservation) checkDirectories() error {
	for _, dir := range []string{temporaryReservationDir, temporaryScratchDir} {
		info, err := r.root.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrAdmissionStageRecovery
		}
	}
	return nil
}

// Release releases only after the entire owned scratch root is absent. Partial
// release is deliberately conservative; callers observe failures through Err.
func (r *TemporaryReservation) Release(n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || n != r.bytes || n <= 0 {
		r.err = errors.Join(r.err, ErrAdmissionStageRecovery)
		return
	}
	r.err = errors.Join(r.err, r.release())
}

func (r *TemporaryReservation) release() error {
	if _, err := r.root.Lstat(r.manifest.Root); !errors.Is(err, os.ErrNotExist) {
		return ErrAdmissionStageRecovery
	}
	unlock, err := r.store.namedLockWait("temporary-quota", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	if err = r.checkDirectories(); err != nil {
		return err
	}
	if err = r.root.Remove(filepath.Join(temporaryReservationDir, r.manifest.Token+".json")); err != nil {
		return err
	}
	dir, err := r.root.Open(temporaryReservationDir)
	if err != nil {
		return err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err == nil {
		r.bytes = 0
	}
	return err
}

// Close attempts only owned scratch cleanup, retaining durable capacity on any
// failure. It never removes native input or another reservation's workspace.
func (r *TemporaryReservation) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.err
	}
	if r.bytes > 0 {
		if err := r.checkDirectories(); err != nil {
			r.err = errors.Join(r.err, err)
		} else if err = r.root.RemoveAll(r.manifest.Root); err != nil {
			r.err = errors.Join(r.err, err)
		} else {
			r.err = errors.Join(r.err, r.release())
		}
	}
	r.closed = true
	r.err = errors.Join(r.err, r.root.Close())
	return r.err
}

func (s *Store) temporaryUsage() (int64, error) {
	dir := filepath.Join(s.home, temporaryReservationDir)
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return 0, ErrAdmissionStageRecovery
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var used int64
	roots := map[string]bool{}
	keys := map[string]bool{}
	for _, e := range entries {
		b, err := readStageFile(filepath.Join(dir, e.Name()), temporaryControlBytes)
		var m temporaryManifest
		if err != nil || json.Unmarshal(b, &m) != nil || m.Version != 1 || !safeFileComponent(m.Token) || e.Name() != m.Token+".json" || !safeFileComponent(m.Key) || m.Root != filepath.Join(temporaryScratchDir, m.Token) || m.Charged < temporaryControlBytes || m.Charged > AdmissionStageQuota-used {
			return 0, ErrAdmissionStageRecovery
		}
		switch m.Owner {
		case CursorAdmission, PublicationPrivacy:
		default:
			return 0, ErrAdmissionStageRecovery
		}
		key := string(m.Owner) + ":" + m.Key
		if keys[key] {
			return 0, ErrAdmissionStageRecovery
		}
		keys[key] = true
		used += m.Charged
		roots[m.Token] = true
	}
	scratchDir := filepath.Join(s.home, temporaryScratchDir)
	if info, e := os.Lstat(scratchDir); e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return 0, ErrAdmissionStageRecovery
	}
	scratch, err := os.ReadDir(filepath.Join(s.home, temporaryScratchDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	for _, e := range scratch {
		if !roots[e.Name()] || !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			return 0, ErrAdmissionStageRecovery
		}
	}
	return used, nil
}
