package discoveryio

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

type regularEntry string

func (entry regularEntry) Name() string { return string(entry) }

func (regularEntry) IsDir() bool { return false }

func (regularEntry) Type() fs.FileMode { return 0 }

func (entry regularEntry) Info() (fs.FileInfo, error) { return entry, nil }

func (regularEntry) Size() int64 { return 1 }

func (regularEntry) Mode() fs.FileMode { return 0600 }

func (regularEntry) ModTime() time.Time { return time.Time{} }

func (regularEntry) Sys() any { return nil }

type syntheticDiscoveryFiles struct{ entries []fs.DirEntry }

func (files syntheticDiscoveryFiles) ReadDir(string) ([]fs.DirEntry, error) {
	return files.entries, nil
}

func (syntheticDiscoveryFiles) Lstat(string) (fs.FileInfo, error) {
	return regularEntry("synthetic"), nil
}

func (syntheticDiscoveryFiles) Open(string) (io.ReadCloser, error) {
	panic("synthetic inspector does not read content")
}

type syntheticInspector struct{}

func (syntheticInspector) Roots(agentapi.NativeLocations, agentapi.DiscoveryPurpose) []agentapi.NativeStoreRoot {
	return nil
}

func (syntheticInspector) InspectHeader(agentapi.NativeHeaderRequest) (agentapi.NativeHeader, error) {
	return agentapi.NativeHeader{NativeID: "synthetic"}, nil
}

func TestDisabledNameDeduplicationDoesNotBuildIdentityMap(t *testing.T) {
	entries := make([]fs.DirEntry, 128)
	for index := range entries {
		entries[index] = regularEntry(fmt.Sprintf("%03d.jsonl", index))
	}
	request := agentapi.DiscoveryRequest{Purpose: agentapi.DiscoveryImport, Stage: agentapi.DiscoveryIdentities, HeaderBytes: 1, RecordBytes: 1, Files: syntheticDiscoveryFiles{entries: entries}}
	roots := []agentapi.NativeStoreRoot{{Path: "synthetic", Suffix: ".jsonl"}}
	measure := func(deduplicate bool) float64 {
		return testing.AllocsPerRun(10, func() {
			count := 0
			report, err := Files(context.Background(), request, agentmeta.Claude, roots, syntheticInspector{}, deduplicate, func(agentapi.DiscoveryCandidate) error { count++; return nil })
			if err != nil || report.Enumerated != len(entries) || count != len(entries) {
				t.Fatalf("candidate count=%d report=%+v err=%v", count, report, err)
			}
		})
	}
	disabled, enabled := measure(false), measure(true)
	t.Logf("disabled=%g enabled=%g allocations with equal candidate work", disabled, enabled)
	if disabled >= enabled {
		t.Fatalf("disabled deduplication still builds identity map: %g >= %g", disabled, enabled)
	}
}

func TestWalkPreservesOrderForSortedAndUnsortedHosts(t *testing.T) {
	for _, names := range [][]string{{"a.jsonl", "b.jsonl", "c.jsonl"}, {"c.jsonl", "a.jsonl", "b.jsonl"}} {
		entries := make([]fs.DirEntry, len(names))
		for index, name := range names {
			entries[index] = regularEntry(name)
		}
		var got []string
		coverage, err := Walk(context.Background(), syntheticDiscoveryFiles{entries: entries}, StoreRoot{Path: "synthetic", Suffix: ".jsonl"}, 2, func(reference Ref) (bool, error) { got = append(got, reference.Path); return true, nil })
		if err != nil || coverage.Complete || !reflect.DeepEqual(got, []string{"synthetic/a.jsonl", "synthetic/b.jsonl"}) {
			t.Fatalf("names=%v got=%v coverage=%+v err=%v", names, got, coverage, err)
		}
	}
}
