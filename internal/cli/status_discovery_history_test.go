package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestStatusReportsUnsupportedHistoryAlongsideSupportedDiscovery(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	const nativeID = "00000000-0000-0000-0000-000000000001"
	const sourcePath = "/private/SYNTHETIC_SOURCE/rollout-" + nativeID + ".jsonl"
	const metadata = `{"type":"session_meta","payload":{"id":"` + nativeID + `","timestamp":"2026-10-03T12:00:00Z","cwd":"/private/SYNTHETIC_PROJECT","source":"cli","originator":"codex-tui","cli_version":"0.159.3","history_mode":"HISTORY"}}`
	const task = `{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + nativeID + `","root_turn_id":"` + nativeID + `","started_at":"2026-10-03T12:00:00Z"}}`
	health := discovery.Health{Enabled: true, LastAttempt: at, Outcomes: map[string]int{}}
	for _, mode := range []string{"paginated", "paginated", "paginated", "compressed", "referenced"} {
		header := sourcefacts.ReadCodexHeader(strings.NewReader(strings.Replace(metadata, "HISTORY", mode, 1)+"\n"+task+"\n"), sourcePath)
		health.Outcomes[header.Outcome]++
		if mode == "paginated" {
			if header.Outcome != "native_format" || !sourcefacts.SupportedCodexProducer(header.Meta) {
				t.Fatalf("supported source did not establish mixed observations: %+v", header)
			}
			health.Supported = true
		} else if header.Outcome != "unsupported_history" {
			t.Fatalf("unsupported history classification changed: %q", header.Outcome)
		}
	}
	app := appStatus{Name: "codex", Hooks: "installed", CodexCaptureScope: "all-projects", Discovery: &health, State: "waiting for first session"}
	view := statusView{configured: true, State: "Waiting for capture", Background: "loaded", Apps: []appStatus{app}}
	for _, verbose := range []bool{false, true} {
		t.Run(map[bool]string{false: "basic", true: "verbose"}[verbose], func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			printStatus(&out, view, statusScreen{style: styleFor(&out), now: at, verbose: verbose})
			text := out.String()
			for _, expected := range []string{"discovery: supported observations", "skipped 2", "unsupported history: 2", "Update agent-archive for format support"} {
				if !strings.Contains(text, expected) {
					t.Errorf("missing %q in rendered status:\n%s", expected, text)
				}
			}
			if verbose && !strings.Contains(text, "Unsupported/incomplete/malformed observations: 2") {
				t.Errorf("verbose details lost rejected-history count:\n%s", text)
			}
			for _, private := range []string{nativeID, "SYNTHETIC_SOURCE", "SYNTHETIC_PROJECT", "native format: 3", "skipped 5"} {
				if strings.Contains(text, private) {
					t.Errorf("rendered status exposed private source facts or counted supported observations: %q", private)
				}
			}
		})
	}
}
