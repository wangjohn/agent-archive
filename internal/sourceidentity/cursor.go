// Package sourceidentity holds pure compatibility values for retained source signatures.
package sourceidentity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// CursorSignature preserves the historical Cursor scan equality fields.
type CursorSignature struct {
	// LastUpdatedAt is composerData.lastUpdatedAt in Unix milliseconds, zero
	// when absent.
	LastUpdatedAt int64
	// HeaderCount is how many messages the chat lists.
	HeaderCount int
	// LastBubbleID is the last listed message's ID.
	LastBubbleID string
	// MessageRows is how many listed messages have a row (a key, whatever
	// its value), so a row that arrives after its header changes the
	// signature. Counting keys reads only the key index.
	MessageRows int
	// LastMessageHash is a hash of the last listed message's row, "" when it
	// has none, so an edit to the message being written changes it.
	LastMessageHash string
}

// SourceSignature encodes exact native equality as a fixed provider token.
func (s CursorSignature) SourceSignature() agentapi.SourceSignature {
	h := sha256.New()
	_, _ = h.Write([]byte("agent-archive/cursor-signature/v1"))
	var b [8]byte
	number := func(n uint64) { binary.BigEndian.PutUint64(b[:], n); _, _ = h.Write(b[:]) }
	text := func(v string) { number(uint64(len(v))); _, _ = h.Write([]byte(v)) }
	number(uint64(s.LastUpdatedAt)) //nolint:gosec // Signed timestamp bits are the existing equality contract.
	number(uint64(s.HeaderCount))   //nolint:gosec // Counts are decoded from nonnegative slice lengths.
	text(s.LastBubbleID)
	number(uint64(s.MessageRows)) //nolint:gosec // Counts are decoded from nonnegative indexed row counts.
	text(s.LastMessageHash)
	return agentapi.SourceSignature{Version: 1, Provider: "cursor/sqlite", Token: hex.EncodeToString(h.Sum(nil))}
}
