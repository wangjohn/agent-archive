// Package doclinks checks the repository's Markdown documentation: every
// relative link resolves to a file in the repository, and every #anchor to a
// heading in it, and dev/maintainers/versions.md matches the code. It holds
// only these tests, which `go test ./...` runs in CI.
package doclinks

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// markdownLink matches an inline link or image target: ](target) or
// ](target "title"). Reference-style definitions are matched by
// referenceLink.
var (
	markdownLink  = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	referenceLink = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]:\s*(\S+)`)
	heading       = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*#*\s*$`)
	// The closing fence starts a line: the newline before it is matched
	// explicitly, which is the same as a mid-pattern ^ in multiline mode.
	fence      = regexp.MustCompile("(?ms)^\\s*(```|~~~).*?\n\\s*(```|~~~)")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
)

// repoRoot is the repository root, two levels above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// markdownFiles lists the repository's .md files. It skips hidden
// directories other than .github (.git, and .claude, whose worktrees hold
// whole copies of the repository), node_modules, and dist.
func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			name := d.Name()
			if slices.Contains([]string{"node_modules", "dist"}, name) || strings.HasPrefix(name, ".") && name != ".github" {
				return filepath.SkipDir
			}
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// slug is GitHub's anchor for a heading: lowercase, punctuation other than
// hyphens and underscores removed, spaces to hyphens.
func slug(title string) string {
	title = inlineCode.ReplaceAllStringFunc(title, func(s string) string { return strings.Trim(s, "`") })
	title = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`).ReplaceAllString(title, "$1")
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// anchors are the heading anchors of a Markdown file, with GitHub's -1, -2
// suffixes for repeated headings.
func anchors(content string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	for _, m := range heading.FindAllStringSubmatch(fence.ReplaceAllString(content, ""), -1) {
		s := slug(m[1])
		if n := seen[s]; n > 0 {
			out[s+"-"+itoa(n)] = true
		} else {
			out[s] = true
		}
		seen[s]++
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

// TestRelativeLinksResolve fails for each relative link in a Markdown file
// whose target file is missing, or whose #anchor names no heading there.
// Links with a scheme (https:, mailto:) are not fetched.
func TestRelativeLinksResolve(t *testing.T) {
	root := repoRoot(t)
	cache := map[string]map[string]bool{}
	anchorsOf := func(path string) (map[string]bool, bool) {
		if a, ok := cache[path]; ok {
			return a, true
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		a := anchors(string(data))
		cache[path] = a
		return a, true
	}
	files := markdownFiles(t, root)
	if len(files) < 5 {
		t.Fatalf("found only %d Markdown files under %s", len(files), root)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := inlineCode.ReplaceAllString(fence.ReplaceAllString(string(data), ""), "")
		var targets []string
		for _, m := range markdownLink.FindAllStringSubmatch(text, -1) {
			targets = append(targets, m[1])
		}
		for _, m := range referenceLink.FindAllStringSubmatch(text, -1) {
			targets = append(targets, m[1])
		}
		rel, _ := filepath.Rel(root, file)
		for _, target := range targets {
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			path, anchor, _ := strings.Cut(target, "#")
			resolved := file
			if path != "" {
				resolved = filepath.Join(filepath.Dir(file), filepath.FromSlash(path))
				if strings.HasPrefix(path, "/") {
					resolved = filepath.Join(root, filepath.FromSlash(path))
				}
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s: link %q: %s does not exist", rel, target, path)
					continue
				}
			}
			if anchor == "" || !strings.HasSuffix(resolved, ".md") {
				continue
			}
			if a, ok := anchorsOf(resolved); ok && !a[anchor] {
				t.Errorf("%s: link %q: no heading #%s", rel, target, anchor)
			}
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Import existing sessions":       "import-existing-sessions",
		"Signing and notarization":       "signing-and-notarization",
		"Phase 2: Cursor database chats": "phase-2-cursor-database-chats",
		"Destination ID (B1b)":           "destination-id-b1b",
		"`status --json` fields":         "status---json-fields",
		"What leaves your Mac":           "what-leaves-your-mac",
		"Source filter version 9":        "source-filter-version-9",
		"[Link](x.md) text":              "link-text",
		"Scope and non-goals":            "scope-and-non-goals",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}
