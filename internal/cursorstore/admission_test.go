//go:build darwin || linux

package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

func admissionWorkspace(t *testing.T) *state.TemporaryReservation {
	t.Helper()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := state.NewTemporaryReservation(s, state.CursorAdmission, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func TestAdmissionPinnedLiveCopyCoherentAcrossExternalGrowth(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "space ü", "state.vscdb")
	w := startWriter(t, path)
	before := `{"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"before"}]}`
	w.put(map[string]string{"composerData:c": before, "padding": strings.Repeat("p", 256<<10)})
	workspace := admissionWorkspace(t)
	var pinnedCheckpointWAL int64
	copied, stats, err := prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{afterStep: func(n int) {
		runtime.GC()
		if n == 1 {
			w.do(writerCommand{Op: writerDelete, Key: "composerData:c"})
			w.put(map[string]string{"composerData:c": `{"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"after"}]}`, "growth": strings.Repeat("g", 512<<10)})
			w.do(writerCommand{Op: writerCheckpoint})
			info, e := os.Stat(path + "-wal")
			if e != nil {
				t.Fatal(e)
			}
			pinnedCheckpointWAL = info.Size()
			if pinnedCheckpointWAL == 0 {
				t.Fatal("external truncate checkpoint ignored pinned reader")
			}

		}
	}})
	if err != nil {
		t.Fatal(stats, err)
	}
	db, err := sql.Open("sqlite", dsn(copied, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err = db.QueryRowContext(t.Context(), "SELECT value FROM cursorDiskKV WHERE key='composerData:c'").Scan(&got); err != nil || got != before {
		t.Fatal("unpinned snapshot", got, err)
	}
	if stats.Steps < 2 || stats.ExternalWALAfter <= stats.ExternalWALBefore {
		t.Fatal("missing actual growth observations", stats)
	}
	info, err := os.Stat(copied)
	if err != nil || info.Size() != stats.Pages*stats.PageBytes {
		t.Fatal(info, stats, err)
	}
	w.do(writerCommand{Op: writerCheckpoint})
	walAfterClose, e := os.Stat(path + "-wal")
	if e != nil || walAfterClose.Size() != 0 {
		t.Fatal("checkpoint remained pinned after copy close", walAfterClose, e)
	}
	t.Logf("external truncate checkpoint WAL bytes while pinned=%d after pinned transaction closes=%d", pinnedCheckpointWAL, walAfterClose.Size())
	if stats.DestinationWriteCalls <= 0 || stats.DestinationMaxWriteEnd > info.Size() || stats.ProhibitedOpens != 0 || stats.ProhibitedWrites != 0 || stats.DestinationOpenDescriptors != 0 {
		t.Fatal("missing before-allocation guard evidence", stats)
	}
	t.Logf("actual pinned copy steps=%d pages=%d page_bytes=%d scratch=%d external_wal_before=%d external_wal_after=%d destination_opens=%d writes=%d write_bytes=%d max_write_end=%d", stats.Steps, stats.Pages, stats.PageBytes, info.Size(), stats.ExternalWALBefore, stats.ExternalWALAfter, stats.DestinationOpens, stats.DestinationWriteCalls, stats.DestinationWriteBytes, stats.DestinationMaxWriteEnd)
}

func TestAdmissionLiveCopyCancellationLeavesCleanupOwned(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": chat("c", 1), "padding": strings.Repeat("p", 256<<10)})
	before := snapshotDir(t, filepath.Dir(path))
	workspace := admissionWorkspace(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	copied, stats, err := prepareAdmissionDatabase(ctx, path, workspace, admissionCopyOptions{afterStep: func(int) { cancel() }})
	if copied != "" || !errors.Is(err, context.Canceled) || stats.Steps != 1 {
		t.Fatal(copied, stats, err)
	}
	after := snapshotDir(t, filepath.Dir(path))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("cancel changed native database", before, after)
	}
	if err = workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned cleanup failed", err)
	}
}

func TestAdmissionReadOnlyLiveCopyLeavesAllNativeBytes(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": chat("c", 1), "padding": strings.Repeat("p", 256<<10)})
	before := snapshotDir(t, filepath.Dir(path))
	workspace := admissionWorkspace(t)
	_, stats, err := PrepareAdmissionDatabase(t.Context(), path, workspace)
	if err != nil {
		t.Fatal(stats, err)
	}
	assertUnchanged(t, filepath.Dir(path), before)
}

func TestAdmissionDestinationRejectsWritesBeforeAllocation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	vfs, err := newAdmissionDestinationVFS(t.Context(), path, f, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer vfs.Close()
	db, err := sql.Open("sqlite", admissionDestinationDSN(path, vfs.name, 512, 1000))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), "CREATE TABLE t(v BLOB); INSERT INTO t VALUES (zeroblob(65536))"); err == nil {
		t.Fatal("uncapped driver allocation succeeded")
	} else {
		t.Log("bounded actual driver refusal", err)
	}
	actual, err := os.Stat(path)
	if err != nil || actual.Size() > 4096 || vfs.deniedWrites.Load() == 0 {
		t.Fatal(actual, vfs.deniedWrites.Load(), vfs.deniedOpens.Load(), err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("implicit sidefile allocated", entries, err)
	}
}

func TestAdmissionSourceSidefileDisappearanceNeverCreatesNativeFiles(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.vscdb")
			w := startWriter(t, path)
			w.put(map[string]string{"composerData:c": chat("c", 1)})
			workspace := admissionWorkspace(t)
			copied, _, err := prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{beforeSourceOpen: func() {
				if e := os.Remove(path + suffix); e != nil {
					t.Fatal(e)
				}
			}})
			if copied != "" || err == nil {
				t.Fatal("disappearance admitted", copied, err)
			}
			if _, err = os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("reader recreated native sidefile", err)
			}
			if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed pin allocated scratch", err)
			}
		})
	}
}

func TestAdmissionSourceRacedSymlinkCannotReadOutsideIdentity(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": chat("c", 1)})
	outside := filepath.Join(t.TempDir(), "outside.db")
	writeDB(t, outside, false, map[string]any{"composerData:outside": chat("outside", 1)})
	before := snapshotDir(t, filepath.Dir(outside))
	workspace := admissionWorkspace(t)
	copied, _, err := prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{beforeSourceOpen: func() {
		if e := os.Rename(path, path+".old"); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(outside, path); e != nil {
			t.Fatal(e)
		}
	}})
	if copied != "" || err == nil {
		t.Fatal("opened raced identity", copied, err)
	}
	assertUnchanged(t, filepath.Dir(outside), before)
	if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("raced source allocated scratch", err)
	}
}

func TestAdmissionGeometryAndExpiredCallerRefuseBeforeScratch(t *testing.T) {
	t.Parallel()
	for _, geometry := range [][2]int64{{0, 1}, {513, 1}, {65537, 1}, {4096, 0}, {4096, -1}, {4096, 1 << 62}, {65536, 2049}} {
		if _, err := admissionImageBytes(geometry[0], geometry[1]); err == nil {
			t.Fatal("unbounded geometry", geometry)
		}
	}
	if n, err := admissionImageBytes(65536, 2048); err != nil || n != admissionImageLimit {
		t.Fatal(n, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	workspace := admissionWorkspace(t)
	_, stats, err := PrepareAdmissionDatabase(ctx, filepath.Join(t.TempDir(), "missing.db"), workspace)
	if !errors.Is(err, context.Canceled) || stats.Steps != 0 {
		t.Fatal(stats, err)
	}
	if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled caller allocated scratch", err)
	}
}

func TestAdmissionDestinationGeometryCapabilityActualDriver(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{512, 4096, 65536} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "native.db")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			path, err = filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatal(err)
			}
			vfs, err := newAdmissionDestinationVFS(t.Context(), path, f, 128<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer vfs.Close()
			if err = verifyAdmissionDestination(t.Context(), admissionDestinationDSN(path, vfs.name, size, 100), size, 100); err != nil {
				t.Fatal(err)
			}
			if info, e := os.Stat(path); e != nil || info.Size() != 0 {
				t.Fatal("geometry probe allocated disk pages", info, e)
			}
		})
	}
}

func TestAdmissionDestinationAncestorSwapCannotWriteOutside(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": `{"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"owned"}]}`, "padding": strings.Repeat("p", 256<<10)})
	workspace := admissionWorkspace(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "native.db")
	sentinel := []byte("outside untouched")
	if err := os.WriteFile(outsideFile, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(workspace.Root())
	retained := parent + "-retained"
	restored := false
	restore := func() {
		if !restored {
			if err := os.Remove(parent); err != nil {
				t.Error(err)
			}
			if err := os.Rename(retained, parent); err != nil {
				t.Error(err)
			}
			restored = true
		}
	}
	_, stats, err := prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{beforeDestinationOpen: func() {
		if e := os.Rename(parent, retained); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(outside, parent); e != nil {
			t.Fatal(e)
		}
		deferRestore := restore
		t.Cleanup(deferRestore)
	}})
	if err != nil {
		t.Fatal("descriptor-bound backup failed", stats, err)
	}
	actual, err := os.ReadFile(outsideFile)
	if err != nil || !reflect.DeepEqual(actual, sentinel) {
		t.Fatal("outside destination modified", string(actual), err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatal("outside allocation", entries, err)
	}
	// Readback refuses the swapped archive path rather than treating it as content.
	if err = readAdmissionWorkspace(t.Context(), workspace, &recoveryTestBudget{rows: 100, bytes: 1 << 20}, func(context.Context, *sql.DB) error { return nil }); err == nil {
		t.Fatal("readback trusted swapped ancestor")
	}
	restore()
	root, err := workspace.OpenWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err = verifyAdmissionImage(root, stats.Pages*stats.PageBytes); err != nil {
		t.Fatal(err)
	}
	if stats.DestinationOpenDescriptors != 0 {
		t.Fatal("native descriptors remain active", stats)
	}
}

func TestAdmissionPrivateDestinationRetainsNativeTablesUntilReaderClose(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.db")
	held, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	vfs, err := newAdmissionDestinationVFS(t.Context(), path, held, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer vfs.Close()
	db, err := sql.Open("sqlite", admissionDestinationDSN(path, vfs.name, 512, 8))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(t.Context(), "CREATE TABLE t(v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err = vfs.Close(); err == nil {
		t.Fatal("native table freed with active connection")
	}
	runtime.GC()
	var count int
	if err = db.QueryRowContext(t.Context(), "SELECT count(*) FROM t").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if vfs.activeDescriptors.Load() != 0 {
		t.Fatal("native file survived connection close")
	}
	// Defer performs the single successful unregister/free after all readers close.
}

func TestAdmissionDestinationShortReadZeroFillsNativeBuffer(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.db")
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	tls := libc.NewTLS()
	defer tls.Close()
	buffer := libc.Xmalloc(tls, 8)
	if buffer == 0 {
		t.Fatal("native buffer allocation failed")
	}
	defer libc.Xfree(tls, buffer)
	copy(libc.GoBytes(buffer, 8), []byte("xxxxxxxx"))
	vfs := &admissionVFS{context: t.Context(), heldFile: held}
	if rc := admissionReadDestination(tls, vfs, buffer, 8, 0); rc != sqlite3.SQLITE_IOERR_SHORT_READ {
		t.Fatal(rc)
	}
	if got := string(libc.GoBytes(buffer, 8)); got != "abc\x00\x00\x00\x00\x00" {
		t.Fatal("short read left native garbage", []byte(got))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	vfs.context = ctx
	if rc := admissionReadDestination(tls, vfs, buffer, 8, 0); rc != sqlite3.SQLITE_INTERRUPT {
		t.Fatal("cancelled native read", rc)
	}
}
