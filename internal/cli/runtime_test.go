package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

const syntheticRuntimeKey = "SYNTHETIC_RUNNING"
const syntheticCleanupKey = "SYNTHETIC_PARENT"

type syntheticDetector struct{ presence bool }

func (s syntheticDetector) Detect(e agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	value, set := e.LookupEnv(syntheticRuntimeKey)
	if !set || strings.TrimSpace(value) == "" {
		return agentapi.RuntimeObservation{}
	}
	got := agentapi.RuntimeObservation{PresenceKey: syntheticRuntimeKey, ProjectLatest: s.presence}
	if !s.presence {
		got.NativeID = strings.TrimSpace(value)
	}
	return got
}
func (syntheticDetector) SessionEnvironmentKeys() []string {
	return []string{syntheticRuntimeKey, syntheticCleanupKey}
}

func syntheticRuntimeRegistry(t *testing.T, presence bool) *builtin.Registry {
	t.Helper()
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: syntheticID, DisplayName: "Synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := builtin.New(c, []builtin.Integration{{Descriptor: agentmeta.Descriptor{ID: syntheticID}, Launcher: syntheticLauncher{}, Runtime: syntheticDetector{presence: presence}}})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestInjectedRuntimeDrivesCurrentSessionAndInteraction(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	env.Agents = syntheticRuntimeRegistry(t, false)
	env.LookupEnv = agentEnv(map[string]string{syntheticRuntimeKey: "  exact-native  "})
	env.IsTerminal = func(any) bool { return true }
	mode, err := env.nonInteractive()
	if err != nil || !mode.on || !strings.Contains(mode.reason, syntheticRuntimeKey) || env.interactive(nil) {
		t.Fatalf("mode %+v %v", mode, err)
	}
	base := archive.SessionRegistration{ArchiveSessionID: "older", NativeSessionID: "exact-native", ProjectID: "test", ProjectRoot: home, Harness: archive.Harness{Name: string(syntheticID)}, RegisteredAt: time.Unix(1, 0), SessionStartedAt: time.Unix(1, 0), TranscriptPath: filepath.Join(home, "transcript.jsonl")}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	must(t, store.SaveRegistration(base))
	latest := base
	latest.ArchiveSessionID = "newer"
	latest.RegisteredAt = time.Unix(2, 0)
	must(t, store.SaveRegistration(latest))
	child := latest
	child.ArchiveSessionID = "child"
	child.ParentSessionID = "older"
	child.SubagentID = "child-native"
	child.RegisteredAt = time.Unix(3, 0)
	must(t, store.SaveRegistration(child))
	id, ok, err := currentHandoffSession(env, home, handoffOptions{})
	if err != nil || !ok || id != "newer" {
		t.Fatalf("current %s %v %v", id, ok, err)
	}
	if _, ok, err := currentHandoffSession(env, home, handoffOptions{harness: "codex"}); err != nil || ok {
		t.Fatalf("harness bypass %v %v", ok, err)
	}
	if !isCallingAgent(env, base) {
		t.Fatal("calling agent missed synthetic runtime")
	}
	other := base
	other.Harness.Name = "codex"
	if isCallingAgent(env, other) {
		t.Fatal("wrong harness calling agent")
	}
	if !currentSessions(env)["exact-native"] {
		t.Fatal("latest exclusion missed exact runtime")
	}
	candidate := nativesessions.Candidate{NativeID: "exact-native", Ref: nativesessions.Ref{Harness: string(syntheticID)}}
	selected, err := nativeCurrent([]nativesessions.Candidate{candidate}, "", env)
	if err != nil || selected == nil || !isCurrentNative(candidate, env) {
		t.Fatalf("native current %+v %v", selected, err)
	}
	if _, err := nativeCurrent([]nativesessions.Candidate{candidate, candidate}, "", env); err == nil {
		t.Fatal("duplicate native identity accepted")
	}
	env.LookupEnv = agentEnv(map[string]string{syntheticRuntimeKey: "exact"})
	if selected, err := nativeCurrent([]nativesessions.Candidate{candidate}, "", env); err != nil || selected != nil {
		t.Fatal("prefix runtime matched")
	}
	env.LookupEnv = agentEnv(map[string]string{syntheticCleanupKey: "parent"})
	mode, _ = env.nonInteractive()
	if mode.on {
		t.Fatal("cleanup key disabled prompts")
	}
	env.LookupEnv = agentEnv(map[string]string{syntheticRuntimeKey: "exact-native", envNonInteractive: "0"})
	if !env.interactive(nil) {
		t.Fatal("explicit override failed")
	}
	env.LookupEnv = agentEnv(map[string]string{syntheticRuntimeKey: "exact-native", envNonInteractive: "typo"})
	mode, err = env.nonInteractive()
	if err == nil || !mode.on {
		t.Fatal("invalid override did not fail closed")
	}
}

func TestInjectedPresenceDrivesGenericProjectFallback(t *testing.T) {
	t.Parallel()
	env := Env{Agents: syntheticRuntimeRegistry(t, true), LookupEnv: agentEnv(map[string]string{syntheticRuntimeKey: "0"})}
	if projectRuntime(env, "") != string(syntheticID) || projectRuntime(env, "codex") != "" || len(currentSessions(env)) != 0 {
		t.Fatal("presence confused with exact identity")
	}
	if !isCallingAgent(env, archive.SessionRegistration{Harness: archive.Harness{Name: string(syntheticID)}}) {
		t.Fatal("presence calling-agent missed")
	}
	var stderr bytes.Buffer
	opts := handoffOptions{to: "codex"}
	_, done := chooseHandoffSession(&opts, t.TempDir(), false, nil, io.Discard, &stderr, env)
	if done || !opts.latest || opts.harness != string(syntheticID) {
		t.Fatalf("fallback %+v done %v %s", opts, done, stderr.String())
	}
}

// Exercise the actual prepared terminal script, not just its Unset inventory.
func TestPreparedTerminalStripsRuntimeAndTracePreservesConfiguration(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	env.Agents = syntheticRuntimeRegistry(t, false)
	binary := filepath.Join(home, "synthetic")
	output := filepath.Join(home, "child-env")
	must(t, os.WriteFile(binary, []byte("#!/bin/sh\n/usr/bin/env > "+shellQuote(output)+"\n"), 0o700))
	env.Executable = func() (string, error) { return "/bin/agent-archive", nil }
	env.WorkingDir = func() (string, error) { return home, nil }
	env.LookPath = func(string) (string, error) { return binary, nil }
	inherited := []string{"PATH=/usr/bin:/bin", syntheticRuntimeKey + "=current", syntheticCleanupKey + "=parent", envTrace + "=1", "CODEX_HOME=/codex", "CLAUDE_CODE_USE_BEDROCK=1"}
	env.Environ = func() []string { return inherited }
	spec, err := buildLaunchSpec(syntheticDestination, "prompt", "/private/handoff.md", home, nil, env)
	if err != nil || !slices.Equal(spec.Env, []string{"PATH=/usr/bin:/bin", "CODEX_HOME=/codex", "CLAUDE_CODE_USE_BEDROCK=1"}) {
		t.Fatalf("foreground %+v %v", spec, err)
	}
	env.OpenTerminal = func(spec termlaunch.Spec) (string, error) {
		return termlaunch.Open(context.Background(), spec, termlaunch.Environment{OS: platform.Linux, LookupEnv: agentEnv(map[string]string{"TMUX": "fake"}), Run: func(_ context.Context, name string, args ...string) error {
			if name != "tmux" {
				t.Fatalf("unexpected terminal %s", name)
			}
			script := strings.Trim(args[len(args)-1], "'")
			cmd := exec.Command("/bin/sh", script)
			cmd.Env = inherited
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("terminal script %s %v", raw, err)
			}
			return nil
		}})
	}
	var stderr bytes.Buffer
	target := handoffTarget{filePath: "/source.jsonl", source: "file"}
	err = launchPreparedHandoff([]byte("filtered record"), archive.Handoff{}, target, syntheticDestination, false, handoffOptions{file: "/source.jsonl"}, home, strings.NewReader(""), strings.NewReader(""), io.Discard, &stderr, env)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	vars := strings.Split(string(raw), "\n")
	for _, key := range launchEnvironmentKeys(env) {
		for _, kv := range vars {
			if strings.HasPrefix(kv, key+"=") {
				t.Fatalf("terminal inherited %s", key)
			}
		}
	}
	for _, kept := range []string{"CODEX_HOME=/codex", "CLAUDE_CODE_USE_BEDROCK=1"} {
		if !slices.Contains(vars, kept) {
			t.Fatalf("terminal lost %s: %s", kept, raw)
		}
	}
}

func TestRuntimePrecedenceAndNativeAmbiguityRemainShared(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	env.LookupEnv = agentEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": "claude-native", "CODEX_THREAD_ID": "codex-native", "CURSOR_AGENT": "0"})
	mode, err := env.nonInteractive()
	if err != nil || !strings.HasPrefix(mode.reason, "CLAUDE_CODE_SESSION_ID ") {
		t.Fatalf("positive key precedence %+v %v", mode, err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct{ agent, native, id string }{{"claude", "claude-native", "claude-archive"}, {"codex", "codex-native", "codex-archive"}} {
		reg := archive.SessionRegistration{ArchiveSessionID: pair.id, NativeSessionID: pair.native, ProjectID: "test", ProjectRoot: home, Harness: archive.Harness{Name: pair.agent}, RegisteredAt: time.Unix(1, 0), SessionStartedAt: time.Unix(1, 0), TranscriptPath: filepath.Join(home, "transcript.jsonl")}
		must(t, store.SaveRegistration(reg))
	}
	id, ok, err := currentHandoffSession(env, home, handoffOptions{})
	if err != nil || !ok || id != "claude-archive" {
		t.Fatalf("configured precedence %s %v %v", id, ok, err)
	}
	id, ok, err = currentHandoffSession(env, home, handoffOptions{harness: "codex"})
	if err != nil || !ok || id != "codex-archive" {
		t.Fatalf("configured constraint %s %v %v", id, ok, err)
	}
	candidates := []nativesessions.Candidate{{NativeID: "claude-native", Ref: nativesessions.Ref{Harness: "claude"}}, {NativeID: "codex-native", Ref: nativesessions.Ref{Harness: "codex"}}}
	if _, err := nativeCurrent(candidates, "", env); err == nil {
		t.Fatal("multiple exact native observations accepted")
	}
	selected, err := nativeCurrent(candidates, "codex", env)
	if err != nil || selected == nil || selected.Ref.Harness != "codex" {
		t.Fatalf("native constraint %+v %v", selected, err)
	}
}
