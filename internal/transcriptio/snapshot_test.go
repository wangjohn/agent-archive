package transcriptio

import (
	"context"
	"errors"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotFixedBoundaryAndChanges(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(OS{}, path, OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.WriteFile(path, []byte("first\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(s.Reader(context.Background()))
	if err != nil || string(data) != "first\n" {
		t.Fatalf("fixed boundary: %q %v", data, err)
	}
	if !errors.Is(s.Check(), ErrChanged) {
		t.Fatal("changed file accepted")
	}
}
func TestSnapshotRejectsDiscoveredSymlinksAndEscapes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(outside, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	s, err := Open(OS{}, link, OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(OS{}, link, OpenPolicy{RejectSymlinks: true, Root: root}); err == nil {
		s.Close()
		t.Fatal("discovered symlink accepted")
	}
	if s, err := Open(OS{}, outside, OpenPolicy{Root: root}); err == nil {
		s.Close()
		t.Fatal("store escape accepted")
	}
}
func TestStampDistinguishesReplacementWithSameSizeAndTime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := Open(OS{}, path, OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	stamp := first.Stamp()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp.ModifiedAt, stamp.ModifiedAt); err != nil {
		t.Fatal(err)
	}
	second, err := Open(OS{}, path, OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if stamp.SameFile(second.Stamp()) {
		t.Fatal("replacement has same identity")
	}
}

type swappingOpener struct {
	OS
	swap   func()
	closed *bool
}

func (o swappingOpener) OpenRegular(path string) (File, error) {
	o.swap()
	f, err := o.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return trackedFile{File: f, closed: o.closed}, nil
}

type trackedFile struct {
	File
	closed *bool
}

func (f trackedFile) Close() error { *f.closed = true; return f.File.Close() }
func TestSnapshotClosesFileReplacedBetweenVerificationAndOpen(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	closed := false
	opener := swappingOpener{closed: &closed, swap: func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}}
	s, err := Open(opener, path, OpenPolicy{RejectSymlinks: true, Root: root})
	if s != nil {
		_ = s.Close()
		t.Fatal("replaced file accepted")
	}
	if !errors.Is(err, ErrChanged) || !closed {
		t.Fatalf("race error=%v closed=%v", err, closed)
	}
}
