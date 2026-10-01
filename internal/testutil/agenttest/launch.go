package agenttest

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"slices"
	"testing"
)

// LaunchConformance preserves native argv and ownership of returned slices.
func LaunchConformance(t *testing.T, launcher agentapi.Launcher, binaries []string, want []string) {
	t.Helper()
	got, err := launcher.Args(agentapi.LaunchRequest{ProjectDir: "/project", HandoffPath: "/private/handoff.md", Prompt: "PROMPT", ExtraArgs: []string{"--model", "x"}})
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("argv %q error %v want %q", got, err, want)
	}
	if !slices.Equal(launcher.Executables().Names, binaries) {
		t.Fatal("binary order changed")
	}
	names := launcher.Executables().Names
	names[0] = "MUTATED"
	if !slices.Equal(launcher.Executables().Names, binaries) {
		t.Fatal("mutable executable inventory")
	}
	if _, err := launcher.Args(agentapi.LaunchRequest{ExtraArgs: []string{"--"}}); err == nil {
		t.Fatal("accepted prompt separator collision")
	}
}
