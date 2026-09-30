package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// repoKeyShape is what RepoKey returns when it is not empty.
var repoKeyShape = regexp.MustCompile(`^repo-[0-9a-f]{16}$`)

// RepoKey derives a stable identifier for "the same repository" from a git
// remote URL, independent of where it is checked out or how it is cloned:
// "repo-" plus the first 16 hex digits of the SHA-256 of
// NormalizeRemoteURL(remote). It is empty when remote names no repository
// that another machine could also reach (no remote, a local path, garbage).
//
// Only this hash is ever stored or uploaded, never the URL. It is guessable
// for a repository whose URL is known, which is why docs/security/privacy.md
// says so.
func RepoKey(remote string) string {
	normalized := NormalizeRemoteURL(remote)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return "repo-" + hex.EncodeToString(sum[:])[:16]
}

// remoteSchemes are the URL schemes a shared git remote uses. Anything else
// (file, ftp, rsync) is not a repository another machine can reach.
var remoteSchemes = map[string]bool{"http": true, "https": true, "ssh": true, "git": true, "git+ssh": true, "ssh+git": true}

// IsRepoKey reports whether s has the shape RepoKey returns for a repository.
func IsRepoKey(s string) bool { return repoKeyShape.MatchString(s) }

// NormalizeRemoteURL reduces a git remote URL to "host/owner/repo", so an SSH
// clone (git@host:owner/repo.git, ssh://git@host/owner/repo.git) and an HTTPS
// clone of one repository normalize to the same string. Userinfo
// (user:token@) is split off with the host and never reaches the result, so
// a credential does not enter the hash input or vary it. The scheme, the port
// (an SSH port is a transport detail, not part of the repository's
// identity), any query or fragment, a trailing ".git" in any case (and
// repeated), trailing slashes, and a trailing dot on the host are dropped,
// and the host is lower-cased. The path keeps its case, so a host that treats
// Acme/Widget and acme/widget as one repository gives them two keys: a
// missed match, never a wrong one. Remotes that are url.insteadOf shorthands
// in git's configuration are read as written, not expanded.
//
// It returns "" for an empty or malformed URL, a local path or file:// URL
// (not portable between machines), and any scheme that is not http, https,
// ssh, or git. It never panics.
func NormalizeRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsFunc(raw, isControl) {
		return ""
	}
	var host, repoPath string
	if strings.Contains(raw, "://") {
		host, repoPath = splitURLRemote(raw)
	} else {
		host, repoPath = splitSCPRemote(raw)
	}
	// A fully qualified host (github.com.) is the same host.
	host = strings.TrimRight(strings.ToLower(host), ".")
	// Percent-escapes were decoded by now (%0A, %00): no control character
	// may survive into the hash input.
	if !plausibleHost(host) || strings.ContainsFunc(repoPath, isControl) {
		return ""
	}
	// Cleaning a rooted path removes "." and ".." elements, so what is left
	// is either empty or a plain owner/repo path.
	repoPath = strings.Trim(path.Clean("/"+repoPath), "/")
	for {
		trimmed := strings.Trim(repoPath, "/")
		if len(trimmed) >= len(".git") && strings.EqualFold(trimmed[len(trimmed)-len(".git"):], ".git") {
			trimmed = trimmed[:len(trimmed)-len(".git")]
		}
		if trimmed == repoPath {
			break
		}
		repoPath = trimmed
	}
	if repoPath == "" {
		return ""
	}
	return host + "/" + repoPath
}

func isControl(r rune) bool { return r <= ' ' || r == 0x7f }

// splitURLRemote splits a scheme://... remote into host (no userinfo, no
// port) and path.
func splitURLRemote(raw string) (host, repoPath string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	if !remoteSchemes[strings.ToLower(u.Scheme)] {
		return "", ""
	}
	return u.Hostname(), u.Path
}

// splitSCPRemote splits scp-style git@host:owner/repo.git. Anything without
// a host before the first colon is a local path.
func splitSCPRemote(raw string) (host, repoPath string) {
	hostPart, rest, found := strings.Cut(raw, ":")
	if !found || strings.Contains(hostPart, "/") {
		return "", ""
	}
	if at := strings.LastIndex(hostPart, "@"); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	// A single letter is a Windows drive (C:\repo), not a host.
	if len(hostPart) < 2 {
		return "", ""
	}
	return hostPart, rest
}

// plausibleHost accepts DNS names and IPv4 addresses, the hosts a shared git
// remote uses, and rejects anything with URL syntax left in it.
func plausibleHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
