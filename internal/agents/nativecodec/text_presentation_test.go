package nativecodec

import (
	"strings"
	"testing"
	"time"
)

func TestCursorTextPromptPresentationPreservesHistoricalOutput(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, input, want string }{
		{"query wrapper", "<timestamp>2026-09-23T09:00:00Z</timestamp>\n<user_query>Review the parser.\nKeep the fixtures.</user_query>", "Review the parser.\nKeep the fixtures."},
		{"slash command", "<command-name>/review-pr</command-name>\n<command-args>300</command-args>", "/review-pr 300"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			filtered, err := (CursorAdapter{}).FilterText(strings.NewReader("user: "+test.input+"\nassistant: Done."), textStart)
			if err != nil {
				t.Fatal(err)
			}
			reg := registration()
			reg.Harness = Harness{Name: "cursor"}
			bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, textStart.Add(time.Hour), nil)
			if err != nil {
				t.Fatal(err)
			}
			handoff, err := BuildHandoff(bundle, nil, HandoffOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(handoff.Exchanges) != 1 || handoff.Exchanges[0].Prompt != test.want {
				t.Errorf("handoff exchanges = %#v, want prompt %q", handoff.Exchanges, test.want)
			}
			transcript, err := BuildTranscript(bundle, HandoffOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(transcript.Exchanges) != 1 || transcript.Exchanges[0].Text != test.want {
				t.Errorf("transcript exchanges = %#v, want prompt %q", transcript.Exchanges, test.want)
			}
		})
	}
}
