package sourceio

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// FileSignature preserves exact size/nanosecond equality including signed time bits.
func FileSignature(size, mtime int64) agentapi.SourceSignature {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(size))  //nolint:gosec // Verified file sizes are nonnegative.
	binary.BigEndian.PutUint64(b[8:], uint64(mtime)) //nolint:gosec // Preserve signed nanosecond timestamp bits.
	return agentapi.SourceSignature{Version: 1, Provider: "file", Token: base64.RawURLEncoding.EncodeToString(b[:])}
}

// CursorSignature bounds the existing exact native tuple without truncating IDs.
func CursorSignature(s cursorstore.Signature) agentapi.SourceSignature { return s.SourceSignature() }

// ValidateSignature enforces the persisted envelope, not native identity lengths.
func ValidateSignature(s agentapi.SourceSignature) error {
	if s.Version != 1 || len(s.Provider) == 0 || len(s.Provider) > 64 {
		return errors.New("invalid source signature")
	}
	for _, c := range s.Provider {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("-/_", c)) {
			return errors.New("invalid source signature provider")
		}
	}
	if len(s.Token) == 0 || len(s.Token) > 64 {
		return errors.New("invalid source signature token")
	}
	for _, c := range s.Token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return errors.New("invalid source signature token")
		}
	}
	// Validated ASCII fields need no JSON escaping; fixed envelope overhead plus 128 bytes is below 256.
	return nil
}
