package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRealCLIOverflowShowKeepsFullBodyAndSourceAuthority(t *testing.T) {
	env, mem, id := publishedFixture(t)
	key := "sessions/codex/" + id + "/metadata.json"
	raw, err := mem.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Title = "Unique overflow authority"
	metadata.PullRequests = []archive.PullRequestLink{{Number: 88123}}
	for i := range 2000 {
		metadata.LinkedSessions = append(metadata.LinkedSessions, archive.LinkedSessionReference{SessionID: fmt.Sprintf("linked-%04d", i), Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: metadata.CapturedAt})
	}
	raw, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = mem.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	remote := privateCatalogFromLegacy(t, mem)
	counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
	measured, err := catalog.Wrap(counts)
	if err != nil {
		t.Fatal(err)
	}
	oracle := env
	oracle.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
	for _, flags := range [][]string{{"--json"}, {"--transcript", "--no-pager"}} {
		var want, wantErr bytes.Buffer
		if code := Run(append([]string{"show", id}, flags...), nil, &want, &wantErr, oracle); code != 0 {
			t.Fatal(code, wantErr.String())
		}
		for _, query := range []string{"overflow", id[:8], "#88123"} {
			counts.Reset()
			counts.paths = map[string]int{}
			var got, errs bytes.Buffer
			if code := Run(append([]string{"show", query}, flags...), nil, &got, &errs, env); code != 0 || got.String() != want.String() || errs.String() != wantErr.String() {
				t.Fatalf("full overflow show %s %v code=%d error=%s", query, flags, code, errs.String())
			}
			if counts.paths["body"] != 1 || counts.Metrics().Lists != 0 {
				t.Fatal("selected overflow body repeated", counts.paths, counts.Metrics())
			}
		}
	}
	counts.Reset()
	counts.paths = map[string]int{}
	var got, errs bytes.Buffer
	if code := Run([]string{"show", "overflow", "--json", "--no-cache"}, nil, &got, &errs, env); code != 0 {
		t.Fatal(code, errs.String())
	}
	var want, wantErr bytes.Buffer
	if code := Run([]string{"show", id, "--json"}, nil, &want, &wantErr, oracle); code != 0 || got.String() != want.String() {
		t.Fatal("uncached overflow authority differs", code, wantErr.String())
	}
	if counts.paths["body"] != 2 || counts.Metrics().Lists != 0 {
		t.Fatal("uncached complete-summary cost changed", counts.paths, counts.Metrics())
	}
}
