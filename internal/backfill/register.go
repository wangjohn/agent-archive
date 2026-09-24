package backfill

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// ErrPaused stops registration when collection was paused while it ran.
var ErrPaused = errors.New("collection was paused during the import; run agent-archive resume, then agent-archive backfill to finish it")

// Hooks wait at most one second for hooks.lock and then drop their event, so
// registration holds it only briefly: at most maxHoldSteps steps or
// maxHold, whichever comes first, then leaves it free for holdGap, longer
// than a waiting hook's 10 ms retry interval, so a waiting hook gets it.
const (
	maxHoldSteps  = 50
	maxHold       = 100 * time.Millisecond
	holdGap       = 20 * time.Millisecond
	hooksLockWait = 10 * time.Second
)

// Registration is step 5 of an import: it registers the confirmed plan's
// sessions, in short holds of hooks.lock. The caller holds setup.lock and
// collector.lock throughout, so no collector pass runs between a session's
// subagent candidates and its registration.
type Registration struct {
	Home       string
	Store      *collector.LocalStore
	Batch      string
	AdmittedAt time.Time
	// MaxHoldSteps overrides maxHoldSteps; tests lower it to force several
	// holds.
	MaxHoldSteps int
	// AfterHold runs after each hold of hooks.lock is released, with the
	// sessions and subagents registered during it. The CLI appends them to
	// the batch file. An error stops registration.
	AfterHold func(sessions, subagents []string) error
}

// RegistrationResult counts what registration did with the plan's sessions.
type RegistrationResult struct {
	// Sessions and Subagents are the archive session IDs registered, and
	// given to subagent candidates.
	Sessions, Subagents []string
	// AlreadyArchived counts sessions a hook, or another run, registered
	// after the plan was made.
	AlreadyArchived int
	// Gone counts sessions whose transcript disappeared after the plan.
	Gone int
	// NotAdmitted counts sessions the configuration stopped accepting after
	// the plan, for example because their project was excluded.
	NotAdmitted int
}

// parentWork is one imported session in progress. Its subagent candidates
// are written before the session is registered, so a crash at any point
// leaves the session unregistered, and a rerun plans and registers it again;
// the candidates it left are reused, or discarded by the collector.
type parentWork struct {
	c        Candidate
	id       string
	next     int
	children []string
	links    []archive.SupplementalEvidence
}

// Run registers every candidate, in order.
func (r Registration) Run(candidates []Candidate) (RegistrationResult, error) {
	var result RegistrationResult
	works := make([]*parentWork, len(candidates))
	for i, c := range candidates {
		works[i] = &parentWork{c: c}
	}
	flushed := [2]int{}
	flush := func() error {
		sessions, subagents := result.Sessions[flushed[0]:], result.Subagents[flushed[1]:]
		flushed = [2]int{len(result.Sessions), len(result.Subagents)}
		if r.AfterHold == nil || (len(sessions) == 0 && len(subagents) == 0) {
			return nil
		}
		return r.AfterHold(sessions, subagents)
	}
	for i := 0; i < len(works); {
		err := r.hold(works, &i, &result)
		if flushErr := flush(); err == nil {
			err = flushErr
		}
		if err != nil {
			return result, err
		}
		if i < len(works) {
			time.Sleep(holdGap)
		}
	}
	return result, nil
}

// hold takes hooks.lock, rereads the configuration, and works through the
// candidates from *i until the hold's budget is spent.
func (r Registration) hold(works []*parentWork, i *int, result *RegistrationResult) error {
	unlock, err := local.NamedLockWait(r.Home, "hooks.lock", hooksLockWait)
	if err != nil {
		return fmt.Errorf("wait for capture hooks: %w", err)
	}
	defer unlock()
	cfg, found, err := config.Load(r.Home)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !found {
		return errors.New("the configuration disappeared during the import")
	}
	if cfg.Paused {
		return ErrPaused
	}
	limit := maxHoldSteps
	if r.MaxHoldSteps > 0 {
		limit = r.MaxHoldSteps
	}
	start := time.Now()
	for steps := 0; *i < len(works) && steps < limit && time.Since(start) < maxHold; steps++ {
		done, err := r.step(cfg, works[*i], result)
		if err != nil {
			return err
		}
		if done {
			*i++
		}
	}
	return nil
}

// step does one small piece of a session: check it and find its archive ID,
// write one subagent candidate, or register it. done is true when the
// session is finished, registered or skipped.
func (r Registration) step(cfg config.Config, w *parentWork, result *RegistrationResult) (done bool, err error) {
	c := w.c
	if w.id == "" {
		skip, err := r.skip(cfg, c, &result.AlreadyArchived, &result.Gone, &result.NotAdmitted)
		if err != nil || skip {
			return true, err
		}
		w.id, _, err = r.Store.EnsureArchiveSessionID(c.NativeSessionID)
		return false, err
	}
	if w.next < len(c.Subagents) {
		sub := c.Subagents[w.next]
		w.next++
		return false, r.subagent(w, sub)
	}
	// The configuration may have changed since the session was checked, in
	// an earlier hold.
	skip, err := r.skip(cfg, c, &result.AlreadyArchived, &result.Gone, &result.NotAdmitted)
	if err != nil || skip {
		return true, err
	}
	reg, err := r.Store.RegisterNewSession(c.NativeSessionID, func(id string) archive.SessionRegistration {
		return r.registration(c, id)
	})
	if err != nil {
		return true, fmt.Errorf("register an imported session: %w", err)
	}
	if err := r.Store.SaveRequest(reg.ArchiveSessionID, "backfill", r.AdmittedAt, w.links...); err != nil {
		return true, fmt.Errorf("queue an imported session: %w", err)
	}
	result.Sessions = append(result.Sessions, reg.ArchiveSessionID)
	result.Subagents = append(result.Subagents, w.children...)
	return true, nil
}

// skip reports whether a session is no longer imported, counting why: the
// configuration no longer accepts it, its transcript is gone, or a hook or
// another run registered it since the plan was made.
func (r Registration) skip(cfg config.Config, c Candidate, alreadyArchived, gone, notAdmitted *int) (bool, error) {
	if !cfg.AcceptSession(r.registration(c, "")) {
		*notAdmitted++
		return true, nil
	}
	if !regularFile(c.TranscriptPath) {
		*gone++
		return true, nil
	}
	id, found, err := r.Store.ArchiveSessionID(c.NativeSessionID)
	if err != nil || !found {
		return false, err
	}
	if _, registered, err := r.Store.LoadRegistration(id); err != nil || !registered {
		return false, err
	}
	*alreadyArchived++
	return true, nil
}

// subagent writes one subagent candidate, as a SubagentStop hook does, and
// keeps the link evidence for the parent's request. The collector validates
// and registers the child.
func (r Registration) subagent(w *parentWork, sub Subagent) error {
	if !regularFile(sub.Path) {
		return nil
	}
	c := w.c
	childNativeID := c.NativeSessionID + ":subagent:" + sub.AgentID
	childID, _, err := r.Store.EnsureArchiveSessionID(childNativeID)
	if err != nil {
		return fmt.Errorf("assign a subagent archive session ID: %w", err)
	}
	if _, registered, err := r.Store.LoadRegistration(childID); err != nil || registered {
		// A hook registered this subagent already.
		return err
	}
	if err := r.Store.SaveSubagentCandidate(collector.SubagentCandidate{
		ArchiveSessionID: childID, NativeSessionID: childNativeID,
		ParentArchiveSessionID: w.id, ParentNativeSessionID: c.NativeSessionID,
		ProjectID: archive.ProjectID(c.ProjectRoot), ProjectRoot: c.ProjectRoot, Harness: archive.Harness{Name: c.Harness},
		AgentID: sub.AgentID, TranscriptPath: sub.Path, ObservedAt: r.AdmittedAt, Origin: archive.SessionOriginImport,
	}); err != nil {
		return fmt.Errorf("record a subagent: %w", err)
	}
	link, err := archive.NewLinkedSessionEvidence(childID, archive.LinkedSessionPending, r.AdmittedAt)
	if err != nil {
		return fmt.Errorf("filter subagent link: %w", err)
	}
	w.links = append(w.links, link)
	w.children = append(w.children, childID)
	return nil
}

// registration is the imported session's registration: its true start, the
// import's admission, and the batch.
func (r Registration) registration(c Candidate, archiveID string) archive.SessionRegistration {
	return archive.SessionRegistration{
		ArchiveSessionID: archiveID,
		NativeSessionID:  c.NativeSessionID,
		ProjectID:        archive.ProjectID(c.ProjectRoot),
		ProjectRoot:      c.ProjectRoot,
		Harness:          archive.Harness{Name: c.Harness},
		TranscriptPath:   c.TranscriptPath,
		SessionStartedAt: c.StartedAt,
		StartedAtSource:  c.StartedAtSource,
		RegisteredAt:     r.AdmittedAt,
		AdmittedAt:       r.AdmittedAt,
		Origin:           archive.SessionOriginImport,
		ImportBatch:      r.Batch,
	}
}

// regularFile reports whether path is still a regular file, without
// following a symlink.
func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
