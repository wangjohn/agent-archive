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
		Unavailable string `json:"unavailable"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if doc.Version != listSchemaVersion || len(doc.Sessions) != 1 || doc.Sessions[0].SessionID != id || doc.Sessions[0].Harness.Name == "" || doc.Unavailable != "" {
		t.Fatalf("doc = %+v", doc)
	}

	out.Reset()
	if code := Run([]string{"list", "--json", "--harness", "cursor"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"sessions": []`) {
		t.Fatalf("empty result is not an empty array:\n%s", out.String())
	}

	out.Reset()
	if code := Run([]string{"list", "--json", "--skill", "x", "--skill-usage", "eligible_no_use"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	doc.Unavailable = ""
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Unavailable == "" || len(doc.Sessions) != 0 {
		t.Fatalf("eligible_no_use JSON: %v\n%s", err, out.String())
	}
}

// show always prints JSON; it accepts --json like list and status.
func TestShowAcceptsJSONFlag(t *testing.T) {
	env, _, id := publishedFixture(t)
	var plain, flagged, errOut bytes.Buffer
	if code := Run([]string{"show", id}, nil, &plain, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if code := Run([]string{"show", id, "--json"}, nil, &flagged, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if plain.String() != flagged.String() || !json.Valid(flagged.Bytes()) {
		t.Fatalf("show --json differs from show:\n%s\n---\n%s", plain.String(), flagged.String())
	}
}
