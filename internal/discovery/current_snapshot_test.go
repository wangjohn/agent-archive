package discovery

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func privateLocator(t *testing.T, p *privateIndex, id string) string {
	t.Helper()
	params := url.Values{"mode": {"ro"}, "immutable": {"1"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p.path, RawQuery: params.Encode()}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var path string
	if err := db.QueryRowContext(t.Context(), "SELECT rollout_path FROM threads WHERE id=?", id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	return path
}

func nativeIndexBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := filepath.Join(root, "state_5.sqlite") + suffix
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[suffix] = raw
	}
	return out
}

func TestPrivateCurrentSnapshotReadsCommittedWALWithoutNativeWrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, true)
	addHint(t, db, "thread", "active-rollout", time.Now())
	before := nativeIndexBytes(t, root)
	snapshot, err := snapshotCurrentIndex(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.close() }()
	if got := privateLocator(t, snapshot, "thread"); got != "active-rollout" {
		t.Fatal(got)
	}
	after := nativeIndexBytes(t, root)
	for name, raw := range before {
		if !bytes.Equal(raw, after[name]) {
			t.Fatalf("native file modified: %s", name)
		}
	}
	info, err := os.Stat(snapshot.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	dir, err := os.Stat(snapshot.dir)
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatal(dir, err)
	}
	if snapshot.metrics.NativeBytes <= 0 || snapshot.metrics.PrivateBytes != info.Size() || snapshot.metrics.PeakBuffers > currentSnapshotLimit {
		t.Fatal(snapshot.metrics)
	}
}

func TestPrivateCurrentSnapshotAcceptsAppendBeyondCapturedCommit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, true)
	addHint(t, db, "thread", "captured", time.Now())
	snapshot, err := snapshotCurrentIndex(t.Context(), root, func(stage string) {
		if stage == "wal" {
			if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", "later", "thread"); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.close() }()
	if got := privateLocator(t, snapshot, "thread"); got != "captured" {
		t.Fatal(got)
	}
}

func TestPrivateCurrentSnapshotRejectsCheckpointResetAndReplacement(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"generation", "main", "wal", "verified"} {
		for _, mutation := range []string{"checkpoint", "reset", "replace"} {
			t.Run(stage+"/"+mutation, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				db := hintDatabase(t, root, true)
				addHint(t, db, "thread", "captured", time.Now())
				snapshot, err := snapshotCurrentIndex(t.Context(), root, func(at string) {
					if at != stage {
						return
					}
					switch mutation {
					case "checkpoint":
						if _, err := db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(FULL)"); err != nil {
							t.Fatal(err)
						}
					case "reset":
						if _, err := db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
							t.Fatal(err)
						}
						if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path='after-reset'"); err != nil {
							t.Fatal(err)
						}
					case "replace":
						path := filepath.Join(root, "state_5.sqlite")
						raw, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path+".new", raw, 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(path+".new", path); err != nil {
							t.Fatal(err)
						}
					}
				})
				if snapshot != nil {
					_ = snapshot.close()
					t.Fatal("accepted mutated native index")
				}
				if !errors.Is(err, errIndexChanged) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestPrivateCurrentSnapshotRejectsCorruptCommittedWALAndCancellation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, true)
	addHint(t, db, "thread", "active", time.Now())
	wal := filepath.Join(root, "state_5.sqlite-wal")
	raw, err := os.ReadFile(wal)
	if err != nil {
		t.Fatal(err)
	}
	raw[56] ^= 1
	if err := os.WriteFile(wal, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := snapshotCurrentIndex(t.Context(), root, nil); snapshot != nil || !errors.Is(err, errIndexChanged) {
		t.Fatal(snapshot, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if snapshot, err := snapshotCurrentIndex(ctx, root, nil); snapshot != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(snapshot, err)
	}
}
