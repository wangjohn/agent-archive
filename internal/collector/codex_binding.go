package collector

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// persistCodexBinding runs after native validation and keeps admission and
// provenance untouched. The caller already holds collector.lock; hooks.lock
// fences stale hooks and setup before installing irreversible writer protection.
func (s *sessionScan) persistCodexBinding(binding *archive.CodexSourceBinding) error {
	if binding == nil {
		return nil
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	if !binding.PreservesFacts(s.reg.CodexBinding) {
		return errors.New("Codex native binding facts changed")
	}
	before, err := json.Marshal(s.reg.CodexBinding)
	if err != nil {
		return err
	}
	after, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if string(before) == string(after) {
		return nil
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
		return errors.New("Codex binding requires current capture permission")
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
			return errors.New("Codex registration changed before binding")
		}
		if !binding.PreservesFacts(current.CodexBinding) {
			return errors.New("Codex binding facts changed before update")
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
		return errors.New("Codex registration disappeared before binding")
	}
	s.reg = updated
	return nil
}
