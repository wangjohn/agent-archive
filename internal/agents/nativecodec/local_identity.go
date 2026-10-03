package nativecodec

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"path/filepath"
	"strings"
)

// LocalIdentity selects Claude's retained identity, preferring a resumed file's ID.
func (ClaudeAdapter) LocalIdentity(t archive.FilteredTranscript, filename string) archive.NativeSessionIdentity {
	return localIdentity(t, filename)
}

// LocalIdentity selects Cursor's structured identity; text admission stays with discovery.
func (CursorAdapter) LocalIdentity(t archive.FilteredTranscript, filename string) archive.NativeSessionIdentity {
	return localIdentity(t, filename)
}

// LocalIdentity owns Codex session-meta precedence and rollout filename conventions.
func (CodexAdapter) LocalIdentity(t archive.FilteredTranscript, filename string) archive.NativeSessionIdentity {
	identity := localIdentity(t, filename)
	if identity.ID != "" {
		return identity
	}
	stem := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	const uuidLength = 36
	if strings.HasPrefix(stem, "rollout-") && len(stem) > len("rollout-")+uuidLength {
		identity.ID = stem[len(stem)-uuidLength:]
	}
	return identity
}

func localIdentity(t archive.FilteredTranscript, filename string) archive.NativeSessionIdentity {
	if t.LocalIdentity.ID != "" {
		return archive.NativeSessionIdentity{ID: t.LocalIdentity.ID}
	}
	stem := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	for _, id := range t.LocalIdentity.Candidates {
		if id == stem {
			return archive.NativeSessionIdentity{ID: id}
		}
	}
	if len(t.LocalIdentity.Candidates) > 0 {
		return archive.NativeSessionIdentity{ID: t.LocalIdentity.Candidates[0]}
	}
	return archive.NativeSessionIdentity{}
}
