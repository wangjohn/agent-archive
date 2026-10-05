package gitremote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"os"
	"path/filepath"
	"strings"
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
		for path := range paths {
			stamp, ok := repositoryStamp(path)
			if !ok {
				return sourcefacts.RepositoryIdentity{}
			}
			id.Dependencies = append(id.Dependencies, sourcefacts.RepositoryDependency{Path: path, Stamp: stamp})
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
	raw := fmt.Sprintf("%s:%d:%d:%d:%v", path, info.Size(), info.Mode(), info.ModTime().UnixNano(), info.Sys())
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:]), true
}
