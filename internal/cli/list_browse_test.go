package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestListDefaultTableIsHumanReadable(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "TITLE") || !strings.Contains(text, "WHEN") || !strings.Contains(text, "PROJECT") || !strings.Contains(text, "ID") {
		t.Fatalf("missing human headers:\n%s", text)
	}
	if strings.Contains(text, "ORIGIN") || strings.Contains(text, "PARSER") || strings.Contains(text, "CAPTURED") {
		t.Fatalf("ops columns should be verbose-only:\n%s", text)
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	if !strings.Contains(text, short) || !strings.Contains(text, "just now") || !strings.Contains(text, "codex") {
		t.Fatalf("missing short id / relative time / harness:\n%s", text)
	}
	if strings.Contains(text, "visible") {
		t.Fatalf("list printed transcript content:\n%s", text)
	}
}

func TestShowAcceptsShortIDPrintedByList(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id[:minShortSessionID]}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("show short ID: code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), id) {
		t.Fatalf("show short ID returned wrong session: %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", id[:minShortSessionID], "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("show short ID with harness: code=%d stderr=%s", code, errOut.String())
	}
}

func TestListVerboseKeepsOpsColumns(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{id, "ORIGIN", "PARSER", "CAPTURED", "2026-01-02T00:00:00Z", "hook"} {
		if !strings.Contains(text, want) {
			t.Fatalf("verbose missing %q:\n%s", want, text)
		}
	}
}

func TestListInteractiveShowAndQuit(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	stdin := strings.NewReader("1\nq\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&out)
	}
	if code := Run([]string{"list"}, stdin, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, errOut.String(), out.String())
	}
	text := out.String()
	if !strings.Contains(text, "#") || !strings.Contains(text, "Enter number") {
		t.Fatalf("expected numbered interactive prompt:\n%s", text)
	}
	var meta archiveMetadataSessionID
	if err := json.Unmarshal([]byte(extractJSONObject(text)), &meta); err != nil {
		t.Fatalf("show json: %v\n%s", err, text)
	}
	if meta.SessionID != id {
		t.Fatalf("showed %q want %q\n%s", meta.SessionID, id, text)
	}
}

func TestShowWithoutIDInteractivePicksOnce(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	stdin := strings.NewReader("1\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&out)
	}
	if code := Run([]string{"show"}, stdin, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, errOut.String(), out.String())
	}
	var meta archiveMetadataSessionID
	if err := json.Unmarshal([]byte(extractJSONObject(out.String())), &meta); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if meta.SessionID != id {
		t.Fatalf("got %q want %q", meta.SessionID, id)
	}
	// One-shot: a second prompt would wait for more input; we only sent one line.
	if strings.Count(out.String(), "Enter number") != 1 {
		t.Fatalf("show should prompt once:\n%s", out.String())
	}
}

func TestShowWithoutIDNonInteractiveStillRequiresID(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"show"}, nil, &out, &errOut, env); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}

type archiveMetadataSessionID struct {
	SessionID string `json:"session_id"`
}

// extractJSONObject returns the first top-level JSON object in text.
func extractJSONObject(text string) string {
	start := strings.Index(text, "{")
	if start < 0 {
		return text
	}
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	return text[start:]
}
