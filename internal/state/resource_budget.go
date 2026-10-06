package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/local"
)

// WithReadBudget borrows the pass ledger for a session's local serialized and
// decoded inputs. The returned close ends ownership of that session's data;
// no Published or Pending value read through the handle may outlive it.
func (s *Store) WithReadBudget(ctx context.Context, budget *agentapi.NativeReadBudget) (*Store, func()) {
	scoped := *s
	scoped.resourceBudget = budget
	scoped.resourceContext = ctx
	scoped.resourceReleases = &[]func(){}
	var once sync.Once
	return &scoped, func() {
		once.Do(func() {
			for _, release := range *scoped.resourceReleases {
				release()
			}
			*scoped.resourceReleases = nil
		})
	}
}

var errStateBudget = agentapi.ReadBudgetLimit(errors.New("local state exceeds shared data budget"))

func (s *Store) readBudgeted(path string, value any, retain bool) error {
	if s.resourceBudget == nil {
		return local.Read(path, value)
	}
	if err := s.resourceContext.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	n := info.Size()
	if !info.Mode().IsRegular() || n < 0 {
		return errStateBudget
	}
	// Serialized input and decoded logical data coexist during Unmarshal.
	if !s.resourceBudget.Reserve(n) {
		return errStateBudget
	}
	defer s.resourceBudget.Release(n)
	if !s.resourceBudget.Reserve(n) {
		return errStateBudget
	}
	keep := false
	defer func() {
		if !keep {
			s.resourceBudget.Release(n)
		}
	}()
	data := make([]byte, n)
	if _, err := io.ReadFull(f, data); err != nil {
		return err
	}
	var extra [1]byte
	if count, err := f.Read(extra[:]); count != 0 || err != io.EOF {
		return errStateBudget
	}
	if err := s.resourceContext.Err(); err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return err
	}
	if retain {
		keep = true
		*s.resourceReleases = append(*s.resourceReleases, func() { s.resourceBudget.Release(n) })
	}
	return nil
}

func (s *Store) writeCompact(path string, value any) error {
	if s.resourceBudget == nil {
		return local.WriteCompact(path, value)
	}
	const scratch = 32 << 10
	if !s.resourceBudget.Reserve(scratch) {
		return errStateBudget
	}
	n, err := jsonwire.Bound(s.resourceContext, value, s.resourceBudget.Available()-1)
	s.resourceBudget.Release(scratch)
	if err != nil {
		return errors.Join(errStateBudget, err)
	}
	if !s.resourceBudget.Reserve(n + 1) {
		return errStateBudget
	}
	defer s.resourceBudget.Release(n + 1)
	return local.WriteCompact(path, value)
}
