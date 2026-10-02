package jsonedit

import (
	"bytes"
	"errors"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"testing"
)

func TestSelectedMemberEditPreservesUnrelatedBytes(t *testing.T) {
	t.Parallel()
	original := []byte("{\r\n  \"untouched\" : [9007199254740993, \"<>&\"],\r\n  \"owned\": true\r\n}\r\n")
	snapshot := bytes.Clone(original)
	doc, err := Parse(original, "owned")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("owned", &Object{Members: []Member{{Key: "first", Value: "<&>"}, {Key: "second", Value: false}}}); err != nil {
		t.Fatal(err)
	}
	result := doc.Bytes()
	if !bytes.Contains(result, []byte(`"untouched" : [9007199254740993, "<>&"]`)) || !bytes.Equal(original, snapshot) || bytes.Contains(result, []byte(`\u003c`)) {
		t.Fatalf("changed unrelated bytes or input: %q", result)
	}
}

func TestOnlySelectedDuplicateMembersAreRefused(t *testing.T) {
	t.Parallel()
	original := []byte(`{"unknown":1,"unknown":2,"owned":0}`)
	if _, err := Parse(original, "owned"); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(original, "unknown"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("duplicate selected member: %v", err)
	}
}
