package collector

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Native links are derived from this pass's registration inventory. They do
// not enumerate native files or read published transcript state. Missing links
// never prevent independently admitted children from publishing.
type nativeLinkKey struct{ native, project, root, destination, home string }

func nativeRegistrationHome(reg archive.SessionRegistration) string {
	if reg.CodexBinding != nil && reg.CodexBinding.Home != "" {
		return reg.CodexBinding.Home
	}
	if reg.NativeSourceHome != "" {
		return reg.NativeSourceHome
	}
	return reg.DiscoveryRoot
}

func nativeParentKey(reg archive.SessionRegistration, native string) nativeLinkKey {
	return nativeLinkKey{native, reg.ProjectID, reg.ProjectRoot, reg.DestinationID, nativeRegistrationHome(reg)}
}

func (p *pass) reconcileNativeLinks() error {
	parents := make(map[nativeLinkKey]archive.SessionRegistration, len(p.registrations))
	ambiguous := map[nativeLinkKey]bool{}
	for _, reg := range p.registrations {
		if reg.Harness.Name != "codex" || nativeRegistrationHome(reg) == "" || p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		key := nativeParentKey(reg, reg.NativeSessionID)
		if prior, ok := parents[key]; ok && prior.ArchiveSessionID != reg.ArchiveSessionID {
			ambiguous[key] = true
		}
		parents[key] = reg
	}
	for i, reg := range p.registrations {
		if !reg.NativeChild || reg.ParentNativeSessionID == "" || reg.ParentSessionID != "" || nativeRegistrationHome(reg) == "" {
			continue
		}
		key := nativeParentKey(reg, reg.ParentNativeSessionID)
		parent, found := parents[key]
		if !found || ambiguous[key] || parent.ArchiveSessionID == reg.ArchiveSessionID {
			continue
		}
		// Removal records are authoritative even during a partial retention/undo.
		if _, removed, err := p.local.Removal("codex", parent.NativeSessionID); err != nil {
			return err
		} else if removed {
			continue
		}
		found, err := p.local.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
			if !current.NativeChild || current.ParentNativeSessionID != reg.ParentNativeSessionID || nativeParentKey(*current, current.ParentNativeSessionID) != key {
				return errors.New("native relationship changed")
			}
			if current.ParentSessionID != "" && current.ParentSessionID != parent.ArchiveSessionID {
				return errors.New("native parent link conflicts")
			}
			current.ParentSessionID = parent.ArchiveSessionID
			return nil
		})
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		reg.ParentSessionID = parent.ArchiveSessionID
		p.registrations[i] = reg
		if err := p.local.SaveRequest(reg.ArchiveSessionID, "native-parent-linked", p.now); err != nil {
			return err
		}
	}
	return nil
}
