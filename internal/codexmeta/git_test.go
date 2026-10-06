package codexmeta

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOptionalGitEvidenceDoesNotInvalidateIdentity(t *testing.T) {
	for _, value := range []string{`null`, `"wrong"`, `{"repository_url":42}`, `{"repository_url":"` + strings.Repeat("x", 4097) + `"}`} {
		var m CodexMeta
		if err := json.Unmarshal([]byte(`{"id":"00000000-0000-0000-0000-000000000001","git":`+value+`}`), &m); err != nil || m.Git.RepositoryURL != "" || m.ID == "" {
			t.Fatalf("%+v %v", m, err)
		}
	}
	var m CodexMeta
	err := json.Unmarshal([]byte(`{"git":{"repository_url":"git@example.test:acme/repo.git"}}`), &m)
	if err != nil || m.Git.RepositoryURL != "git@example.test:acme/repo.git" {
		t.Fatalf("%+v %v", m, err)
	}
}
