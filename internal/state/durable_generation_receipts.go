package state

import (
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// Completed receipts are classified only for a global census. Directory
// observation caching never proves the contents of an in-place rewritten file.
func (s *Store) completedGenerationReceipt(home *local.RootedHome, id string) (complete bool, err error) {
	if err := s.durableContext().Err(); err != nil {
		return false, err
	}
	path := filepath.Join(generationRecoveryDir, id+".json")
	before, err := home.Root.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	if before.Size() > durableControlBytes {
		return false, nil
	}
	if s.resourceBudget != nil {
		if !s.resourceBudget.Reserve(4 * durableControlBytes) {
			return false, errStateBudget
		}
		defer s.resourceBudget.Release(4 * durableControlBytes)
	}
	file, err := home.Root.Open(path)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			complete = false
		}
	}()
	opened, err := file.Stat()
	if err != nil || !sameDurableStamp(before, opened) {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	var receipt generationRecovery
	decoder := json.NewDecoder(io.LimitReader(file, durableControlBytes+1))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&receipt)
	if decodeErr == nil {
		var extra any
		if !errors.Is(decoder.Decode(&extra), io.EOF) {
			decodeErr = ErrDurableStorageRecovery
		}
	}
	named, namedErr := home.Root.Lstat(path)
	if namedErr != nil || !sameDurableStamp(before, named) {
		return false, errors.Join(ErrDurableStorageRecovery, namedErr)
	}
	if err := errors.Join(home.Check(), s.durableContext().Err()); err != nil {
		return false, err
	}
	return decodeErr == nil && receipt.Version == 1 && receipt.Previous == id && safeFileComponent(receipt.Next) && receipt.Key.Validate() == nil && receipt.Complete && receipt.Registration == nil && receipt.Pending == nil && receipt.Request == nil, nil
}
