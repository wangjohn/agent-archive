package retention

import "testing"

func TestChooseSessionAction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		expired        bool
		current        bool
		remoteEvidence bool
		want           sessionAction
	}{
		{"active current session", false, true, false, sweepRemote},
		{"active previous destination", false, false, true, leaveSession},
		{"expired previous destination with remote evidence", true, false, true, forgetPreviousDestination},
		{"expired previous destination without remote evidence", true, false, false, forgetPreviousDestination},
		{"expired never published", true, true, false, forgetNeverPublished},
		{"expired published or pending", true, true, true, sweepRemote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := chooseSessionAction(tt.expired, tt.current, tt.remoteEvidence)
			if got != tt.want {
				t.Fatalf("chooseSessionAction(%t, %t, %t) = %v, want %v", tt.expired, tt.current, tt.remoteEvidence, got, tt.want)
			}
		})
	}
}
