// Package schedulertest is what tests use in place of a real scheduler: a
// scheduler-level model (Model) that any code written against the port can
// run over, and the conformance suite (RunConformance) that every adapter
// passes over a fake Runner, so a new backend is done when it does. It is test
// code only; depguard keeps it out of production packages.
package schedulertest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// Model is a scheduler that is not launchd: a second implementation of the
// port, with a vocabulary of its own (refs are "model-...", a definition is a
// text file <user home>/.model/<ref>.job) and a manager that is a map. Code
// that is written against the port, and never names launchd, runs over it; the
// conformance suite runs over it too, which is what shows the suite asks
// nothing launchd-specific.
//
// It is also the Manager the suite drives it through (Put, Held, Calls).
type Model struct {
	mu    sync.Mutex
	held  map[key]held
	calls []string
}

type key struct {
	site string
	ref  scheduler.Ref
}

// held is what the model's manager says about a job.
type held struct {
	state      scheduler.JobState
	loadedFrom string
}

// NewModel is a model with no job loaded.
func NewModel() *Model { return &Model{held: map[key]held{}} }

// Name is "model".
func (*Model) Name() string { return "model" }

// Words are the model's nouns.
func (*Model) Words() scheduler.Words {
	return scheduler.Words{Manager: "the model", Job: "model job", Definition: "job file", Tool: "modelctl"}
}

// DefaultPATH is the PATH the model gives a job whose definition sets none.
func (*Model) DefaultPATH() string { return "/model/bin" }

// Ref is "model-default" for the default installation and otherwise "model-"
// and the first 8 hex digits of the SHA-256 of the data directory.
func (*Model) Ref(inst scheduler.Installation) scheduler.Ref {
	if inst.Default {
		return "model-default"
	}
	sum := sha256.Sum256([]byte(filepath.Clean(inst.DataHome)))
	return scheduler.Ref("model-" + hex.EncodeToString(sum[:])[:8])
}

func definitionPath(site scheduler.Site, ref scheduler.Ref) string {
	return filepath.Join(site.UserHome, ".model", string(ref)+".job")
}

// Locate is the site and the job of a definition path: <user home>/.model/<ref>.job.
func (*Model) Locate(definition string) (scheduler.Site, scheduler.Ref, error) {
	site := scheduler.Site{UserHome: filepath.Dir(filepath.Dir(definition))}
	ref, ok := strings.CutSuffix(filepath.Base(definition), ".job")
	if !ok || definitionPath(site, scheduler.Ref(ref)) != definition {
		return site, "", fmt.Errorf("%s is not a model job file (<home>/.model/<ref>.job)", definition)
	}
	return site, scheduler.Ref(ref), nil
}

// Plan is the definition of spec: one file, lines of `key=value`.
func (m *Model) Plan(site scheduler.Site, inst scheduler.Installation, spec scheduler.JobSpec) (scheduler.Plan, error) {
	if !filepath.IsAbs(spec.Executable) || !filepath.IsAbs(spec.DataHome) {
		return scheduler.Plan{}, errors.New("the model job's paths must be absolute")
	}
	ref := m.Ref(inst)
	var b strings.Builder
	fmt.Fprintf(&b, "job=%s\nprogram=%s\nargs=%s\ndata=%s\ninterval=%s\nrun_at_load=%v\n", ref, spec.Executable, strings.Join(spec.Args, " "), spec.DataHome, spec.Interval, spec.RunAtLoad)
	for _, name := range slices.Sorted(maps.Keys(spec.Env)) {
		if name == "" || name == "AGENT_ARCHIVE_HOME" || strings.ContainsAny(name+spec.Env[name], "=\n") {
			return scheduler.Plan{}, fmt.Errorf("the model job cannot set %q", name)
		}
		fmt.Fprintf(&b, "env.%s=%s\n", name, spec.Env[name])
	}
	return scheduler.Plan{Ref: ref, Artifacts: []scheduler.Artifact{scheduler.FileArtifact(definitionPath(site, ref), []byte(b.String()), 0o600)}}, nil
}

// Definition reads the definition file alone.
func (*Model) Definition(site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	path := definitionPath(site, ref)
	status := scheduler.Status{Paths: []string{path}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return status
	}
	status.Defined = true
	if err != nil {
		status.DefinitionErr = err
		return status
	}
	if !strings.HasPrefix(string(data), "job=") {
		status.DefinitionErr = errors.New("not a model job file")
		return status
	}
	status.Env = map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		name, value, ok := strings.Cut(line, "=")
		switch {
		case !ok:
		case name == "program":
			status.Program = value
		case name == "data":
			status.DataHome = value
		case strings.HasPrefix(name, "env."):
			status.Env[strings.TrimPrefix(name, "env.")] = value
		}
	}
	return status
}

// Inspect is Definition and what the model's manager holds for the job.
func (m *Model) Inspect(_ context.Context, site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	m.record("inspect " + string(ref))
	status := m.Definition(site, ref)
	status.State = m.Held(site, ref)
	expected := definitionPath(site, ref)
	switch status.State {
	case scheduler.AnotherInstallation:
		status.Problem = &scheduler.Problem{Kind: scheduler.ProblemNotOwned, Ref: ref, LoadedFrom: m.heldJob(site, ref).loadedFrom, Expected: expected, Fix: "Remove the other installation's model job"}
	case scheduler.Unknown:
		status.Problem = &scheduler.Problem{Kind: scheduler.ProblemCannotTell, Ref: ref, Expected: expected, Fix: "Check that modelctl works"}
	case scheduler.Loaded, scheduler.Running, scheduler.Missing:
	}
	return status
}

// Installed is the model's own job first, then every other job of the same
// data directory in the model's folder, as earlier labels.
func (m *Model) Installed(_ context.Context, site scheduler.Site, inst scheduler.Installation) ([]scheduler.Job, error) {
	own := m.Ref(inst)
	jobs := []scheduler.Job{{Ref: own}}
	entries, _ := os.ReadDir(filepath.Dir(definitionPath(site, own))) // no folder, no earlier jobs
	for _, entry := range entries {
		ref, ok := strings.CutSuffix(entry.Name(), ".job")
		if !ok || scheduler.Ref(ref) == own {
			continue
		}
		if status := m.Definition(site, scheduler.Ref(ref)); status.Defined && status.DefinitionErr == nil && status.DataHome == inst.DataHome {
			jobs = append(jobs, scheduler.Job{Ref: scheduler.Ref(ref), Alias: scheduler.EarlierLabel})
		}
	}
	return jobs, nil
}

// Load loads the definition on disk.
func (m *Model) Load(_ context.Context, site scheduler.Site, ref scheduler.Ref) error {
	m.record("load " + string(ref))
	path := definitionPath(site, ref)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("modelctl load: %w", err)
	}
	if state := m.Held(site, ref); state != scheduler.Missing {
		return fmt.Errorf("modelctl load: the job is already %s", state)
	}
	m.set(site, ref, held{state: scheduler.Loaded, loadedFrom: path})
	return nil
}

// Unload stops the job when it was loaded from this site's definition.
func (m *Model) Unload(_ context.Context, site scheduler.Site, ref scheduler.Ref) error {
	m.record("unload " + string(ref))
	words := m.Words()
	problem := scheduler.Problem{Ref: ref, Expected: definitionPath(site, ref)}
	switch m.Held(site, ref) {
	case scheduler.Loaded, scheduler.Running:
		m.set(site, ref, held{state: scheduler.Missing})
		return nil
	case scheduler.Missing:
		return nil
	case scheduler.AnotherInstallation:
		problem.Kind, problem.LoadedFrom = scheduler.ProblemNotOwned, m.heldJob(site, ref).loadedFrom
		return &scheduler.NotOwnedError{Words: words, Problem: problem}
	case scheduler.Unknown:
	}
	problem.Kind = scheduler.ProblemCannotTell
	return &scheduler.IndeterminateError{Words: words, Problem: problem}
}

// Put makes the model's manager report the job ref at site as state. A job
// another installation owns is loaded from a file of that installation's.
func (m *Model) Put(site scheduler.Site, ref scheduler.Ref, state scheduler.JobState) {
	h := held{state: state}
	switch state {
	case scheduler.Loaded, scheduler.Running:
		h.loadedFrom = definitionPath(site, ref)
	case scheduler.AnotherInstallation:
		h.loadedFrom = filepath.Join(string(filepath.Separator), "elsewhere", string(ref)+".job")
	case scheduler.Missing, scheduler.Unknown:
	}
	m.set(site, ref, h)
}

// Held is the state the manager holds for the job: Missing for one it was
// never given.
func (m *Model) Held(site scheduler.Site, ref scheduler.Ref) scheduler.JobState {
	return m.heldJob(site, ref).state
}

func (m *Model) heldJob(site scheduler.Site, ref scheduler.Ref) held {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.held[key{site.UserHome, ref}]; ok {
		return h
	}
	return held{state: scheduler.Missing}
}

func (m *Model) set(site scheduler.Site, ref scheduler.Ref, h held) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held[key{site.UserHome, ref}] = h
}

func (m *Model) record(call string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
}

// Calls is every question and change the manager was asked, in order, as
// "inspect REF", "load REF" and "unload REF".
func (m *Model) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}
