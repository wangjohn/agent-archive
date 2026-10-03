package stats

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestMCPDisplayLabelsPreserveIdentity(t *testing.T) {
	id := "1a59c906-04da-521d-bda7-0123456789ab"
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sessions := []archive.Metadata{meta("one", "claude", now, mcp(id, 3), mcp("Claude_Browser", 5), mcp("Claude_Code_iOS_Simulator", 4))}
	for _, aliases := range []map[string]string{nil, {id: "GitHub"}} {
		s := Compute(sessions, Options{Now: now, Location: time.UTC, MCPServerNames: aliases})
		for _, srv := range s.MCP.Servers {
			switch srv.Name {
			case "Claude_Browser":
				if srv.Label() != "Claude Browser" {
					t.Fatal(srv)
				}
			case "Claude_Code_iOS_Simulator":
				if srv.Label() != "iOS Simulator" {
					t.Fatal(srv)
				}
			case id:
				want := "Unknown MCP server 1"
				if aliases != nil {
					want = "GitHub"
				}
				if srv.Label() != want || srv.Calls != 3 {
					t.Fatal(srv)
				}
			default:
				t.Fatalf("lost identity: %+v", srv)
			}
		}
	}
}

func TestChartStartsAtAvailableHistoryAndPreservesGaps(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sessions := []archive.Metadata{meta("start", "claude", now.AddDate(0, 0, -4)), meta("end", "claude", now)}
	s := Compute(sessions, Options{Now: now, Days: 30, Location: time.UTC})
	if s.Coverage.FirstRecordedDay != "2026-09-27" || len(s.ChartDays()) != 5 || len(s.Daily) != 30 {
		t.Fatalf("coverage %+v, chart %d, window %d", s.Coverage, len(s.ChartDays()), len(s.Daily))
	}
	if s.ChartDays()[1].Sessions != 0 || s.ChartCoverageNote() == "" {
		t.Fatal("gap or coverage note lost")
	}
	// Older available sessions establish that the whole requested window is
	// within the history, even when its first days contain no sessions.
	sessions = append(sessions, meta("old", "claude", now.AddDate(0, 0, -40)))
	s = Compute(sessions, Options{Now: now, Days: 30, Location: time.UTC})
	if len(s.ChartDays()) != 30 || s.ChartCoverageNote() != "" {
		t.Fatal("older history did not retain the full chart")
	}
}
