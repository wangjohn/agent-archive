package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

const admissionImageLimit int64 = 128 << 20

const admissionStepPages int32 = 16

const admissionBusyRetries = 64

// AdmissionCopyStats reports bounded copy work and external WAL observations.
// External WAL bytes are not owned or capped by the provider reservation.
type AdmissionCopyStats struct {
	WorkspaceImage             bool
	Steps                      int
	BusyRetries                int
	Pages                      int64
	PageBytes                  int64
	ExternalWALBefore          int64
	ExternalWALAfter           int64
	DestinationWriteCalls      int
	DestinationWriteBytes      int64
	DestinationMaxWriteEnd     int64
	ProhibitedWrites           int
	DestinationOpens           int
	ProhibitedOpens            int
	DestinationOpenDescriptors int
}

type admissionCopyOptions struct {
	afterStep             func(int)
	beforeSourceOpen      func()
	beforeDestinationOpen func()
}

// PrepareAdmissionDatabase returns settled input or a bounded immutable live
// snapshot in the caller's workspace. Only admission uses this path. The caller
// closes SQLite readers before closing the workspace; failed cleanup retains
// durable quota. No ordinary snapshot or unbounded backup is invoked.
func PrepareAdmissionDatabase(ctx context.Context, path string, workspace agentapi.TemporaryWorkspaceBudget) (string, AdmissionCopyStats, error) {
	return prepareAdmissionDatabase(ctx, path, workspace, admissionCopyOptions{})
}

func prepareAdmissionDatabase(ctx context.Context, path string, workspace agentapi.TemporaryWorkspaceBudget, opts admissionCopyOptions) (out string, stats AdmissionCopyStats, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	src, err := resolve(path)
	if err != nil {
		return "", stats, err
	}
	if !src.live {
		return src.path, stats, nil
	}
	if workspace == nil {
		return "", stats, errors.Join(errors.New("cursor import remains unadmitted; live admission requires reserved private scratch"), NotChecked(Locked))
	}
	if err = workspace.Err(); err != nil {
		return "", stats, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	allowed, err := admissionSourceFiles(src)
	if err != nil {
		return "", stats, err
	}
	stats.ExternalWALBefore = allowed[src.path+"-wal"].Size()
	err = withAdmissionSource(ctx, src, allowed, opts, func(conn *sql.Conn, tx *sql.Tx) error {
		imageBytes, e := pinAdmissionGeometry(ctx, tx, &stats)
		if e != nil {
			return e
		}
		out, e = copyAdmissionIntoWorkspace(ctx, conn, workspace, imageBytes, &stats, opts)
		return errors.Join(e, checkAdmissionNativeFiles(src, allowed, &stats))
	})
	if err != nil {
		return "", stats, err
	}
	if err = ctx.Err(); err != nil {
		return "", stats, err
	}
	stats.WorkspaceImage = true
	return out, stats, nil
}

func admissionSourceFiles(src source) (map[string]os.FileInfo, error) {
	allowed := map[string]os.FileInfo{src.path: src.before}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Lstat(src.path + suffix)
		if err != nil || !info.Mode().IsRegular() {
			return nil, NotChecked(Locked)
		}
		allowed[src.path+suffix] = info
	}
	return allowed, nil
}

func withAdmissionSource(ctx context.Context, src source, allowed map[string]os.FileInfo, opts admissionCopyOptions, copyPinned func(*sql.Conn, *sql.Tx) error) (err error) {
	vfs, err := newAdmissionVFS(allowed, false, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, vfs.Close()) }()
	if opts.beforeSourceOpen != nil {
		opts.beforeSourceOpen()
	}
	db, err := sql.Open("sqlite", admissionDSN(dsn(src.path, true), vfs.name))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, tx.Rollback()) }()
	return copyPinned(conn, tx)
}

func pinAdmissionGeometry(ctx context.Context, tx *sql.Tx, stats *AdmissionCopyStats) (int64, error) {
	// BEGIN alone is deferred. A real read pins this SAME driver connection
	// before geometry is observed or any provider scratch is allocated.
	var count int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&count); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&stats.PageBytes); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&stats.Pages); err != nil {
		return 0, err
	}
	return admissionImageBytes(stats.PageBytes, stats.Pages)
}

func copyAdmissionIntoWorkspace(ctx context.Context, conn *sql.Conn, workspace agentapi.TemporaryWorkspaceBudget, imageBytes int64, stats *AdmissionCopyStats, opts admissionCopyOptions) (out string, err error) {
	if err = workspace.Reserve(imageBytes); err != nil {
		return "", err
	}
	rooted, ok := workspace.(interface{ OpenWorkspace() (*os.Root, error) })
	if !ok {
		return "", errors.New("cursor admission requires a rooted reserved workspace")
	}
	root, err := rooted.OpenWorkspace()
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	f, err := root.OpenFile("native.db", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	out = filepath.Join(workspace.Root(), "native.db")
	dest, err := newAdmissionDestinationVFS(ctx, out, f, imageBytes)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, dest.Close()) }()
	defer dest.copyStats(stats)
	if opts.beforeDestinationOpen != nil {
		opts.beforeDestinationOpen()
	}
	dst := admissionDestinationDSN(out, dest.name, stats.PageBytes, stats.Pages)
	if err = verifyAdmissionDestination(ctx, dst, stats.PageBytes, stats.Pages); err != nil {
		return "", err
	}
	if err = runAdmissionBackup(ctx, conn, dst, stats, opts); err != nil {
		return "", err
	}
	if err = verifyAdmissionImage(root, imageBytes); err != nil {
		return "", err
	}
	return out, ctx.Err()
}

func runAdmissionBackup(ctx context.Context, conn *sql.Conn, dst string, stats *AdmissionCopyStats, opts admissionCopyOptions) error {
	return conn.Raw(func(driverConn any) (err error) {
		b, ok := driverConn.(backuper)
		if !ok {
			return errors.New("bounded cursor admission backup capability unavailable")
		}
		bk, err := b.NewBackup(dst)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, bk.Finish()) }()
		maxSteps := (stats.Pages + int64(admissionStepPages) - 1) / int64(admissionStepPages)
		for int64(stats.Steps) < maxSteps {
			if err = ctx.Err(); err != nil {
				return err
			}
			more, e := bk.Step(admissionStepPages)
			if e != nil {
				if !busy(e) || stats.BusyRetries >= admissionBusyRetries {
					return e
				}
				stats.BusyRetries++
				timer := time.NewTimer(backupRetry)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
				continue
			}
			stats.Steps++
			if opts.afterStep != nil {
				opts.afterStep(stats.Steps)
			}
			if int64(bk.PageCount()) != stats.Pages {
				return errors.New("cursor admission pinned geometry changed")
			}
			if !more {
				return ctx.Err()
			}
		}
		return errBackupIncomplete
	})
}

func checkAdmissionNativeFiles(src source, allowed map[string]os.FileInfo, stats *AdmissionCopyStats) (err error) {
	sides := existingSideFiles(src.path)
	if len(sides) != 2 || !sides["-wal"] || !sides["-shm"] {
		return NotChecked(Locked)
	}
	if after, e := os.Lstat(src.path + "-wal"); e == nil {
		stats.ExternalWALAfter = after.Size()
	} else {
		err = e
	}
	for path, before := range allowed {
		after, e := os.Lstat(path)
		if e != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
			err = errors.Join(err, NotChecked(Locked))
		}
	}
	return err
}

func verifyAdmissionImage(root *os.Root, imageBytes int64) error {
	copied, err := root.Stat("native.db")
	if err != nil || copied.Size() != imageBytes {
		return errors.New("cursor admission image geometry is incomplete")
	}
	entries, err := root.Open(".")
	if err != nil {
		return err
	}
	names, err := entries.Readdirnames(2)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	err = errors.Join(err, entries.Close())
	if err != nil || len(names) != 1 || names[0] != "native.db" {
		return errors.New("cursor admission unexpected scratch sidefile")
	}
	return nil
}

func admissionDSN(dsn, name string) string {
	u, _ := url.Parse(dsn)
	q := u.Query()
	q.Set("vfs", name)
	u.RawQuery = q.Encode()
	return u.String()
}

func admissionDestinationDSN(path, name string, pageSize, pages int64) string {
	q := url.Values{"mode": {"rw"}, "vfs": {name}}
	for _, pragma := range []string{"journal_mode(MEMORY)", "temp_store(MEMORY)", "mmap_size(0)", "page_size(" + strconv.FormatInt(pageSize, 10) + ")", "max_page_count(" + strconv.FormatInt(pages, 10) + ")"} {
		q.Add("_pragma", pragma)
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

func verifyAdmissionDestination(ctx context.Context, dsn string, pageSize, pages int64) (err error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	for _, check := range []struct {
		pragma   string
		expected string
	}{{"journal_mode", "memory"}, {"temp_store", "2"}, {"page_size", strconv.FormatInt(pageSize, 10)}, {"max_page_count", strconv.FormatInt(pages, 10)}} {
		var got string
		if err = db.QueryRowContext(ctx, "PRAGMA "+check.pragma).Scan(&got); err != nil {
			return err
		}
		if got != check.expected {
			return fmt.Errorf("cursor admission destination %s unsupported", check.pragma)
		}
	}
	return nil
}

func admissionImageBytes(pageSize, pages int64) (int64, error) {
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 || pages <= 0 || pages > admissionImageLimit/pageSize {
		return 0, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	return pages * pageSize, nil
}
