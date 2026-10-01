package agentmeta

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestSessionKeyPreservesExactIdentity(t *testing.T) {
	alias, err := NewSessionKey(" Claude-Code ", " ABC \x00/界")
	if err != nil || alias.Agent != Claude || alias.NativeID != " ABC \x00/界" {
		t.Fatalf("%#v %v", alias, err)
	}
	canonical, _ := NewSessionKey("claude", alias.NativeID)
	if alias != canonical {
		t.Fatal("alias differs")
	}
	for _, value := range []string{"ABC", " abc ", " ABC \x00/界 ", " ABC \x00/界\u0301"} {
		other, err := NewSessionKey("claude", value)
		if err != nil || bytes.Equal(alias.Encoding(), other.Encoding()) {
			t.Fatalf("identity normalized: %#v %v", other, err)
		}
	}
	for _, value := range []string{"", " \t\n", string([]byte{0xff})} {
		if _, err := NewSessionKey("claude", value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	key, _ := NewSessionKey("codex", "界")
	expected := append([]byte("agent-archive/session-key/v1"), binary.BigEndian.AppendUint64(nil, 5)...)
	expected = append(expected, []byte("codex")...)
	expected = binary.BigEndian.AppendUint64(expected, 3)
	expected = append(expected, []byte("界")...)
	if !bytes.Equal(key.Encoding(), expected) {
		t.Fatalf("encoding %x != %x", key.Encoding(), expected)
	}
	// The encoding itself remains unambiguous independent of agent validation.
	a := SessionKey{Agent: "a", NativeID: "b:c"}
	b := SessionKey{Agent: "a:b", NativeID: "c"}
	if bytes.Equal(a.Encoding(), b.Encoding()) {
		t.Fatal("delimiter collision")
	}
}
