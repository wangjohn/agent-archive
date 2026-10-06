package collector

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// persistCodexBinding runs after native validation and keeps admission and
// provenance untouched. The caller already holds collector.lock; hooks.lock
// fences stale hooks and setup before installing irreversible writer protection.
func (s *sessionScan) persistCodexBinding(binding *archive.CodexSourceBinding) error {
	if binding == nil {
		return nil
	}
	changed, err := s.nativeBindingChanged(binding)
	if err != nil || !changed {
		return err
	}

	canonicalCwd := ""
	if s.reg.CodexAdmission != nil {
		selectedCwd := binding.SelectedCwd
		if selectedCwd == "" {
			selectedCwd = binding.Cwd
		}
		canonicalCwd, err = canonicalBindingCwd(selectedCwd)
		if err != nil {
			return err
		}
	}
	home := s.local.Home()
	unlock, err := local.NamedLockWait(home, "hooks.lock", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	if setupjournal.TransactionPending(home) {
		return errors.New("setup pending before history writer protection")
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	if !found || cfg.Paused || !cfg.AcceptSession(s.reg) {
		return errors.New("codex binding requires current capture permission")
	}
	if s.reg.CodexAdmission != nil {
		if !cfg.CodexContinuationAllowed(s.reg.ProjectRoot, canonicalCwd) {
			return errors.New("native cwd is outside current Codex continuation permission")
		}
	}
	if !cfg.CodexHistoryProtection {
		cfg.CodexHistoryProtection = true
		if err := config.Save(home, cfg); err != nil {
			return err
		}
	}
	var updated archive.SessionRegistration
	found, err = s.local.UpdateRegistration(s.id(), func(current *archive.SessionRegistration) error {
		if current.NativeSessionID != s.reg.NativeSessionID || !cfg.AcceptSession(*current) {
			return errors.New("codex registration changed before binding")
		}
		if !binding.PreservesFacts(current.CodexBinding) {
			return errors.New("codex binding facts changed before update")
		}
		if err := applyNativeChildBinding(current, binding); err != nil {
			return err
		}
		current.CodexBinding = binding
		current.TranscriptPath = binding.Path
		updated = *current
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("codex registration disappeared before binding")
	}
	s.reg = updated
	return nil
}

// canonicalBindingCwd preserves the spelling below an existing ancestor when a
// native worktree has vanished. It is used only during migration, outside locks.
func canonicalBindingCwd(path string) (string, error) {
	path = filepath.Clean(path)
	var missing []string
	for range 64 {
		canonical, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, missing[i])
			}
			return canonical, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
	return "", errors.New("native cwd ancestor limit exceeded")
}

func nativeBindingRelationshipsMatch(reg archive.SessionRegistration, binding *archive.CodexSourceBinding) bool {
	if !binding.Child {
		return true
	}
	return reg.NativeChild && (binding.ParentID == "" || reg.ParentNativeSessionID == binding.ParentID) && (binding.RootID == "" || reg.NativeRootSessionID == binding.RootID) && (binding.Home == "" || reg.NativeSourceHome == binding.Home)
}

func applyNativeChildBinding(reg *archive.SessionRegistration, binding *archive.CodexSourceBinding) error {
	if !binding.Child {
		return nil
	}
	if reg.ParentNativeSessionID != "" && binding.ParentID != "" && reg.ParentNativeSessionID != binding.ParentID {
		return errors.New("native child parent facts conflict")
	}
	if reg.NativeRootSessionID != "" && binding.RootID != "" && reg.NativeRootSessionID != binding.RootID {
		return errors.New("native child root facts conflict")
	}
	reg.NativeChild = true
	if binding.ParentID != "" {
		reg.ParentNativeSessionID = binding.ParentID
	}
	if binding.RootID != "" {
		reg.NativeRootSessionID = binding.RootID
	}
	if binding.Home != "" {
		reg.NativeSourceHome = binding.Home
	}
	return nil
}

func (s *sessionScan) nativeBindingChanged(binding *archive.CodexSourceBinding) (bool, error) {
	if err := binding.Validate(); err != nil {
		return false, err
	}
	if s.reg.CodexBinding == nil && s.reg.CodexAdmission != nil {
		cwd, err := canonicalBindingCwd(binding.Cwd)
		if err != nil {
			return false, err
		}
		if cwd != s.reg.CodexAdmission.Cwd {
			facts, known := sourcefacts.PhysicalProject(binding.Cwd)
			if !known || facts.Root != s.reg.ProjectRoot {
				return false, errors.New("native cwd contradicts admitted physical project evidence")
			}
		}
	}
	if !binding.PreservesFacts(s.reg.CodexBinding) {
		return false, errors.New("codex native binding facts changed")
	}
	before, err := json.Marshal(s.reg.CodexBinding)
	if err != nil {
		return false, err
	}
	after, err := json.Marshal(binding)
	if err != nil {
		return false, err
	}
	if string(before) == string(after) && nativeBindingRelationshipsMatch(s.reg, binding) {
		return false, nil
	}
	return true, nil
}
