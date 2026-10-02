// Package hooks supplies host observations and applies generic file plans.
package hooks

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"io"
	"os"
)

// Owner is the common installation marker retained by native configurators.
const Owner = "agent-archive lifecycle capture"

// Hook identifies an installation and its injected configurator lookup.
type Hook struct {
	Executable      string
	DataHome        string
	DefaultDataHome string
	Ports           agentapi.HooksLookup
}

func (h Hook) owner() agentapi.HookOwner {
	return agentapi.HookOwner{Executable: h.Executable, DataHome: h.DataHome, DefaultDataHome: h.DefaultDataHome}
}

func (h Hook) port(name string) (agentapi.HookConfigurator, error) {
	if h.Ports == nil {
		return nil, errors.New("hook configurator lookup required")
	}
	p, ok := h.Ports.LookupHooks(name)
	if !ok {
		return nil, errors.New("unsupported harness")
	}
	return p, nil
}

// Command delegates native command construction without applying or executing it.
func (h Hook) Command(name string) (string, error) {
	p, err := h.port(name)
	if err != nil {
		return "", err
	}
	c, ok := p.(interface {
		Command(agentapi.HookOwner) (string, error)
	})
	if !ok {
		return "", errors.New("command unavailable")
	}
	return c.Command(h.owner())
}

const maxHookSettingsBytes = 8 << 20

func readSettings(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxHookSettingsBytes+1))
	if err == nil && len(data) > maxHookSettingsBytes {
		err = errors.New("hook settings exceed bounded input")
	}
	return data, err
}

func observe(files Files, h Hook, name string) (agentapi.HookFile, agentapi.HookConfigurator, agentapi.HookOwner, error) {
	p, err := h.port(name)
	if err != nil {
		return agentapi.HookFile{}, nil, agentapi.HookOwner{}, err
	}
	path, err := files.path(name)
	if err != nil {
		return agentapi.HookFile{}, nil, agentapi.HookOwner{}, err
	}
	data, readErr := readSettings(path)
	present := readErr == nil
	var problem error
	mode := os.FileMode(0600)
	regular := false
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		problem = readErr
	}
	if present {
		info, e := os.Stat(path)
		if e != nil {
			problem = e
		} else {
			mode = info.Mode().Perm()
		}
		link, e := os.Lstat(path)
		if e != nil {
			problem = e
		} else {
			regular = link.Mode().IsRegular()
		}
	}
	file := agentapi.HookFile{Path: path, Bytes: data, Present: present, ReadError: problem, Mode: mode, Regular: regular}
	if problem != nil {
		return file, p, h.owner(), &readError{path: path, cause: problem}
	}
	owner, e := resolveOwners(p, file, h.owner())
	if e != nil {
		e = fmt.Errorf("%s: %w", path, e)
	}
	return file, p, owner, e
}

func resolveOwners(p agentapi.HookConfigurator, file agentapi.HookFile, owner agentapi.HookOwner) (agentapi.HookOwner, error) {
	first, err := p.Inspect(agentapi.HookInspectionRequest{File: file, Owner: owner})
	if err != nil {
		return owner, err
	}
	owner.Locations = map[string]string{}
	names := append([]string{owner.DataHome, owner.DefaultDataHome}, first.OwnershipLocations...)
	if len(names) > maxHookSettingsBytes {
		return owner, errors.New("too many ownership references")
	}
	for i, name := range names {
		if name == "" {
			continue
		}
		key := local.CanonicalPath(name)
		for _, prior := range names[:i] {
			if prior != "" && local.SameLocation(name, prior) {
				key = owner.Locations[prior]
				break
			}
		}
		owner.Locations[name] = key
	}
	return owner, nil
}
