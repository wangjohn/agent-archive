package nativecodec

import "testing"

func TestTextRoleWireSpellings(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{string(textRoleUser), "user"},
		{string(textRoleAssistant), "assistant"},
		{string(textRoleTool), "tool"},
		{string(textRoleSystem), "system"},
		{string(textRoleDeveloper), "developer"},
		{string(textRoleThinking), "thinking"},
		{string(textRoleAnalysis), "analysis"},
	} {
		if c.got != c.want {
			t.Errorf("role = %q, want %q", c.got, c.want)
		}
	}
}
