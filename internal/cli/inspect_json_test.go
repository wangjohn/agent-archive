package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// `list --json` prints the same sessions as the table, as a versioned
// document of their metadata sidecars, and an empty result is an empty array.
func TestListJSONPrintsVersionedMetadataDocument(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	var doc struct {
		Version  int `json:"schema_version"`
		Sessions []struct {
			SessionID string `json:"session_id"`
			Harness   struct {
				Name string `json:"name"`
			} `json:"harness"`
		} `json:"sessions"`
		Limit        int  `json:"limit"`
		Returned     int  `json:"returned"`
		TotalMatched int  `json:"total_matched"`
		Truncated    bool `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if doc.Version != listSchemaVersion || len(doc.Sessions) != 1 || doc.Sessions[0].SessionID != id || doc.Sessions[0].Harness.Name == "" {
		t.Fatalf("doc = %+v", doc)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["unavailable"]; present {
		t.Fatalf("list document contains removed unavailable field: %s", out.String())
	}
	if doc.Limit != defaultListLimit || doc.Returned != 1 || doc.TotalMatched != 1 || doc.Truncated {
		t.Fatalf("limit fields = limit=%d returned=%d total=%d truncated=%v", doc.Limit, doc.Returned, doc.TotalMatched, doc.Truncated)
	}

	out.Reset()
	if code := Run([]string{"list", "--json", "--harness", "cursor"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"sessions": []`) {
		t.Fatalf("empty result is not an empty array:\n%s", out.String())
	}
}

// Like list, show prints text by default and its metadata sidecar with
// --json.
func TestShowPrintsSummaryUnlessJSON(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var plain, flagged, errOut bytes.Buffer
	if code := Run([]string{"show", id}, nil, &plain, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if code := Run([]string{"show", id, "--json"}, nil, &flagged, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if json.Valid(plain.Bytes()) || !strings.Contains(plain.String(), "ID "+id) || !strings.Contains(plain.String(), "--transcript") {
		t.Fatalf("show did not print a summary:\n%s", plain.String())
	}
	var decoded archiveMetadataSessionID
	if err := json.Unmarshal(flagged.Bytes(), &decoded); err != nil || decoded.SessionID != id {
		t.Fatalf("show --json is not the sidecar (%v):\n%s", err, flagged.String())
	}
}
