package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

type capabilityEvidence struct {
	State      string `json:"state"`
	Evidence   string `json:"evidence"`
	NextAction string `json:"next_action,omitempty"`
}

type captureCapabilities struct {
	FreshStart      capabilityEvidence `json:"fresh_start"`
	Transcript      capabilityEvidence `json:"transcript"`
	Lifecycle       capabilityEvidence `json:"lifecycle"`
	SkillEvidence   capabilityEvidence `json:"skill_evidence"`
	SubagentLinkage capabilityEvidence `json:"subagent_linkage"`
	AdapterFixtures capabilityEvidence `json:"adapter_fixtures"`
}

type applicationDiscovery struct {
	Installed     bool   `json:"installed"`
	Version       string `json:"version,omitempty"`
	VersionSource string `json:"version_source,omitempty"`
	// VersionKind names the numbering scheme the discovered version belongs
	// to: versionKindCLI for a `--version` answer, versionKindAppBundle for a
	// macOS bundle's CFBundleShortVersionString. Captures report the harness's
	// own version (Codex cli_version, Claude Code record version, Cursor hook
	// cursor_version), which may be numbered differently from an app bundle.
	VersionKind  string    `json:"version_kind,omitempty"`
	VersionState string    `json:"version_state"`
	ObservedAt   time.Time `json:"observed_at"`
}

const (
	versionKindCLI       = "cli"
	versionKindAppBundle = "app_bundle"
)

// Reason codes for installed_version_support = unverified. They are an API.
const (
	supportReasonNoVerifiedCapture = "no_verified_capture"
	supportReasonNoMatchingVersion = "no_matching_verified_version"
	// supportReasonVersionSourceMismatch: the installed version and every
	// verified capture's version follow different numbering schemes (for
	// example a Cursor app-bundle version against the hook's cursor_version),
	// so they cannot match even when the same build produced both.
	supportReasonVersionSourceMismatch = "version_source_mismatch"
)

func applicationDiscoveriesPath(home string) string {
	return filepath.Join(home, "application-versions.json")
}

func recordApplicationDiscoveries(home string, discoveries map[string]applicationDiscovery, now time.Time) error {
	for name, discovery := range discoveries {
		discovery.ObservedAt = now.UTC()
		discoveries[name] = discovery
	}
	return local.Write(applicationDiscoveriesPath(home), discoveries)
}

func readApplicationDiscoveries(home string) (map[string]applicationDiscovery, error) {
	result := map[string]applicationDiscovery{}
	if err := local.Read(applicationDiscoveriesPath(home), &result); err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}
	return result, nil
}

func captureCapabilityProfile(name string) captureCapabilities {
	documented := func(evidence string) capabilityEvidence {
		return capabilityEvidence{State: "documented", Evidence: evidence}
	}
	unavailable := func(evidence, next string) capabilityEvidence {
		return capabilityEvidence{State: "unavailable", Evidence: evidence, NextAction: next}
	}
	profile := captureCapabilities{
		SkillEvidence:   unavailable("No supported native eligibility/use contract has been verified.", "Treat eligibility comparisons as unavailable."),
		SubagentLinkage: unavailable("A lifecycle event alone does not provide a verified child transcript and parent link.", "Validate child identity, parent identity, and transcript path for the installed version."),
		AdapterFixtures: documented("Synthetic fixtures exercise the bounded adapter; they do not prove an installed version."),
	}
	switch canonicalHarness(name) {
	case "codex":
		profile.FreshStart = documented("SessionStart.source distinguishes startup/clear from resume/compact.")
		profile.Transcript = documented("Hooks provide transcript_path; official documentation says its format is not stable.")
		profile.Lifecycle = documented("SessionStart, Stop, Interrupt, SessionEnd, SubagentStart, and SubagentStop are documented.")
	case "claude":
		profile.SubagentLinkage = capabilityEvidence{State: "fixture_validated", Evidence: "Documented SubagentStop identity/path plus synthetic JSONL ownership and native timestamp fixtures. Actual installed-version capture is unverified.", NextAction: "Run a synthetic parent/child capture and read-back for the installed version."}
		profile.FreshStart = documented("SessionStart.source distinguishes startup/clear from resume/compact.")
		profile.Transcript = documented("Hooks provide transcript_path to the native JSONL transcript.")
		profile.Lifecycle = documented("SessionStart, Stop, SessionEnd, and SubagentStop are documented.")
	case "cursor":
		// Observed on the desktop app 3.21.13: a new chat fires no
		// sessionStart; its first hook is beforeSubmitPrompt with
		// transcript_path null, and afterAgentResponse and stop then name the
		// transcript. A resumed chat's first prompt already names its
		// non-empty transcript. cursor_version is not evidence either way.
		profile.FreshStart = documented("A never-seen chat is registered at its first beforeSubmitPrompt (or sessionStart) when transcript_path is null, absent, or names a missing or empty file; a transcript that already has bytes is a resume and is declined. Observed on Cursor 3.21.13.")
		profile.Transcript = documented("A new chat's first prompt carries transcript_path null; afterAgentResponse and stop name ~/.cursor/projects/<workspace>/agent-transcripts/<id>/<id>.jsonl, which is recorded only when it matches the conversation id. Observed on Cursor 3.21.13.")
		profile.Lifecycle = documented("beforeSubmitPrompt, afterAgentResponse, stop, and sessionEnd fire for a desktop chat (sessionEnd can fire mid-turn); sessionStart, subagentStart, and subagentStop are documented.")
	default:
		unknown := capabilityEvidence{State: "unknown", Evidence: "No capability contract is registered."}
		return captureCapabilities{unknown, unknown, unknown, unknown, unknown, unknown}
	}
	return profile
}

func discoverApplications(userHome string) map[string]applicationDiscovery {
	return map[string]applicationDiscovery{
		"codex":  discoverCommandVersion("codex", codexVersionCandidates(userHome)),
		"claude": discoverCommandVersion("claude", claudeVersionCandidates(userHome)),
		"cursor": discoverCursorVersion(userHome),
	}
}

// codexVersionCandidates lists where a Codex CLI may be, standalone installs
// first. The ChatGPT desktop app bundles its own copy, which may be the only
// one on a machine that never installed the CLI.
func codexVersionCandidates(userHome string) [][]string {
	var candidates [][]string
	for _, path := range []string{
		"/Applications/Codex.app/Contents/Resources/codex",
		filepath.Join(userHome, "Applications/Codex.app/Contents/Resources/codex"),
		"codex",
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		filepath.Join(userHome, "Applications/ChatGPT.app/Contents/Resources/codex"),
	} {
		candidates = append(candidates, []string{path, "--version"})
	}
	return candidates
}

// claudeVersionCandidates lists where a Claude Code CLI may be: PATH, the
// native and legacy local install locations (setup may run with a minimal
// PATH), then the copies the Claude desktop app keeps, newest first.
//
// A bundled copy is only a fallback: the version recorded from it may belong
// to an older bundle (when the newest one does not answer) or differ from a
// CLI installed off PATH that the hooks actually run. That affects only
// whether status labels the installed version verified or unverified. The
// same holds for the Codex copy inside ChatGPT.app.
func claudeVersionCandidates(userHome string) [][]string {
	paths := []string{
		"claude",
		filepath.Join(userHome, ".local/bin/claude"),
		filepath.Join(userHome, ".claude/local/claude"),
	}
	paths = append(paths, claudeDesktopBundledCLIs(userHome)...)
	candidates := make([][]string, len(paths))
	for i, path := range paths {
		candidates[i] = []string{path, "--version"}
	}
	return candidates
}

// versionDirPattern matches a directory named exactly for a version, such as
// "2.1.280"; unlike versionPattern it does not find one inside other text,
// so "backup-2.1.300" is not a version directory.
var versionDirPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)+(?:[-+][0-9A-Za-z.-]+)?$`)

// claudeDesktopBundledCLIs returns the Claude Code executables the Claude
// desktop app keeps under one directory per version, newest version first.
// Versions compare numerically, so 2.1.100 sorts above 2.1.99.
func claudeDesktopBundledCLIs(userHome string) []string {
	root := filepath.Join(userHome, "Library/Application Support/Claude/claude-code")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var versions []string
	for _, entry := range entries {
		if entry.IsDir() && versionDirPattern.MatchString(entry.Name()) {
			versions = append(versions, entry.Name())
		}
	}
	sort.SliceStable(versions, func(i, j int) bool { return compareDottedVersions(versions[i], versions[j]) > 0 })
	paths := make([]string, len(versions))
	for i, version := range versions {
		paths[i] = filepath.Join(root, version, "claude.app/Contents/MacOS/claude")
	}
	return paths
}

// compareDottedVersions orders two versions by their numeric components,
// returning -1, 0, or 1. A missing component counts as zero, and any
// pre-release or build suffix is ignored.
func compareDottedVersions(a, b string) int {
	parts := func(value string) []string {
		value = normalizedVersion(value)
		if i := strings.IndexAny(value, "-+"); i >= 0 {
			value = value[:i]
		}
		return strings.Split(value, ".")
	}
	left, right := parts(a), parts(b)
	for i := 0; i < len(left) || i < len(right); i++ {
		var l, r int
		if i < len(left) {
			l, _ = strconv.Atoi(left[i])
		}
		if i < len(right) {
			r, _ = strconv.Atoi(right[i])
		}
		if l != r {
			if l < r {
				return -1
			}
			return 1
		}
	}
	return 0
}

// discoverCommandVersion tries each candidate in order. A candidate that is
// present but fails to answer does not stop discovery; only when every present
// candidate fails is the version unknown, and only when none is present is the
// application absent.
func discoverCommandVersion(name string, candidates [][]string) applicationDiscovery {
	present := false
	for _, original := range candidates {
		candidate := append([]string(nil), original...)
		path := candidate[0]
		if filepath.IsAbs(path) {
			if info, err := os.Stat(path); err != nil || info.IsDir() {
				continue
			}
		} else if resolved, err := exec.LookPath(path); err == nil {
			candidate[0] = resolved
		} else {
			continue
		}
		present = true
		if version, ok := boundedVersionCommand(candidate...); ok {
			return applicationDiscovery{Installed: true, Version: version, VersionSource: candidate[0] + " --version", VersionKind: versionKindCLI, VersionState: "observed"}
		}
	}
	if present {
		return applicationDiscovery{Installed: true, VersionSource: name + " --version", VersionKind: versionKindCLI, VersionState: "unknown"}
	}
	return applicationDiscovery{VersionState: "absent"}
}

func discoverCursorVersion(userHome string) applicationDiscovery {
	for _, bundle := range []string{"/Applications/Cursor.app", filepath.Join(userHome, "Applications/Cursor.app")} {
		if info, err := os.Stat(bundle); err != nil || !info.IsDir() {
			continue
		}
		plist := filepath.Join(bundle, "Contents", "Info.plist")
		version, ok := boundedVersionCommand("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", plist)
		if ok {
			return applicationDiscovery{Installed: true, Version: version, VersionSource: plist + ":CFBundleShortVersionString", VersionKind: versionKindAppBundle, VersionState: "observed"}
		}
		return applicationDiscovery{Installed: true, VersionSource: plist, VersionKind: versionKindAppBundle, VersionState: "unknown"}
	}
	return applicationDiscovery{VersionState: "absent"}
}

func boundedVersionCommand(argv ...string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 250 * time.Millisecond
	var output cappedBuffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false
	}
	value := strings.TrimSpace(output.String())
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	return value, true
}

func installedVersionSupport(discovery applicationDiscovery, verifiedVersions []string) string {
	state, _ := installedVersionSupportDetail(discovery, verifiedVersions)
	return state
}

// installedVersionSupportDetail reports the support state and, when it is
// unverified, a reason code saying why the installed version is not matched by
// a verified capture.
func installedVersionSupportDetail(discovery applicationDiscovery, verifiedVersions []string) (string, string) {
	if !discovery.Installed {
		if discovery.VersionState != "absent" {
			return "unknown", ""
		}
		return "absent", ""
	}
	if discovery.Version == "" || discovery.VersionState == "stale" {
		return "unknown", ""
	}
	installed := normalizedVersion(discovery.Version)
	if len(verifiedVersions) == 0 {
		return "unverified", supportReasonNoVerifiedCapture
	}
	comparable := false
	for _, version := range verifiedVersions {
		verified := normalizedVersion(version)
		if installed != "" && installed == verified {
			return "verified_by_capture", ""
		}
		if versionShape(installed) == versionShape(verified) {
			comparable = true
		}
	}
	if !comparable {
		return "unverified", supportReasonVersionSourceMismatch
	}
	return "unverified", supportReasonNoMatchingVersion
}

// versionPattern finds a dotted numeric version with an optional pre-release
// or build suffix anywhere in a tool's `--version` answer, such as
// "codex-cli 1.2.3", "v1.2.3", or "1.2.3.4".
var versionPattern = regexp.MustCompile(`(?:^|[^0-9])([0-9]+(?:\.[0-9]+)+(?:[-+][0-9A-Za-z.-]+)?)(?:$|[^0-9A-Za-z.+-])`)

// normalizedVersion extracts the version number from a version answer. When
// no dotted numeric version is present, the trimmed answer itself is the
// version so opaque schemes still compare by exact text.
func normalizedVersion(value string) string {
	value = strings.TrimSpace(value)
	match := versionPattern.FindStringSubmatch(value)
	if len(match) == 2 {
		return match[1]
	}
	return value
}

// versionShape classifies a version as a dotted numeric string or an opaque
// label. Two versions of different shapes come from different numbering
// schemes and cannot be compared.
func versionShape(value string) string {
	if versionPattern.MatchString(value) {
		return "dotted"
	}
	return "opaque"
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	const limit = 4096
	remaining := limit - b.Len()
	if remaining <= 0 {
		return 0, errors.New("version output exceeded limit")
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		return remaining, errors.New("version output exceeded limit")
	}
	return b.Buffer.Write(p)
}
