package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestStatusCompatibilityUsesRecordedVersionsAndDoesNotSkipUntestedFormats(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	h := discovery.Health{Enabled: true, Supported: true, LastAttempt: at, Outcomes: map[string]int{"unsupported_history": 1}, Formats: []discovery.FormatObservation{
		{Profile: sourcefacts.CodexLegacyJSONL, Version: "0.159.3", Source: "cli", Evidence: sourcefacts.CodexRuntimeTested, Observations: 1},
		{Profile: sourcefacts.CodexPaginatedJSONL, Version: "0.155.0-alpha.9.2", Source: "vscode", Evidence: sourcefacts.CodexSourceInspected, Observations: 1},
		{Profile: sourcefacts.CodexPaginatedJSONL, Version: "0.999.0-alpha.1", Source: "vscode", Evidence: sourcefacts.CodexCompatibleUntested, Observations: 1},
	}}
	app := appStatus{Name: "codex", Hooks: "absent", InstalledVersion: "0.123.0", VersionSupport: "unverified", Discovery: &h}
	view := statusView{configured: true, Background: "loaded", Apps: []appStatus{app}}
	for _, verbose := range []bool{false, true} {
		var out bytes.Buffer
		printStatus(&out, view, statusScreen{style: styleFor(&out), now: at, verbose: verbose})
		for _, expected := range []string{"Recorded session formats:", "0.159.3 (cli, codex_jsonl_legacy, runtime tested)", "0.155.0-alpha.9.2 (vscode, codex_jsonl_paginated, source inspected)", "0.999.0-alpha.1 (vscode, codex_jsonl_paginated, compatible untested)", "skipped 1", "unsupported history: 1", "capture still requires consent and publication/read-back"} {
			if !strings.Contains(out.String(), expected) {
				t.Errorf("missing %q in status:\n%s", expected, out.String())
			}
		}
		if strings.Contains(out.String(), "0.123.0 (") || strings.Contains(out.String(), "skipped 4") {
			t.Fatal("installed executable affected observed format diagnostics")
		}
	}
	raw, err := json.Marshal(view)
	if err != nil || !bytes.Contains(raw, []byte(`"producer_version":"0.999.0-alpha.1"`)) || !bytes.Contains(raw, []byte(`"evidence":"compatible_untested"`)) {
		t.Fatal("JSON status lost format evidence", err)
	}
}
