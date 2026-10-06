package backfill

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

const cursorRecoveryRows = 1024
const cursorRecoveryBytes = 128 << 20
const cursorRecoveryRenewals = 8
const cursorRecoveryRecords = 65536

type recoveryCatalogState struct{}

func (recoveryCatalogState) Classify(string, string) (SkipReason, error) { return "", nil }

type recoveryFileStamp struct {
	info   fs.FileInfo
	absent bool
}

func recoveryStamps(env Environment, paths []string) ([]recoveryFileStamp, bool) {
	stamps := make([]recoveryFileStamp, len(paths))
	for i, path := range paths {
		info, err := env.lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			stamps[i].absent = true
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return nil, false
		}
		stamps[i].info = info
	}
	return stamps, true
}
func sameRecoveryStamps(a, b []recoveryFileStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for i, old := range a {
		fresh := b[i]
		if old.absent != fresh.absent {
			return false
		}
		if !old.absent && (!os.SameFile(old.info, fresh.info) || old.info.Size() != fresh.info.Size() || !old.info.ModTime().Equal(fresh.info.ModTime()) || old.info.Mode() != fresh.info.Mode()) {
			return false
		}
	}
	return true
}

type cursorRecoveryEpoch struct {
	works    []*work
	complete bool
	budget   bool
	digest   [32]byte
	paths    []string
	stamps   []recoveryFileStamp
	settled  bool
}

// readCursorRecoveryEpoch validates supported database candidates before any
// output filter. It retains only content-free ownership/eligibility facts.
func readCursorRecoveryEpoch(ctx context.Context, env Environment, r *resolver, unread unreadable) (epoch cursorRecoveryEpoch, err error) {
	path := env.cursorStateDatabase()
	if path != "" {
		epoch.paths = []string{path, path + "-wal", path + "-shm", path + "-journal"}
	}
	before, stable := recoveryStamps(env, epoch.paths)
	if env.CursorRecoveryDatabase == nil {
		epoch.complete = env.CursorDatabase == nil && (path == "" || (stable && before[0].absent))
		epoch.stamps, epoch.settled = before, stable
		return epoch, nil
	}
	if unread.cursorIncomplete {
		return epoch, nil
	}
	res, err := env.CursorRecoveryDatabase(ctx, cursorRecoveryRows, cursorRecoveryBytes)
	if err != nil {
		if fatalSourceFailure(err) {
			return epoch, err
		}
		return epoch, nil
	}
	if res.Close != nil {
		defer func() { err = errors.Join(err, res.Close()) }()
	}
	epoch.budget = res.RecoveryBudgetExhausted
	if !res.Checked || res.ReadRecoverySnapshot == nil {
		return epoch, nil
	}
	if len(res.Chats) > cursorRecoveryRows {
		epoch.budget = true
		return epoch, nil
	}
	epoch.paths = append(epoch.paths, cursorRecoveryWorkspacePaths(env, res.Chats)...)
	// Bracket workspace observations as well as database reads. A workspace
	// changed while resolving its folder cannot become a settled witness.
	allBefore, allStable := recoveryStamps(env, epoch.paths)
	works, toRead, err := selectCursorDatabaseChats(res.Chats, nil, recoveryCatalogState{}, true, time.Time{}, time.Time{})
	if err != nil {
		return epoch, err
	}
	readChat, readSnapshot, budget := cursorRecoveryReaders(res, cursorRecoveryBytes-res.RecoveryBytes)
	if err := readCursorDatabaseChats(ctx, env, 1, readChat, readSnapshot, toRead, env.Sources); err != nil {
		if ctx.Err() != nil || fatalSourceFailure(err) {
			return epoch, err
		}
		epoch.budget = budget.exhausted
		return epoch, nil
	}
	epoch.complete, err = observeCursorRecoveryWorks(ctx, env, r, works, budget)
	if err != nil {
		return epoch, err
	}
	epoch.budget = epoch.budget || budget.exhausted
	epoch.works = works
	epoch.digest = cursorRecoveryDigest(works)
	settleCursorRecoveryEpoch(env, &epoch, before, allBefore, stable, allStable)

	return epoch, nil
}

// prepareCursorRecoveryWitnesses makes one prefilter evidence observation,
// then validates settled database/side-state/workspace stamps per admission
// slice. Virtual/unsettled sources require one renewed bounded epoch instead.
func prepareCursorRecoveryWitnesses(ctx context.Context, env Environment, r *resolver, items []*work, unread unreadable) ([]*work, bool, error) {
	relevant := false
	for _, w := range items {
		if !env.exists(w.t.cwd) && (archive.IsRepoKey(w.t.repoKey) || r.filters.ProjectMappings[filepath.Clean(w.t.cwd)] != "") {
			relevant = true
			break
		}
	}
	if !relevant {
		return nil, false, nil
	}
	epoch, err := readCursorRecoveryEpoch(ctx, env, r, unread)
	if err != nil {
		return nil, false, err
	}
	renewals := 0
	r.databaseRecoveryCurrent = func(ctx context.Context) bool {
		if !epoch.complete || ctx.Err() != nil {
			return false
		}
		if epoch.settled {
			current, ok := recoveryStamps(env, epoch.paths)
			return ok && sameRecoveryStamps(epoch.stamps, current)
		}
		if renewals >= cursorRecoveryRenewals {
			return false // A new plan is needed to renew an unsettled source again.
		}
		renewals++
		renewed, err := readCursorRecoveryEpoch(ctx, env, r, unread)
		return err == nil && renewed.complete && renewed.digest == epoch.digest
	}
	r.recoveryInventoryBudget = epoch.budget
	return epoch.works, !epoch.complete, nil
}

type cursorRecoveryReadBudget struct {
	remaining int64
	rows      int
	exhausted bool
}

func (b *cursorRecoveryReadBudget) RemainingRows() int    { return b.rows }
func (b *cursorRecoveryReadBudget) RemainingBytes() int64 { return b.remaining }
func (b *cursorRecoveryReadBudget) Charge(rows int, bytes int64) error {
	if b.exhausted || rows < 0 || bytes < 0 || rows > b.rows || bytes > b.remaining {
		b.exhausted = true
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	b.rows -= rows
	b.remaining -= bytes
	return nil
}
func (b *cursorRecoveryReadBudget) charge(c cursorstore.Composer) error {
	size := int64(len(c.Composer))
	for _, bubble := range c.Bubbles {
		size += int64(len(bubble.Value))
	}
	return b.Charge(0, size)
}
func cursorRecoveryReaders(res CursorDatabaseResult, remaining int64) (func(context.Context, string) (cursorstore.Composer, error), func(context.Context, string) (cursorstore.Composer, agentapi.SourceSnapshot, error), *cursorRecoveryReadBudget) {
	budget := res.recoveryReadBudget
	if budget == nil {
		budget = &cursorRecoveryReadBudget{remaining: remaining, rows: cursorRecoveryRecords}
	}
	readSnapshot := func(ctx context.Context, id string) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
		if budget.exhausted || budget.remaining <= 0 {
			budget.exhausted = true
			return cursorstore.Composer{}, nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
		}
		c, snap, err := res.ReadRecoverySnapshot(ctx, id, budget)
		if agentapi.HasFailure(err, agentapi.Limit) {
			budget.exhausted = true
		}
		return c, snap, err
	}
	return nil, readSnapshot, budget
}

func cursorRecoveryDigest(works []*work) [32]byte {
	// Membership, native identity, original creation, format eligibility and
	// ownership all participate. Hashes stay private; no message bytes are kept.
	type fact struct {
		Chat                                                CursorDatabaseChat
		Root                                                string
		Skip                                                SkipReason
		Unsafe, Empty, Large, Vanished, Duplicate, Mismatch bool
	}
	facts := make([]fact, 0, len(works))
	for _, w := range works {
		facts = append(facts, fact{w.chat, w.res.root, w.res.skip, w.unsafe, w.empty, w.tooLarge, w.vanished, w.duplicate, w.t.identityMismatch})
	}
	sort.Slice(facts, func(i, j int) bool {
		a, _ := json.Marshal(facts[i])
		b, _ := json.Marshal(facts[j])
		return string(a) < string(b)
	})
	data, _ := json.Marshal(facts)
	return sha256.Sum256(data)
}

func cursorRecoveryWorkspacePaths(env Environment, chats []CursorDatabaseChat) []string {
	var paths []string
	for _, chat := range chats {
		if id := chat.WorkspaceID; id != "" && id == filepath.Base(id) && id != "." && id != ".." {
			paths = append(paths, filepath.Join(cursorWorkspaceStorage(env), id, "workspace.json"))
		}
	}
	return paths
}

func observeCursorRecoveryWorks(ctx context.Context, env Environment, r *resolver, works []*work, budget *cursorRecoveryReadBudget) (bool, error) {
	complete := !budget.exhausted
	workspaceEnv := env
	var workspaceErr error
	workspaceEnv.ReadFile = recoveryWorkspaceReader(ctx, env, budget, &complete, &workspaceErr)
	for _, w := range works {
		if w.vanished || w.duplicate || w.unsafe || w.tooLarge || w.t.identityMismatch || w.empty {
			complete = false
			continue
		}
		folder := cursorChatFolder(workspaceEnv, w.chat, w.messageFolders)
		w.t.cwd = folder
		w.res = r.resolve(folder)
		if w.res.skip == SkipProjectUnknown {
			complete = false
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return complete, workspaceErr
}

func settleCursorRecoveryEpoch(env Environment, epoch *cursorRecoveryEpoch, before, allBefore []recoveryFileStamp, stable, allStable bool) {
	after, ok := recoveryStamps(env, epoch.paths)
	if stable && allStable && ok && len(before) > 0 && sameRecoveryStamps(before, after[:len(before)]) && sameRecoveryStamps(allBefore, after) && (!before[0].absent || len(epoch.works) == 0) {
		epoch.stamps, epoch.settled = after, true
	} else if !allStable || !ok || !sameRecoveryStamps(allBefore, after) || (len(before) > 0 && !sameRecoveryStamps(before, after[:min(len(before), len(after))])) {
		epoch.complete = false
	}
}
