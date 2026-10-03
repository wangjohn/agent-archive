package discovery

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"time"
)

func admit(store *state.Store, candidate Candidate, project, generation string, now time.Time) (archive.SessionRegistration, bool, error) {
	created, err := admitSession(store, candidate, project, generation, now)
	if err != nil {
		return archive.SessionRegistration{}, created, err
	}
	id, _, err := store.ArchiveSessionID(sessionKey(candidate.Agent, candidate.NativeSessionID))
	if err != nil {
		return archive.SessionRegistration{}, created, err
	}
	reg, _, err := store.LoadRegistration(id)
	return reg, created, err
}
