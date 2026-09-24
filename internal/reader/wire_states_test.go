package reader

import "testing"

// TestLinkedStateSpellings pins the linked-session states that inspect
// --json writes. The typed constants are the only place these spellings
// live, so a renamed constant value would otherwise change the output
// without failing any test.
func TestLinkedStateSpellings(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{string(LinkedStateUnavailable), "unavailable"},
		{string(LinkedStateIdentityMismatch), "identity_mismatch"},
		{string(LinkedStatePending), "pending"},
		{string(LinkedStateUnavailableOrExpired), "unavailable_or_expired"},
		{string(LinkedStateLookupFailed), "lookup_failed"},
		{string(LinkedStateMetadataAvailable), "metadata_available"},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("state = %q, want %q", c.got, c.want)
		}
	}
}
