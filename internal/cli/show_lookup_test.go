package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestShowReusesSelectedMetadataForExactShortAndTranscript(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	recorder := &recordingStore{ObjectStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return recorder, nil }
	key := "sessions/codex/" + id + "/metadata.json"
	home, err := env.Home()
	if err != nil {
		t.Fatal(err)
	}
	var expected string
	for _, args := range [][]string{
		{"show", id, "--json", "--harness", "codex"},
		{"show", id, "--json"},
		{"show", id[:8], "--json"},
		{"show", id, "--transcript", "--no-pager"},
		{"show", id[:8], "--transcript", "--no-pager"},
	} {
		if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d error=%s", args, code, errOut.String())
		}
		_, gets := recorder.take()
		parentReads := 0
		for _, got := range gets {
			if got == key {
				parentReads++
			}
		}
		if parentReads != 1 {
			t.Fatalf("%v selected parent reads=%d gets=%v", args, parentReads, gets)
		}
		if strings.Contains(strings.Join(args, " "), "--json") {
			if expected == "" {
				expected = out.String()
			} else if out.String() != expected {
				t.Fatalf("exact/short lookup output changed: %v", args)
			}
		}
	}
}

func TestShowAmbiguousHarnessesWinOverMalformedMetadata(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	recorder := &recordingStore{ObjectStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return recorder, nil }
	if err := mem.Put(context.Background(), "sessions/claude/"+id+"/metadata.json", []byte("malformed")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id, "--json"}, nil, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "exists under more than one harness (claude, codex); pass --harness") || out.Len() != 0 {
		t.Fatalf("ambiguous show: code=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	lists, gets := recorder.take()
	if len(lists) != 0 || len(gets) != len(agentmeta.Names(agentmeta.Builtins())) {
		t.Fatalf("ambiguity reread/listed: %v %v", lists, gets)
	}
}
