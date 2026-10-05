package cursor

import "testing"

// A chat's messages are counted as the collector's reader counts them: from
// its headers when it has them, even an empty list, and from its inline
// conversation only when it has none.
func TestComposerMessagesCountedAsTheReaderCountsThem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value   string
		counted bool
	}{
		{`{"_v":18,"composerId":"a","fullConversationHeadersOnly":[{"bubbleId":"m"}]}`, true},
		{`{"_v":18,"composerId":"a","fullConversationHeadersOnly":[],"conversation":[{"bubbleId":"m"}]}`, false},
		{`{"_v":3,"composerId":"a","conversation":[{"bubbleId":"m"}]}`, true},
	}
	for _, tc := range cases {
		c, ok := decodeComposerData("composerData:a", []byte(tc.value))
		if !ok || c.counted != tc.counted {
			t.Errorf("%s: ok %v counted %v, want %v", tc.value, ok, c.counted, tc.counted)
		}
	}
}
