package collector

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestPublicationCarriesTheReplayMarker(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	reg.Replay = &archive.Replay{RunID: "run-7"}
	opts := Options{MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	if got := publishOnce(t, local, storagetest.NewMemoryStore(), reg, &opts); got.Replay == nil || got.Replay.RunID != "run-7" {
		t.Errorf("replay = %+v, want the registration's", got.Replay)
	}
}

// A replay's subagents are replays too, so hiding replays hides them all.
func TestSubagentRegistrationCopiesTheParentsReplayMarker(t *testing.T) {
	t.Parallel()
	parent := archive.SessionRegistration{
		ArchiveSessionID: "parent", NativeSessionID: "native-parent", ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "claude"}, Replay: &archive.Replay{RunID: "run-7"},
	}
	child := assembleSubagentRegistration(parent, state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-parent:subagent:a", AgentID: "a"})
	if child.Replay == nil || child.Replay.RunID != "run-7" {
		t.Errorf("subagent replay = %+v, want its parent's", child.Replay)
	}
}
