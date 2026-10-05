package agentmeta

import (
	"encoding/binary"
	"errors"
	"strings"
	"unicode/utf8"
)

// SessionKey identifies an agent's exact, opaque native session ID.
// Construct keys with NewSessionKey; storage boundaries also validate literals.
type SessionKey struct {
	Agent    ID     `json:"Agent"`
	NativeID string `json:"NativeID"`
}

// NewSessionKey canonicalizes the agent and preserves valid native UTF-8 bytes.
func NewSessionKey(agent, nativeID string) (SessionKey, error) {
	key := SessionKey{Agent: ID(Canonical(Builtins(), agent)), NativeID: nativeID}
	if err := key.Validate(); err != nil {
		return SessionKey{}, err
	}
	return key, nil
}

// Validate rejects noncanonical agents and identities JSON cannot round-trip.
func (k SessionKey) Validate() error {
	if !safe(string(k.Agent)) || Canonical(Builtins(), string(k.Agent)) != string(k.Agent) {
		return errors.New("session agent must be a canonical safe identity")
	}
	if strings.TrimSpace(k.NativeID) == "" {
		return errors.New("native session ID is required")
	}
	if !utf8.ValidString(k.NativeID) {
		return errors.New("native session ID is not valid UTF-8")
	}
	return nil
}

// Encoding returns the versioned length-prefixed bytes hashed by local indexes.
func (k SessionKey) Encoding() []byte {
	out := []byte("agent-archive/session-key/v1")
	for _, value := range []string{string(k.Agent), k.NativeID} {
		out = binary.BigEndian.AppendUint64(out, uint64(len(value)))
		out = append(out, value...)
	}
	return out
}
