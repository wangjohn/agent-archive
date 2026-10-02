package hooks

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/hookconfig"
	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/jsonedit"
	"github.com/wangjohn/agent-archive/internal/local"
)

var testPorts = builtin.NewBuiltins()

var errInvalidConfiguration = hookconfig.ErrInvalidConfiguration

func (h Hook) sameInstallation(home string) bool {
	if home == "" {
		home = h.DefaultDataHome
	}
	target := h.DataHome
	if target == "" {
		target = h.DefaultDataHome
	}
	return local.SameLocation(home, target)
}

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

// PlanRemoval prepares the inverse of Plan for uninstall: for each harness,
// a Change whose After is the current file with only hook's installation's
// handlers (and the prototype's) stripped (see Remove). A harness whose hook
// file is missing, or whose file never contained those, yields no Change at
// all, so an unrelated configuration, or one only another installation's
// hooks are in, is never rewritten or reformatted. Apply the result with
// Apply, which keeps its refuse-on-concurrent-edit and rollback behavior.
func PlanRemoval(files Files, hook Hook, harnesses []string) ([]Change, error) {
	changes := []Change{}
	for _, h := range harnesses {
		c, found, err := PlanRemovalOf(files, hook, h)
		if err != nil {
			return nil, err
		}
		if found {
			changes = append(changes, c)
		}
	}
	return changes, nil
}

func CommandDataHome(command string) (string, bool) { return hookconfig.CommandDataHome(command) }

func Merge(existing []byte, name string, h Hook) ([]byte, error) {
	p, err := h.port(name)
	if err != nil {
		return nil, err
	}
	file := agentapi.HookFile{Path: "/hook-settings", Bytes: existing, Present: len(existing) > 0, Mode: 0600, Regular: true}
	owner, err := resolveOwners(p, file, h.owner())
	if err != nil {
		return nil, err
	}
	changes, err := p.Plan(agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: file, Owner: owner})
	if err != nil {
		return nil, err
	}
	if len(changes) != 1 {
		return nil, errors.New("hook install must plan one file")
	}
	return changes[0].After, nil
}

func Remove(existing []byte, name string, h Hook) ([]byte, bool, error) {
	p, err := h.port(name)
	if err != nil {
		return nil, false, err
	}
	file := agentapi.HookFile{Path: "/hook-settings", Bytes: existing, Present: true, Mode: 0600, Regular: true}
	owner, err := resolveOwners(p, file, h.owner())
	if err != nil {
		return nil, false, err
	}
	changes, err := p.Plan(agentapi.HookPlanRequest{Action: agentapi.HookRemove, File: file, Owner: owner})
	if err != nil {
		return nil, false, err
	}
	if len(changes) == 0 {
		return existing, false, nil
	}
	return changes[0].After, true, nil
}

func Empty(data []byte) bool {
	d, err := jsonedit.Parse(data)
	return err == nil && len(d.Root.Members) == 0
}

func Applied(c Change) bool { return fileapply.Applied(c) }

func Unapplied(c Change) bool { return fileapply.Unapplied(c) }

func Rollback(c []Change) error { return fileapply.Rollback(c) }

func apply(c []Change, target func(string) (string, error)) error {
	return fileapply.ApplyWithTarget(c, target)
}

func resolveTarget(path string) (string, error) { return fileapply.ResolveTarget(path) }
