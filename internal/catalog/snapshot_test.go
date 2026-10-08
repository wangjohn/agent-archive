package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func activateFixture(t *testing.T, w *Writer) {
	t.Helper()
	c := w.Coordinator()
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(t.Context(), owner, "private synthetic proof"); err != nil {
		t.Fatal(err)
	}
	if err = c.Release(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotBoundedRangeCountsCursorsAndBodyAuthority(t *testing.T) {
	w, store := fixture(t)
	for i := range 140 {
		m := mutation(t, w, fmt.Sprintf("session-%04d", i))
		if _, err := w.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	activateFixture(t, w)
	measured := storagetest.NewMeasuredStore(store, 0)
	cache := &NodeCache{}
	s, err := OpenSnapshot(t.Context(), measured, cache)
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Index: CaptureIndex, Lower: OrderPrefix(false) + "0", Upper: OrderPrefix(false) + ":", Reverse: true}
	count, err := s.Count(t.Context(), q)
	if err != nil || count != 140 {
		t.Fatalf("count %d %v", count, err)
	}
	p, err := s.Query(t.Context(), q, "", 50)
	if err != nil || len(p.Rows) != 50 || p.Next == "" {
		t.Fatalf("page %d %v", len(p.Rows), err)
	}
	for i, row := range p.Rows {
		if row.Entry.Summary.SessionID != fmt.Sprintf("session-%04d", i) {
			t.Fatalf("tie order %s", row.Entry.Summary.SessionID)
		}
	}
	next, err := s.Query(t.Context(), q, p.Next, 50)
	if err != nil || next.Rows[0].Entry.Summary.SessionID != "session-0050" {
		t.Fatalf("continuation %+v %v", next, err)
	}
	changed := q
	changed.Lower = "other"
	if _, err = s.Query(t.Context(), changed, p.Next, 50); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("query cursor accepted", err)
	}
	reopened, err := OpenSnapshot(t.Context(), measured, cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Query(t.Context(), q, p.Next, 50); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("reopened cursor accepted", err)
	}
	before := measured.Metrics()
	if _, err = reopened.Query(t.Context(), q, "", 1); err != nil {
		t.Fatal(err)
	}
	cost := measured.Metrics()
	if cost.Lists != 0 || cost.Gets-before.Gets > 2 {
		t.Fatalf("warm node cost %+v before %+v", cost, before)
	}
	row := p.Rows[0]
	old, err := s.ReadMetadata(t.Context(), row.Entry)
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, w, "session-0000")
	m.ID = "successor"
	m.ExpectedRevision = row.Entry.Revision
	if _, err = w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	pinned, err := s.ReadMetadata(t.Context(), row.Entry)
	if err != nil || string(old) != string(pinned) {
		t.Fatal("selected body switched roots", err)
	}
	if _, err = s.Query(t.Context(), q, p.Next, 50); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("changed head cursor accepted", err)
	}
	s.started = time.Now().Add(-SnapshotLifetime)
	if _, err = s.ReadMetadata(t.Context(), row.Entry); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("expired body accepted", err)
	}
}

func TestSnapshotMissingCorruptAndCandidateFailClosed(t *testing.T) {
	w, store := fixture(t)
	m := mutation(t, w, "one")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSnapshot(t.Context(), store, nil); err == nil {
		t.Fatal("candidate treated complete")
	}
	activateFixture(t, w)
	s, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	head, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(t.Context(), head.Capture.Key, []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Query(t.Context(), Query{Index: CaptureIndex}, "", 1); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatal(err)
	}
	if err = store.Delete(t.Context(), head.Identity.Key); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Find(t.Context(), m.SessionKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCoordinatorDrainsGloballyAndNeverTimesOut(t *testing.T) {
	w, store := fixture(t)
	c := w.Coordinator()
	if err := c.Admit(t.Context(), "remote machine/history", "digest", nil); err != nil {
		t.Fatal(err)
	}
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = restarted.Coordinator().HeldBarrier(owner).Hold(t.Context()); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("unfinished owner drained", err)
	}
	if err = restarted.Coordinator().Release(t.Context(), owner); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("unfinished owner recovered", err)
	}
	if err = c.Admit(t.Context(), "remote machine/history", "digest", nil); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("duplicate ownership accepted", err)
	}
	if err = c.Complete(t.Context(), "remote machine/history", "wrong"); !errors.Is(err, ErrMutationReuse) {
		t.Fatal(err)
	}
	if err = c.Complete(t.Context(), "remote machine/history", "digest"); err != nil {
		t.Fatal(err)
	}
	_, release, err := c.HeldBarrier(owner).Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, w, "blocked")
	if _, err = w.Commit(t.Context(), m); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("direct commit crossed global seal", err)
	}
	remote, err := Wrap(store)
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Put(t.Context(), "sessions/claude/blocked/source", []byte("bytes")); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("source crossed seal", err)
	}
	release()
	if _, err = w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
}

func TestOpeningAlreadyConsumedLifetimeFails(t *testing.T) {
	w, store := fixture(t)
	m := mutation(t, w, "one")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	if _, err := openSnapshotStarted(t.Context(), store, nil, true, time.Now().Add(-SnapshotLifetime)); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("opening extended consumed lifetime", err)
	}
}

func TestPendingInvocationExclusiveAndRestartClosed(t *testing.T) {
	w, raw := fixture(t)
	mutation := mutation(t, w, "pending")
	data, err := json.Marshal(mutation.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.BeginPublication(t.Context(), "frozen", data); err != nil {
		t.Fatal(err)
	}
	if err = store.BeginPublication(t.Context(), "frozen", data); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("parallel duplicate admitted", err)
	}
	store.EndPublicationAttempt("frozen")
	if err = store.BeginPublication(t.Context(), "frozen", data); err != nil {
		t.Fatal("owned sequential retry refused", err)
	}
	restarted, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.BeginPublication(t.Context(), "frozen", data); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("unresolved restart ownership accepted", err)
	}
	if err = store.CompletePublication(t.Context(), "frozen", data); err != nil {
		t.Fatal(err)
	}
	store.EndPublicationAttempt("frozen")
}

func TestIndexCountsDeletionSplitsAndRootReplayDiscriminators(t *testing.T) {
	w, store := fixture(t)
	var mutations []CatalogMutation
	for i := range 100 {
		m := mutation(t, w, fmt.Sprintf("session-%04d", i))
		if i%3 == 0 {
			m.Next.Summary.ParentSessionID = "parent"
		}
		if i%5 == 0 {
			m.Next.Summary.Replay = &archive.Replay{}
		}
		raw, err := json.Marshal(m.Next.Summary)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := w.PutImmutable(t.Context(), KindMetadata, raw)
		if err != nil {
			t.Fatal(err)
		}
		m.Next.Metadata = ref
		revision, err := w.Commit(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		m.ExpectedRevision = revision
		mutations = append(mutations, m)
	}
	activateFixture(t, w)
	for step := range 2 {
		s, err := OpenSnapshot(t.Context(), store, nil)
		if err != nil {
			t.Fatal(err)
		}
		live, roots, children := 0, map[bool]uint64{}, map[bool]uint64{}
		for i, m := range mutations {
			if step == 1 && i%2 == 0 {
				continue
			}
			live++
			replay := m.Next.Summary.Replay != nil
			if m.Next.Summary.ParentSessionID == "" {
				roots[replay]++
			} else {
				children[replay]++
			}
		}
		for _, idx := range []Index{CaptureIndex, ActivityIndex, ProjectIndex} {
			total, err := s.Count(t.Context(), Query{Index: idx})
			if err != nil || total != uint64(live) {
				t.Fatalf("all-session aggregate %s %d want %d %v", idx, total, live, err)
			}
			for _, replay := range []bool{false, true} {
				p := OrderPrefix(replay)
				count, err := s.Count(t.Context(), Query{Index: idx, Lower: p, Upper: p + "~"})
				if err != nil || count != roots[replay] {
					t.Fatal("root count", idx, count, roots[replay], err)
				}
				kind := "ordinary/"
				if replay {
					kind = "replay/"
				}
				p = "!children/" + kind
				count, err = s.Count(t.Context(), Query{Index: idx, Lower: p, Upper: p + "~"})
				if err != nil || count != children[replay] {
					t.Fatal("child count", idx, count, children[replay], err)
				}
			}
		}
		if step == 0 {
			for i, m := range mutations {
				if i%2 == 0 {
					if _, err = w.Commit(t.Context(), CatalogMutation{ID: "delete/" + m.ID, SessionKey: m.SessionKey, ExpectedRevision: m.ExpectedRevision}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}
