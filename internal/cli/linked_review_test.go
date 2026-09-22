package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestShowKeepsParentReadableWhenChildIsGone(t *testing.T) {
	env, remote, id := publishedFixture(t)
	parent, err := readSingleMetadata(t, remote)
	if err != nil {
		t.Fatal(err)
	}
	parent.LinkedSessions = []archive.LinkedSessionReference{{SessionID: "missing-child", Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: time.Now().UTC()}}
	key, _ := archive.MetadataObjectKey(parent.Harness.Name, id)
	data, _ := json.Marshal(parent)
	if err := remote.Put(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
	if err := remote.Delete(context.Background(), parent.SourceBundle.Key); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut.String())
	}
	// `"hook_finals"` appears only in the normalized view, which this
	// metadata-only invocation must not print. (It replaces a `"turns":`
	// check, which parser 0.6 also emits inside `counts`.)
	if !strings.Contains(out.String(), `"unavailable_or_expired"`) || strings.Contains(out.String(), `"hook_finals"`) {
		t.Fatalf("bad metadata-only output: %s", out.String())
	}
	var decoded archive.Metadata
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded.SessionID != id {
		t.Fatalf("metadata compatibility lost: %v", err)
	}
}
