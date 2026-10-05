package gitremote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ProjectIdentity supplies bounded, local checkout evidence for configured-root recovery.
// Existing non-Git scratch roots are known absent; unreadable/missing roots stay unknown.
func ProjectIdentity(ctx context.Context, root string) sourcefacts.RepositoryIdentity {
	top := ProjectRoot(ctx, root, nil)
	if top != "" {
		key, known := ProjectKey(ctx, root, nil)
		id := sourcefacts.RepositoryIdentity{Root: top, Key: key, Known: known}
		if !known {
			return id
		}
		bounded, cancel := context.WithTimeout(ctx, Timeout)
		defer cancel()
		raw, err := ExecRunner(bounded, root, "-C", root, "config", "--show-origin", "--name-only", "-z", "--list")
		if err != nil || bounded.Err() != nil {
			return sourcefacts.RepositoryIdentity{}
		}
		parts := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if len(parts)%2 != 0 {
			return sourcefacts.RepositoryIdentity{}
		}
		paths := map[string]bool{root: true, filepath.Join(root, ".git"): true}
		for i := 0; i < len(parts); i += 2 {
			path, ok := strings.CutPrefix(parts[i], "file:")
			if !ok {
				return sourcefacts.RepositoryIdentity{}
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(root, path)
			}
			paths[filepath.Clean(path)] = true
		}
		if len(paths) > 128 {
			return sourcefacts.RepositoryIdentity{}
		}
		ordered := make([]string, 0, len(paths))
		for path := range paths {
			ordered = append(ordered, path)
		}
		sort.Strings(ordered)
		for _, path := range ordered {
			stamp, ok := repositoryStamp(path)
			if !ok {
				return sourcefacts.RepositoryIdentity{}
			}
			id.Dependencies = append(id.Dependencies, sourcefacts.RepositoryDependency{Path: path, Stamp: stamp})
		}
		// The final reads must agree with the earlier identity under unchanged metadata.
		verifiedRoot := ProjectRoot(ctx, root, nil)
		verifiedKey, verifiedKnown := ProjectKey(ctx, root, nil)
		if !verifiedKnown || verifiedRoot != top || verifiedKey != key || !ProjectIdentityCurrent(id) {
			return sourcefacts.RepositoryIdentity{}
		}
		return id
	}
	if ctx.Err() != nil {
		return sourcefacts.RepositoryIdentity{}
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return sourcefacts.RepositoryIdentity{}
	}
	// Bare or damaged repository metadata cannot be discarded as scratch.
	if _, headErr := os.Lstat(filepath.Join(root, "HEAD")); headErr == nil {
		if _, objectsErr := os.Lstat(filepath.Join(root, "objects")); objectsErr == nil {
			return sourcefacts.RepositoryIdentity{}
		}
	}
	for p, depth := filepath.Clean(root), 0; depth < 64; depth++ {
		_, err := os.Lstat(filepath.Join(p, ".git"))
		if err == nil || !os.IsNotExist(err) {
			return sourcefacts.RepositoryIdentity{}
		}
		parent := filepath.Dir(p)
		if parent == p {
			stamp, ok := repositoryStamp(root)
			return sourcefacts.RepositoryIdentity{Known: ok, Dependencies: []sourcefacts.RepositoryDependency{{Path: root, Stamp: stamp}}}
		}
		p = parent
	}
	return sourcefacts.RepositoryIdentity{}
}

// ProjectIdentityCurrent validates cached Git metadata without spawning Git.
func ProjectIdentityCurrent(id sourcefacts.RepositoryIdentity) bool {
	if !id.Known || len(id.Dependencies) == 0 {
		return false
	}
	for _, dep := range id.Dependencies {
		stamp, ok := repositoryStamp(dep.Path)
		if !ok || stamp != dep.Stamp {
			return false
		}
	}
	return true
}

func repositoryStamp(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	// Same inode, size, mode and modification time identify the observed metadata.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	raw := fmt.Sprintf("%s:%d:%d:%d:%d:%d", path, info.Size(), info.Mode(), info.ModTime().UnixNano(), stat.Dev, stat.Ino)
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:]), true
}
