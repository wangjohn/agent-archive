package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// publishedProtocol classifies existing physical state under the held root.
// Classification only accounts bytes; it supplies no source or deletion authority.
func (q *durableQuota) publishedProtocol(path string, expected os.FileInfo) (int, error) {
	if err := q.store.durableContext().Err(); err != nil {
		return 0, err
	}
	if !expected.Mode().IsRegular() || expected.Size() < 0 {
		return 0, ErrDurableStorageRecovery
	}
	if expected.Size() > durableStorageQuota {
		return 0, ErrDurableStorageCapacity
	}
	n := expected.Size()
	if q.store.resourceBudget != nil {
		if !q.store.resourceBudget.Reserve(2 * n) {
			return 0, errStateBudget
		}
		defer q.store.resourceBudget.Release(2 * n)
	}
	f, err := q.home.Root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(expected, opened) {
		return 0, errors.Join(ErrDurableStorageRecovery, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, n+1))
	if err != nil || int64(len(raw)) != n {
		return 0, errors.Join(ErrDurableStorageRecovery, err)
	}
	var p publishedState
	if err = json.Unmarshal(raw, &p); err != nil {
		return 0, errors.Join(ErrDurableStorageRecovery, err)
	}
	named, err := q.home.Root.Lstat(path)
	if err != nil || !sameDurableStamp(expected, named) {
		return 0, errors.Join(ErrDurableStorageRecovery, err)
	}
	if err = q.guard.CheckHome(q.path); err != nil {
		return 0, err
	}
	if err = q.store.durableContext().Err(); err != nil {
		return 0, err
	}
	if p.PublicationVersion == 2 {
		return 2, nil
	}
	// Current ordinary blocked controls can precede the first readable bundle.
	// Their closed status/reason is legacy accounting, never source authority.
	if p.PublicationVersion == 0 && p.Status == CacheStatusBlocked && p.Bundle.ArchiveSessionID == "" && p.Bundle.NativeSessionID == "" && p.Commit == nil && len(p.Sources) == 0 && len(p.MetadataBytes) == 0 && p.LastPublished == nil && p.PublishedAt.IsZero() {
		switch p.BlockedReason {
		case BlockedReasonTranscriptRewritten, BlockedReasonTranscriptTooLarge, BlockedReasonRecordTooLarge, BlockedReasonTranscriptMissing:
			return 1, nil
		}
	}
	if p.PublicationVersion != 0 || p.Bundle.ArchiveSessionID == "" || p.Bundle.NativeSessionID == "" {
		return 0, ErrDurableStorageRecovery
	}
	return 1, nil
}

func (q *durableQuota) scanPublished(remaining *int, u *durableUsage) (err error) {
	root, err := privateDirectory(q.home.Root, "published", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	for {
		if err = q.store.durableContext().Err(); err != nil {
			return err
		}
		entries, e := dir.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		for _, entry := range entries {
			*remaining--
			if *remaining < 0 {
				return ErrDurableStorageRecovery
			}
			info, e := root.Lstat(entry.Name())
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() || info.Size() < 0 {
				return ErrDurableStorageRecovery
			}
			path := filepath.Join("published", entry.Name())
			supportedName := !strings.HasPrefix(entry.Name(), ".pending-") && strings.HasSuffix(entry.Name(), ".json") && safeFileComponent(strings.TrimSuffix(entry.Name(), ".json"))
			if supportedName {
				version, e := q.publishedProtocol(path, info)
				if e != nil {
					return e
				}
				if version == 1 {
					continue
				}
			} else {
				u.recovery = true
			}
			if info.Size() > (durableStorageQuota-u.charged)/2 {
				return ErrDurableStorageCapacity
			}
			u.physical += info.Size()
			u.charged += 2 * info.Size()
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
	}
}

func (q *durableQuota) publishedAdditional(path string, old os.FileInfo, n int64) (int64, error) {
	if n < 0 || n > durableStorageQuota/2 {
		return 0, ErrDurableStorageCapacity
	}
	o, c := int64(0), int64(0)
	if old != nil {
		o = old.Size()
		if o < 0 || o > durableStorageQuota {
			return 0, ErrDurableStorageCapacity
		}
		version, err := q.publishedProtocol(path, old)
		if err != nil {
			return 0, err
		}
		if version == 2 {
			c = 2 * o
		}
	}
	// n <= Q/2 and o <= Q: these expressions cannot overflow int64.
	return max(int64(0), o+n-c, 2*n-c), nil
}
