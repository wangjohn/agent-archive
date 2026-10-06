package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestPrivateAppendRejectsUncertainGenerationAndReleases(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"checkpoint", "reset", "malformed", "cancel", "budget"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			db := hintDatabase(t, root, true)
			addHint(t, db, "selected", "original", time.Now())
			p, err := snapshotCurrentIndex(t.Context(), root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.close() }()
			addHint(t, db, "unrelated", "other", time.Now())
			ctx := t.Context()
			budget := agentapi.NewNativeReadBudget(currentSnapshotLimit)
			switch change {
			case "checkpoint":
				_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(FULL)")
			case "reset":
				_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
			case "malformed":
				f, e := os.OpenFile(filepath.Join(root, "state_5.sqlite-wal"), os.O_WRONLY, 0600)
				if e != nil {
					t.Fatal(e)
				}
				_, err = f.WriteAt([]byte{0xff}, int64(p.proof.end+24))
				_ = f.Close()
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "budget":
				budget = agentapi.NewNativeReadBudget(1)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := appendCurrentIndex(ctx, root, p, budget); err == nil {
				t.Fatal("uncertainty accepted")
			}
			used, _ := budget.Charged()
			if used != 0 {
				t.Fatal("leaked", used)
			}
			if got := privateLocator(t, p, "selected"); got != "original" {
				t.Fatal(got)
			}
		})
	}
}

func TestPrivateAppendIgnoresTornUncommittedTail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, true)
	addHint(t, db, "selected", "original", time.Now())
	p, err := snapshotCurrentIndex(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()
	addHint(t, db, "unrelated", "other", time.Now())
	wal := filepath.Join(root, "state_5.sqlite-wal")
	f, err := os.OpenFile(wal, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write([]byte{1, 2, 3})
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(currentSnapshotLimit)
	if err := appendCurrentIndex(t.Context(), root, p, budget); err != nil {
		t.Fatal(err)
	}
	if got := privateLocator(t, p, "unrelated"); got != "other" {
		t.Fatal(got)
	}
	used, _ := budget.Charged()
    if used != 0 {
		t.Fatal(used)
	}
}

func TestPrivateCurrentSnapshotSettledTruncatedWAL(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, true)
	addHint(t, db, "selected", "settled", time.Now())
	if _, err := db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	before := nativeIndexBytes(t, root)
	p, err := snapshotCurrentIndex(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()
	if got := privateLocator(t, p, "selected"); got != "settled" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(root, "state_5.sqlite-wal")); errors.Is(err, os.ErrNotExist) {
		t.Fatal("native WAL removed")
	}
	after := nativeIndexBytes(t, root)
	for key, raw := range before {
		if string(raw) != string(after[key]) {
			t.Fatal("native write", key)
		}
	}
}
