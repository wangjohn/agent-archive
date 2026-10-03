package archive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	startSHA = strings.Repeat("3f", 20)
	lastSHA  = strings.Repeat("9e", 20)
)

func TestApplyGitHeadCopiesWhatTheHooksRecorded(t *testing.T) {
	t.Parallel()
	dirty := true
	pacific := time.FixedZone("PDT", -7*3600)
	reg := SessionRegistration{
		StartHead: &GitHead{SHA: startSHA, Dirty: &dirty, ObservedAt: time.Date(2026, 9, 30, 9, 0, 0, 0, pacific)},
		// A dirty flag on the last HEAD is never published: stop hooks do
		// not ask for one.
		LastHead: &GitHead{SHA: lastSHA, Dirty: &dirty, ObservedAt: time.Date(2026, 9, 30, 10, 0, 0, 0, pacific)},
	}
	var m Metadata
	m.ApplyGitHead(reg)
	encoded, err := json.Marshal(m.GitHead)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"start":{"sha":"` + startSHA + `","dirty":true,"observed_at":"2026-09-30T16:00:00Z"},"last":{"sha":"` + lastSHA + `","observed_at":"2026-09-30T17:00:00Z"}}`
	if string(encoded) != want {
		t.Errorf("git_head = %s\nwant %s", encoded, want)
	}
	// The metadata holds copies: changing the registration afterwards does
	// not change what was published.
	*reg.StartHead.Dirty = false
	if !*m.GitHead.Start.Dirty {
		t.Error("git_head.start.dirty shares the registration's flag")
	}
}

func TestApplyGitHeadLeavesOutWhatIsNotACommit(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	for name, head := range map[string]*GitHead{
		"nothing":               nil,
		"an abbreviation":       {SHA: "3f9c2ab", ObservedAt: at},
		"upper case":            {SHA: strings.ToUpper(startSHA), ObservedAt: at},
		"a branch":              {SHA: "main", ObservedAt: at},
		"a ref with a commit":   {SHA: "refs/heads/" + startSHA, ObservedAt: at},
		"a name with a newline": {SHA: startSHA + "\n", ObservedAt: at},
		"no time":               {SHA: startSHA},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var m Metadata
			m.ApplyGitHead(SessionRegistration{StartHead: head, LastHead: head})
			if m.GitHead != nil {
				t.Errorf("git_head = %+v, want none", m.GitHead)
			}
		})
	}
	var m Metadata
	m.ApplyGitHead(SessionRegistration{LastHead: &GitHead{SHA: lastSHA, ObservedAt: at}})
	if m.GitHead == nil || m.GitHead.Start != nil || m.GitHead.Last == nil {
		t.Errorf("git_head = %+v, want only last", m.GitHead)
	}
	sha256Name := strings.Repeat("ab", 32)
	m.ApplyGitHead(SessionRegistration{StartHead: &GitHead{SHA: sha256Name, ObservedAt: at}})
	if m.GitHead == nil || m.GitHead.Start == nil || m.GitHead.Start.SHA != sha256Name || m.GitHead.Last != nil {
		t.Errorf("git_head = %+v, want a SHA-256 start alone", m.GitHead)
	}
}

// The schema accepts only full object names, so a branch, a path, or an
// abbreviation in git_head fails validation even if the code let it through.
func TestMetadataSchemaAcceptsOnlyFullCommitNamesInGitHead(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "metadata.schema.json")
	gitHead := func(head string) []byte {
		return []byte(`{"schema_version":1,"session_id":"s","native_session_id":"n","machine_id":"m","project_id":"p",` +
			`"started_at":"2026-09-30T09:00:00Z","captured_at":"2026-09-30T09:00:00Z","metadata_derived_at":"2026-09-30T09:00:00Z",` +
			`"harness":{"name":"codex"},"adapter":{"name":"codex","version":"1"},"parser":{"name":"codex","version":"1","status":"partial"},` +
			`"filter_version":"12","state":"idle","skill_detection":"unavailable","counts":{},` +
			`"source_bundle":{"key":"k","sha256":"` + strings.Repeat("a", 64) + `","compressed_bytes":1},"git_head":` + head + `}`)
	}
	valid := func(data []byte) bool {
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return schema.Validate(instance) == nil
	}
	for _, head := range []string{
		`{"start":{"sha":"` + startSHA + `","dirty":false,"observed_at":"2026-09-30T09:00:00Z"}}`,
		`{"last":{"sha":"` + strings.Repeat("ab", 32) + `","observed_at":"2026-09-30T09:00:00Z"}}`,
	} {
		if !valid(gitHead(head)) {
			t.Errorf("rejected %s", head)
		}
	}
	for _, head := range []string{
		`{}`,
		`{"start":{"sha":"3f9c2ab","observed_at":"2026-09-30T09:00:00Z"}}`,
		`{"start":{"sha":"main","observed_at":"2026-09-30T09:00:00Z"}}`,
		`{"start":{"sha":"` + startSHA + `"}}`,
		`{"last":{"sha":"` + startSHA + `","dirty":true,"observed_at":"2026-09-30T09:00:00Z"}}`,
		`{"start":{"sha":"` + startSHA + `","observed_at":"2026-09-30T09:00:00Z","branch":"main"}}`,
		`{"start":{"sha":"` + startSHA + `","observed_at":"2026-09-30T09:00:00Z"},"remote":"https://example.test/acme/widget.git"}`,
	} {
		if valid(gitHead(head)) {
			t.Errorf("accepted %s", head)
		}
	}
}
