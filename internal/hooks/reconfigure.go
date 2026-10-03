package hooks

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

type setupHookOwner struct {
	file agentapi.HookFile
	port agentapi.HookConfigurator
	hook Hook
}

type setupHookDestination struct {
	before   agentapi.HookFile
	removals []setupHookOwner
	installs []setupHookOwner
}

// PlanReconfiguration composes retiring owners' pure removals before selected
// owners' installations. Each canonical atomic destination gets one change,
// bound to the original disk observation; hardlinks remain separate destinations.
func PlanReconfiguration(files, previous Files, install, removal Hook, selected, installed []string) ([]Change, error) {
	var destinations []setupHookDestination
	indices := map[string]int{}
	add := func(paths Files, hook Hook, name string, removing bool) error {
		file, port, _, err := observe(paths, hook, name)
		if err != nil {
			return err
		}
		key := local.CanonicalPath(file.Path)
		index, found := indices[key]
		if !found {
			index = len(destinations)
			indices[key] = index
			destinations = append(destinations, setupHookDestination{before: file})
		} else {
			before := destinations[index].before
			if before.Present != file.Present || before.Mode != file.Mode || !bytes.Equal(before.Bytes, file.Bytes) {
				return fmt.Errorf("hook settings changed while planning %s", file.Path)
			}
		}
		owner := setupHookOwner{file: file, port: port, hook: hook}
		if removing {
			destinations[index].removals = append(destinations[index].removals, owner)
		} else {
			destinations[index].installs = append(destinations[index].installs, owner)
		}
		return nil
	}
	for _, name := range selected {
		if err := add(files, install, name, false); err != nil {
			return nil, err
		}
	}
	for _, name := range installed {
		if slices.Contains(selected, name) && local.CanonicalPath(previous[name]) == local.CanonicalPath(files[name]) {
			continue
		}
		if err := add(previous, removal, name, true); err != nil {
			return nil, err
		}
	}
	var changes []Change
	for _, destination := range destinations {
		change, err := destination.plan()
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}
	return changes, nil
}

func (destination setupHookDestination) plan() (*Change, error) {
	if err := destination.validateRemovalAliases(); err != nil {
		return nil, err
	}
	current := cloneHookFile(destination.before)
	changed := false
	for _, owner := range destination.removals {
		change, err := owner.plan(current, agentapi.HookRemove)
		if err != nil {
			return nil, err
		}
		if change != nil {
			if change.Delete && len(destination.installs) == 0 && destination.hasRemovalAlias() {
				return nil, fmt.Errorf("hook owners have conflicting deletion plans for %s", owner.file.Path)
			}
			current = plannedHookFile(current, *change)
			changed = true
		}
	}
	// No retired owner may undo another's removal. Check using native ports,
	// rather than attempting to interpret or merge their settings bytes here.
	if len(destination.removals) > 1 {
		for _, owner := range destination.removals {
			change, err := owner.plan(current, agentapi.HookRemove)
			if err != nil {
				return nil, err
			}
			if change != nil && !sameHookContents(current, plannedHookFile(current, *change)) {
				return nil, fmt.Errorf("hook owners have conflicting removal plans for %s", owner.file.Path)
			}
		}
	}
	// The atomic destination stays on disk when a selected owner replaces it.
	// A virtual removal may discard its contents, but must not make native
	// installation apply creation defaults to an existing file's permissions.
	if !current.Present && destination.before.Present && len(destination.installs) > 0 {
		current.Present = true
		current.Regular = destination.before.Regular
	}
	var installation *Change
	for _, owner := range destination.installs {
		change, err := owner.plan(current, agentapi.HookInstall)
		if err != nil {
			return nil, err
		}
		if installation != nil && (installation.Delete != change.Delete || installation.Mode != change.Mode || installation.Existed != change.Existed || !bytes.Equal(installation.Before, change.Before) || !bytes.Equal(installation.After, change.After)) {
			return nil, fmt.Errorf("hook owners have conflicting installation plans for %s", owner.file.Path)
		}
		installation = change
	}
	if installation != nil {
		current = plannedHookFile(current, *installation)
		changed = true
	}
	if changed {
		change := Change{Path: destination.before.Path, Before: bytes.Clone(destination.before.Bytes), Existed: destination.before.Present, After: bytes.Clone(current.Bytes), Mode: current.Mode, Delete: !current.Present}
		action := agentapi.HookRemove
		if installation != nil {
			action = agentapi.HookInstall
		}
		if err := validateHookChange(destination.before, change, action); err != nil {
			return nil, err
		}
		return &change, nil
	}
	return nil, nil
}

func (destination setupHookDestination) hasRemovalAlias() bool {
	for _, owner := range destination.removals {
		if owner.file.Present && !owner.file.Regular {
			return true
		}
	}
	return false
}

// Check original deletion restrictions before sequential retirement can make
// another owner's original symlink observation disappear. Keep the same
// refusal regardless of which owner is visited first.
func (destination setupHookDestination) validateRemovalAliases() error {
	if len(destination.installs) > 0 || !destination.hasRemovalAlias() {
		return nil
	}
	for _, owner := range destination.removals {
		change, err := owner.plan(destination.before, agentapi.HookRemove)
		if err != nil {
			return err
		}
		if change != nil && change.Delete {
			return fmt.Errorf("hook owners have conflicting deletion plans for %s", owner.file.Path)
		}
	}
	return nil
}

func (owner setupHookOwner) plan(current agentapi.HookFile, action agentapi.HookAction) (*Change, error) {
	file := cloneHookFile(current)
	file.Path = owner.file.Path
	// A symlink remains a symlink through atomic replacement of its target.
	file.Regular = current.Present && (owner.file.Regular || !owner.file.Present)
	identity, err := resolveOwners(owner.port, file, owner.hook.owner())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file.Path, err)
	}
	changes, err := owner.port.Plan(agentapi.HookPlanRequest{Action: action, File: cloneHookFile(file), Owner: identity})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file.Path, err)
	}
	if action == agentapi.HookRemove && len(changes) == 0 {
		return nil, nil
	}
	if len(changes) != 1 {
		return nil, fmt.Errorf("hook action %d must plan one file", action)
	}
	change := changes[0]
	if err := validateHookChange(file, change, action); err != nil {
		return nil, err
	}
	change.Before = bytes.Clone(change.Before)
	change.After = bytes.Clone(change.After)
	return &change, nil
}

func plannedHookFile(before agentapi.HookFile, change Change) agentapi.HookFile {
	after := before
	after.Bytes = bytes.Clone(change.After)
	after.Mode = change.Mode
	after.Present = !change.Delete
	if change.Delete {
		after.Bytes = nil
		after.Regular = false
	} else if !before.Present {
		after.Regular = true
	}
	return after
}

func sameHookContents(a, b agentapi.HookFile) bool {
	return a.Present == b.Present && (!a.Present || (a.Mode == b.Mode && bytes.Equal(a.Bytes, b.Bytes)))
}
