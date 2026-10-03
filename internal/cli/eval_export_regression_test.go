package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

type evalFailWriter struct {
	short bool
	calls int
}

func (w *evalFailWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}
func TestEvalExportStopsWhenOutputFails(t *testing.T) {
	for _, short := range []bool{false, true} {
		env, _, id := publishedFixture(t)
		w := &evalFailWriter{short: short}
		var stderr bytes.Buffer
		if code := Run([]string{"eval", "export", id, id}, nil, w, &stderr, env); code != 1 || w.calls != 1 {
			t.Fatalf("short %v: code %d, writes %d, stderr %q", short, code, w.calls, stderr.String())
		}
		err := writeEvalRecord(&evalFailWriter{short: short}, map[string]string{"x": "y"})
		want := io.ErrClosedPipe
		if short {
			want = io.ErrShortWrite
		}
		if !errors.Is(err, want) {
			t.Fatalf("error %v, want %v", err, want)
		}
	}
}

func TestEvalExportRejectsMetadataUnderAnotherIdentity(t *testing.T) {
	for _, detail := range []string{"metadata", "full"} {
		env, mem, id := publishedFixture(t)
		key, err := archive.MetadataObjectKey("codex", id)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := mem.Get(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		other := strings.Repeat("f", 32)
		otherKey, err := archive.MetadataObjectKey("codex", other)
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Put(context.Background(), otherKey, raw); err != nil {
			t.Fatal(err)
		}
		records, _, code := evalLines(t, env, "--detail", detail, other)
		if code != 1 || len(records) != 1 || records[0]["record"] != "error" {
			t.Fatalf("%s: code %d, records %v", detail, code, records)
		}
	}
}

func TestEvalExportReadErrorsDoNotEchoPrivateMetadata(t *testing.T) {
	for _, detail := range []string{"metadata", "full"} {
		env, mem, id := publishedFixture(t)
		key, err := archive.MetadataObjectKey("codex", id)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := mem.Get(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		data["started_at"] = "private-synthetic-token"
		raw, err = json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Put(context.Background(), key, raw); err != nil {
			t.Fatal(err)
		}
		records, stderr, code := evalLines(t, env, "--detail", detail, id)
		output, _ := json.Marshal(records)
		if code != 1 || strings.Contains(string(output)+stderr, "private-synthetic-token") {
			t.Fatalf("%s: code %d, stdout %s stderr %s", detail, code, output, stderr)
		}
	}
}

func TestEvalExportHarnessAliasesUseCatalog(t *testing.T) {
	env, _, id := publishedFixture(t)
	opts, code := evalExportOptionsFromArgs([]string{"--harness", "claude-code", id}, io.Discard, env)
	if code != 0 || opts.harness != "claude" {
		t.Fatalf("alias code %d harness %q", code, opts.harness)
	}
}

func TestEvalExportAmbiguousIdentityNeedsHarness(t *testing.T) {
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := archive.MetadataObjectKey("claude", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), otherKey, raw); err != nil {
		t.Fatal(err)
	}
	records, _, code := evalLines(t, env, id)
	if code != 1 || len(records) != 1 || records[0]["error"].(map[string]any)["code"] != "ambiguous" {
		t.Fatalf("code %d records %v", code, records)
	}
	records, _, code = evalLines(t, env, "--harness", "claude", id)
	if code != 1 || len(records) != 1 || records[0]["record"] != "error" {
		t.Fatalf("mismatched harness code %d records %v", code, records)
	}
	records, _, code = evalLines(t, env, "--harness", "codex", id)
	if code != 0 || len(records) != 1 || records[0]["record"] != "session" {
		t.Fatalf("selected harness code %d records %v", code, records)
	}
}

func TestEvalExportOutputEscapesControlsWithoutChangingText(t *testing.T) {
	text := "a\u009b\u202eb\nline"
	var out bytes.Buffer
	if err := writeEvalRecord(&out, map[string]string{"text": text}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 1 || strings.Contains(out.String(), "\u009b") || strings.Contains(out.String(), "\u202e") {
		t.Fatalf("unsafe JSONL %q", out.String())
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["text"] != text {
		t.Fatalf("text %q want %q", got["text"], text)
	}
}
