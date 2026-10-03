package stats

import (
	"fmt"
	"strings"
)

// Label returns the display name, falling back to the recorded identity.
func (s MCPServer) Label() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}

var knownMCPDisplayNames = map[string]string{
	"Claude_Browser":            "Claude Browser",
	"Claude_Code_iOS_Simulator": "iOS Simulator",
}

func mcpDisplayName(name string, aliases map[string]string, ordinal int) string {
	if label := strings.TrimSpace(aliases[name]); label != "" {
		return label
	}
	if label := knownMCPDisplayNames[name]; label != "" {
		return label
	}
	// A UUID gives no reliable evidence of which service it represents.
	if isUUID(name) {
		return fmt.Sprintf("Unknown MCP server %d", ordinal)
	}
	return ""
}

func isUUID(name string) bool {
	if len(name) != 36 {
		return false
	}
	for i, r := range name {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// ChartDays omits days before the first available session. It preserves gaps
// within the available history and the requested window in the underlying data.
func (s Stats) ChartDays() []Day {
	days := s.Daily
	for len(days) > 0 && s.Coverage.FirstRecordedDay != "" && days[0].Date < s.Coverage.FirstRecordedDay {
		days = days[1:]
	}
	return days
}

// ChartCoverageNote explains the chart's evidence boundary without claiming
// that the collector ran continuously or that absent sessions mean inactivity.
func (s Stats) ChartCoverageNote() string {
	if s.Coverage.FirstRecordedDay == "" || s.Coverage.FirstRecordedDay <= s.Window.FirstDay {
		return ""
	}
	return "Available history starts " + s.Coverage.FirstRecordedDay + "; earlier days omitted. Empty days mean no archived sessions."
}
