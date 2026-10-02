package orbifold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/skillconfig"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
	"github.com/wangjohn/agent-archive/internal/filechange"
)

// Skills declares fixture-owned locations, separately from other integrations.
func Skills() skillconfig.Provider {
	return skillconfig.Provider{ManagedSuffix: ".orbifold/skills", Roots: []skillconfig.Root{{Suffix: ".orbifold/skills", Scope: "user_orbits"}, {Suffix: ".orbifold/skills", Scope: "project_orbits", Project: true}}}
}

// Hooks interprets an independent JSON hook settings layout, preserving foreign fields.
type Hooks struct{}

func (Hooks) Location(l agentapi.HookLocations) string {
	return filepath.Join(l.UserHome, ".orbifold", "starboard.json")
}
func (Hooks) EnvironmentKeys() []string { return []string{"ORBIT_CONFIG"} }
func hookDocument(f agentapi.HookFile) (map[string]any, error) {
	if f.ReadError != nil {
		return nil, f.ReadError
	}
	d := map[string]any{}
	if f.Present {
		if !f.Regular {
			return nil, errors.New("nonregular starboard")
		}
		if err := json.Unmarshal(f.Bytes, &d); err != nil {
			return nil, err
		}
	}
	return d, nil
}
func (Hooks) Inspect(r agentapi.HookInspectionRequest) (agentapi.HookInspection, error) {
	d, err := hookDocument(r.File)
	if err != nil {
		return agentapi.HookInspection{State: agentapi.HookUnreadable}, err
	}
	if !r.File.Present {
		return agentapi.HookInspection{State: agentapi.HookAbsent}, nil
	}
	entries, _ := d["orbits"].(map[string]any)
	_, found := entries[r.Owner.DataHome]
	state := agentapi.HookForeign
	if found {
		state = agentapi.HookOwned
	}
	return agentapi.HookInspection{State: state, Installed: found}, nil
}
func (h Hooks) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	if r.Action != agentapi.HookInstall && r.Action != agentapi.HookRemove {
		return nil, errors.New("unknown starboard action")
	}
	d, err := hookDocument(r.File)
	if err != nil {
		return nil, err
	}
	entries, _ := d["orbits"].(map[string]any)
	if entries == nil {
		entries = map[string]any{}
	}
	if r.Action == agentapi.HookRemove {
		if _, found := entries[r.Owner.DataHome]; !found {
			return nil, nil
		}
		delete(entries, r.Owner.DataHome)
	} else {
		entries[r.Owner.DataHome] = map[string]any{"birth": r.Owner.Executable, "reply": r.Owner.Executable, "rest": r.Owner.Executable}
	}
	d["orbits"] = entries
	after, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(after, r.File.Bytes) {
		return nil, nil
	}
	return []filechange.Change{{Path: r.File.Path, Before: r.File.Bytes, After: after, Existed: r.File.Present, Mode: 0600}}, nil
}

// Launcher keeps native argv independent of shared process execution.
type Launcher struct{}

func (Launcher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"orbit-run"}, Install: "synthetic fixture only"}
}
func (Launcher) Args(r agentapi.LaunchRequest) ([]string, error) {
	if r.ProjectDir == "" {
		return nil, errors.New("landing directory required")
	}
	return append([]string{"--landing", r.ProjectDir, "--carry", r.Prompt}, r.ExtraArgs...), nil
}
func (p *Ports) SessionEnvironmentKeys() []string { return []string{"ORBIT_NATIVE_KEY"} }
func (p *Ports) Detect(e agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	if e.LookupEnv == nil {
		return agentapi.RuntimeObservation{}
	}
	id, ok := e.LookupEnv("ORBIT_NATIVE_KEY")
	if !ok {
		return agentapi.RuntimeObservation{}
	}
	return agentapi.RuntimeObservation{NativeID: id, PresenceKey: "ORBIT_NATIVE_KEY"}
}
func (p *Ports) ObserveVersion(e agentapi.VersionEnvironment) agentapi.ApplicationDiscovery {
	out := agentapi.ApplicationDiscovery{VersionState: "unavailable"}
	if e.Host == nil {
		return out
	}
	path, found := e.Host.ResolveExecutable("orbit-run")
	if !found {
		return out
	}
	out.Installed = true
	version, observed := e.Host.ProbeVersion(agentapi.VersionProbe{Kind: agentapi.VersionCLI, Path: path})
	if observed {
		out.Version = version
		out.VersionState = "observed"
		out.VersionSource = "synthetic_cli"
	}
	return out
}
func (p *Ports) CaptureEvidence() agentapi.CaptureCapabilities {
	validated := agentapi.CapabilityEvidence{State: agentapi.CapabilityFixtureValidated, Evidence: "synthetic Orbifold fixtures only"}
	unavailable := agentapi.CapabilityEvidence{State: agentapi.CapabilityUnavailable, Evidence: "no child native evidence declared"}
	return agentapi.CaptureCapabilities{FreshStart: validated, Transcript: validated, Lifecycle: validated, SkillEvidence: validated, SubagentLinkage: unavailable, AdapterFixtures: validated}
}
func (p *Ports) PreviewRecord(ctx context.Context, raw []byte) (archive.RecordPreview, error) {
	if err := ctx.Err(); err != nil {
		return archive.RecordPreview{}, err
	}
	var pulse NativePulse
	if err := json.Unmarshal(raw, &pulse); err != nil {
		return archive.RecordPreview{}, err
	}
	if pulse.PulseKind != "exchange" {
		return archive.RecordPreview{}, archive.ErrUnsafeSourceFormat
	}
	text, _ := archive.RedactSensitive(pulse.Words)
	kind := archive.TurnKindAssistant
	if pulse.Speaker == "pilot" {
		kind = archive.TurnKindHumanPrompt
	}
	return archive.RecordPreview{Title: text, Kind: kind}, nil
}

// Discovery owns the unusual native header and filename vocabulary.
type Discovery struct{ Qualified bool }

func (Discovery) DefaultDirectories(home string) []string {
	return []string{filepath.Join(home, ".orbifold", "constellations")}
}
func (d Discovery) Roots(l agentapi.NativeLocations, purpose agentapi.DiscoveryPurpose) []agentapi.NativeStoreRoot {
	if purpose == agentapi.DiscoveryHandoff {
		return nil
	}
	dirs := l.Directories
	if len(dirs) == 0 {
		dirs = d.DefaultDirectories(l.UserHome)
	}
	var roots []agentapi.NativeStoreRoot
	for _, dir := range dirs {
		roots = append(roots, agentapi.NativeStoreRoot{Harness: string(ID), Path: dir, Prefix: "orbit-", Suffix: ".orbit"})
	}
	return roots
}
func (Discovery) InspectHeader(r agentapi.NativeHeaderRequest) (agentapi.NativeHeader, error) {
	var header agentapi.NativeHeader
	var parseErr error
	err := r.Scan(func(raw []byte) bool {
		var pulse NativePulse
		if parseErr = json.Unmarshal(raw, &pulse); parseErr != nil {
			return false
		}
		header.NativeID = pulse.NativeIdentity
		header.Directory = pulse.Landing
		return false
	})
	return header, errors.Join(parseErr, err)
}
func (d Discovery) Discover(ctx context.Context, r agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (agentapi.DiscoveryReport, error) {
	roots := r.Roots
	if len(roots) == 0 {
		roots = d.Roots(r.Locations, r.Purpose)
	}
	var report agentapi.DiscoveryReport
	if r.Purpose == agentapi.DiscoveryHandoff {
		return report, errors.New("Orbifold manifest has no bounded native handoff file view")
	}
	for _, root := range roots {
		coverage, err := discoveryio.Walk(ctx, r.Files, root, r.MaxFiles, func(ref discoveryio.Ref) (bool, error) {
			info, err := r.Files.Lstat(ref.Path)
			if err != nil {
				return true, err
			}
			id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(ref.Path), "orbit-"), ".orbit")
			c := agentapi.DiscoveryCandidate{Session: agentapi.NativeSession{Agent: ID, NativeID: id}, Source: agentapi.SourceRef{Path: ref.Path}, Root: ref.Store, Bytes: info.Size()}
			if d.Qualified {
				c.Source.Kind = Kind
				c.Source.Key = id
			}
			if r.Stage == agentapi.DiscoveryIdentities {
				header, err := d.InspectHeader(agentapi.NativeHeaderRequest{Purpose: r.Purpose, Path: ref.Path, Scan: func(visit func([]byte) bool) error {
					return discoveryio.ScanRecords(ctx, r.Files, ref.Path, r.HeaderBytes, r.RecordBytes, visit)
				}})
				c.Header = header
				c.IdentityInspected = true
				c.IdentityError = err
				c.Session.NativeID = header.NativeID
			}
			return true, emit(c)
		})
		report.Enumerated += coverage.Enumerated
		report.UnreadableFolders += coverage.UnreadableFolders
		report.StoreUnreadable = report.StoreUnreadable || coverage.RootUnreadable
		report.Incomplete = report.Incomplete || !coverage.Complete
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// ProjectPaths demonstrates a native inventory declaration without probing paths.
func (p *Ports) ProjectPaths(e agentapi.NativePathEnvironment) agentapi.NativeProjectPaths {
	return agentapi.NativeProjectPaths{Worktrees: []string{filepath.Join(e.Locations.UserHome, ".orbifold", "landings")}}
}

// ReadManifestShards is intentionally limited to the caller's synthetic files.
// Production providers use their own bounded verified reads instead.
func ReadManifestShards(paths []string, open func(string) (io.ReadCloser, error)) ([][]byte, error) {
	var out [][]byte
	for _, path := range paths {
		f, err := open(path)
		if err != nil {
			return nil, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, archive.MaxRecordBytes+1))
		err = errors.Join(readErr, f.Close())
		if err != nil {
			return nil, err
		}
		if len(raw) > archive.MaxRecordBytes {
			return nil, archive.ErrRecordTooLarge
		}
		out = append(out, raw)
	}
	return out, nil
}
