package nativecodec

import (
	"bytes"
	"encoding/json"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNormalizeRemoteURL(t *testing.T) {
	t.Parallel()
	const want = "github.com/acme/widget"
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"https", "https://github.com/acme/widget", want},
		{"https with .git", "https://github.com/acme/widget.git", want},
		{"https with credentials", "https://user:token@github.com/acme/widget.git", want},
		{"https with token only", "https://token@github.com/acme/widget", want},
		{"https default port", "https://github.com:443/acme/widget", want},
		{"http", "http://github.com/acme/widget", want},
		{"scp-style", "git@github.com:acme/widget.git", want},
		{"scp-style without user", "github.com:acme/widget", want},
		{"ssh scheme", "ssh://git@github.com/acme/widget.git", want},
		{"ssh scheme with port", "ssh://git@github.com:22/acme/widget.git", want},
		{"ssh scheme with other port", "ssh://git@github.com:2222/acme/widget.git", want},
		{"git scheme", "git://github.com/acme/widget.git", want},
		{"mixed-case host", "https://GitHub.COM/acme/widget", want},
		{"trailing slash", "https://github.com/acme/widget/", want},
		{"trailing .git slash", "https://github.com/acme/widget.git/", want},
		{"query and fragment", "https://github.com/acme/widget.git?x=1#frag", want},
		{"padded", "  https://github.com/acme/widget \n", want},
		{"upper-case .GIT", "https://github.com/acme/widget.GIT", want},
		{"repeated .git", "https://github.com/acme/widget.git.git", want},
		{".git before a slash", "https://github.com/acme/widget.git//", want},
		{"fully qualified host", "https://github.com./acme/widget", want},
		{"fully qualified scp host", "git@github.com.:acme/widget.git", want},
		{"credentials after the colon of an scp-style remote", "user:token@github.com:acme/widget.git", ""},
		{"credentials in a path", "git@github.com:acme/token@widget.git", ""},
		{"escaped newline in the path", "https://github.com/acme/wid%0Aget", ""},
		{"escaped NUL in the path", "https://github.com/acme/widget%00", ""},
		{"escaped space in the path", "https://github.com/acme/wid%20get", ""},
		{"escaped control in the host", "https://github.com%0A/acme/widget", ""},
		{"path case is kept", "https://github.com/Acme/Widget", "github.com/Acme/Widget"},
		{"nested group", "git@gitlab.example.com:group/sub/widget.git", "gitlab.example.com/group/sub/widget"},
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"garbage", "not a url at all", ""},
		{"no path", "https://github.com", ""},
		{"only .git", "https://github.com/.git", ""},
		{"absolute path", "/srv/git/widget.git", ""},
		{"relative path", "../widget", ""},
		{"home path", "~/src/widget", ""},
		{"file url", "file:///srv/git/widget.git", ""},
		{"windows path", `C:\src\widget`, ""},
		{"windows drive with slash", "C:/src/widget", ""},
		{"unsupported scheme", "ftp://example.com/acme/widget", ""},
		{"no host", "https:///acme/widget", ""},
		{"bad port", "https://github.com:port/acme/widget", ""},
		{"dot dot path", "https://github.com/../..", ""},
		{"scheme only", "https://", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeRemoteURL(tc.in); got != tc.want {
				t.Errorf("NormalizeRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRepoKeyIsTheSameForSSHAndHTTPSClones(t *testing.T) {
	t.Parallel()
	key := RepoKey("https://github.com/acme/widget.git")
	if !regexp.MustCompile(`^repo-[0-9a-f]{16}$`).MatchString(key) {
		t.Fatalf("RepoKey = %q, want repo- and 16 hex digits", key)
	}
	for _, remote := range []string{
		"git@github.com:acme/widget.git",
		"ssh://git@github.com/acme/widget",
		"https://GITHUB.com/acme/widget/",
	} {
		if got := RepoKey(remote); got != key {
			t.Errorf("RepoKey(%q) = %q, want %q", remote, got, key)
		}
	}
	if other := RepoKey("https://github.com/acme/gadget.git"); other == key {
		t.Errorf("different repositories share key %q", key)
	}
}

func TestRepoKeyIgnoresCredentials(t *testing.T) {
	t.Parallel()
	plain := RepoKey("https://github.com/acme/widget.git")
	for _, remote := range []string{
		"https://user:token@github.com/acme/widget.git",
		"https://other:different-token@github.com/acme/widget.git",
		"https://x-access-token:synthetic@github.com/acme/widget.git",
	} {
		if got := RepoKey(remote); got != plain {
			t.Errorf("RepoKey(%q) = %q, want the credential-free %q", remote, got, plain)
		}
	}
}

func TestRepoKeyIsEmptyWithoutAPortableRemote(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{"", "  ", "/srv/git/widget", "file:///srv/widget", "garbage"} {
		if got := RepoKey(remote); got != "" {
			t.Errorf("RepoKey(%q) = %q, want empty", remote, got)
		}
	}
}

func TestApplyRepoKeyKeepsOnlyAKey(t *testing.T) {
	t.Parallel()
	key := RepoKey("https://github.com/acme/widget")
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"a key", key, key},
		{"empty", "", ""},
		{"a url", "https://user:token@github.com/acme/widget.git", ""},
		{"a hash of the wrong length", "repo-abc", ""},
		{"upper-case hex", "repo-ABCDEF0123456789", ""},
	} {
		var m Metadata
		m.ApplyRepoKey(tc.in)
		if m.RepoKey != tc.want {
			t.Errorf("%s: ApplyRepoKey(%q) set %q, want %q", tc.name, tc.in, m.RepoKey, tc.want)
		}
	}
}

func TestMetadataSchemaAcceptsARepoKeyAndNothingElseInItsPlace(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "metadata.schema.json")
	var bundle SourceBundle
	for _, b := range fixtureBundles(t) {
		bundle = b
		break
	}
	base := func(repoKey string) []byte {
		derived := time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)
		metadata, err := BuildMetadata(bundle, "machine-1", derived, derived,
			SourceReference{Key: "sessions/x/y/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 10}, ParserInfo{})
		if err != nil && !IsParseError(err) {
			t.Fatal(err)
		}
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		doc["repo_key"] = repoKey
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	validateAgainst(t, schema, "metadata with a repo_key", base(RepoKey("https://example.test/acme/widget")))
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(base("https://example.test/acme/widget.git")))
	if err != nil {
		t.Fatal(err)
	}
	if schema.Validate(instance) == nil {
		t.Error("the schema accepts a URL as repo_key")
	}
}
