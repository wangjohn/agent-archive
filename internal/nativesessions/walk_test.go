package nativesessions

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type disk struct{}

func (disk) ReadDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }

func TestWalkStopsAtFileCapAndCancellation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	c, err := discoveryio.Walk(context.Background(), disk{}, StoreRoot{Harness: "claude", Depth: 1, Suffix: ".jsonl", Path: root}, 2, func(Ref) (bool, error) { n++; return true, nil })
	if err != nil || c.Complete || n != 2 || c.Enumerated != 2 {
		t.Fatalf("cap: %+v %d %v", c, n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err = discoveryio.Walk(ctx, disk{}, StoreRoot{Harness: "claude", Depth: 1, Suffix: ".jsonl", Path: root}, 0, func(Ref) (bool, error) { t.Fatal("canceled visit"); return true, nil })
	if !errors.Is(err, context.Canceled) || c.Complete {
		t.Fatalf("cancel: %+v %v", c, err)
	}
}

func BenchmarkWalk(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			root := b.TempDir()
			dir := filepath.Join(root, "project")
			if err := os.Mkdir(dir, 0700); err != nil {
				b.Fatal(err)
			}
			for i := range count {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%05d.jsonl", i)), nil, 0600); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				c, err := discoveryio.Walk(context.Background(), disk{}, StoreRoot{Harness: "claude", Depth: 1, Suffix: ".jsonl", Path: root}, 10000, func(Ref) (bool, error) { return true, nil })
				if err != nil || !c.Complete || c.Enumerated != count {
					b.Fatalf("%+v %v", c, err)
				}
			}
		})
	}
}

// Once the cap is reached, even empty sibling folders must stay unread.
func TestWalkFileCapStopsBeforeLaterDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a", "session.jsonl"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	files := &countingDirectories{}
	coverage, err := discoveryio.Walk(context.Background(), files, StoreRoot{Harness: "claude", Depth: 1, Suffix: ".jsonl", Path: root}, 1, func(Ref) (bool, error) { return true, nil })
	if err != nil || coverage.Complete || coverage.Enumerated != 1 || files.reads != 2 {
		t.Fatalf("coverage=%+v directory reads=%d err=%v", coverage, files.reads, err)
	}
}

type countingDirectories struct{ reads int }

func (d *countingDirectories) ReadDir(path string) ([]os.DirEntry, error) {
	d.reads++
	return os.ReadDir(path)
}
