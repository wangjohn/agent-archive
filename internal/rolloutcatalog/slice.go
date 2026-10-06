package rolloutcatalog

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// BeginValidationSlice validates the entire epoch once under the caller's context.
// Without an explicit deadline the sweep has a thirty second bound. Validation
// failures belong to the returned slice; cancellation is checked before reuse.
func (c *Catalog) BeginValidationSlice(ctx context.Context, limits agentapi.CodexValidationLimits) (agentapi.CodexRolloutSlice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limits.Steps <= 0 {
		limits.Steps = 512
	}
	if limits.Duration <= 0 {
		limits.Duration = 30 * time.Second
	}
	if limits.Steps > 512 || limits.Duration > 60*time.Second {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("shorten catalog validation slice limits"))
	}
	bounded := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		bounded, cancel = context.WithTimeout(ctx, 30*time.Second)
	}
	defer cancel()
	err := c.ensure(bounded)
	if err == nil && c.invalid {
		err = failure("rollout selection epoch unavailable")
	}
	if err == nil {
		err = c.validate(bounded, true)
	}
	return &validationSlice{catalog: c, remaining: limits.Steps, expires: time.Now().Add(limits.Duration), validation: err}, nil
}

type validationSlice struct {
	catalog    *Catalog
	remaining  int
	expires    time.Time
	validation error
	closed     bool
}

func (s *validationSlice) Valid(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return failure("catalog validation slice closed; retry with a new slice")
	}
	if s.remaining <= 0 || !time.Now().Before(s.expires) {
		return agentapi.Wrap(agentapi.Limit, errors.New("catalog validation slice expired; close snapshots and retry with a new slice"))
	}
	if s.validation != nil {
		return s.validation
	}
	if s.catalog.invalid {
		return failure("rollout selection epoch unavailable; renew catalog")
	}
	return nil
}

func (s *validationSlice) step(ctx context.Context) error {
	if err := s.Valid(ctx); err != nil {
		return err
	}
	s.remaining--
	return nil
}

func (s *validationSlice) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	if err := s.step(ctx); err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	c := s.catalog
	key := strings.ToLower(id)
	current := c.current[key]
	if !c.complete {
		current = nil
	}
	return agentapi.CodexRolloutSet{Current: cloneRef(current), Candidates: slices.Clone(c.threads[key]), Revision: c.revisions[key], Complete: c.complete}, nil
}

func (s *validationSlice) Rollout(ctx context.Context, id string) ([]agentapi.SourceRef, error) {
	if err := s.step(ctx); err != nil {
		return nil, err
	}
	return slices.Clone(s.catalog.rollouts[strings.ToLower(id)]), nil
}

func (s *validationSlice) Check(ctx context.Context, id, revision string) error {
	if err := s.step(ctx); err != nil {
		return err
	}
	s.catalog.counters.Checks++
	if revision == "" || s.catalog.revisions[strings.ToLower(id)] != revision {
		return failure("rollout selection epoch unavailable")
	}
	return nil
}

func (s *validationSlice) Close() error { s.closed = true; return nil }

// Files preserves individually approved store authority for dependency opens.
func (s *validationSlice) Files() transcriptio.Opener { return s.catalog.Files() }
