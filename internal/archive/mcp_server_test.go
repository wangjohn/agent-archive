package archive

import (
	"testing"
)

func TestMCPServer(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"mcp__github__create_issue":                "github",
		"mcp__Claude_Code_iOS_Simulator__control":  "Claude_Code_iOS_Simulator",
		"mcp__a-b.c__tool__with__underscores":      "a-b.c",
		"mcp__1a59c906-04da-521d-bda7-7f7__update": "1a59c906-04da-521d-bda7-7f7",
		"mcp__server":     "",
		"mcp__server__":   "",
		"mcp____tool":     "",
		"mcp__":           "",
		"Bash":            "",
		"MCP__github__x":  "",
		"xmcp__github__x": "",
		"search_docs":     "",
		"":                "",
	} {
		got, ok := mcpServer(name)
		if got != want || ok != (want != "") {
			t.Errorf("mcpServer(%q) = (%q, %v), want %q", name, got, ok, want)
		}
	}
}
