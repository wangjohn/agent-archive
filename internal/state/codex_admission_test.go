package state

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"testing"
	"time"
)

func TestCodexAdmissionProofCannotBeRetrofittedOrChanged(t *testing.T) {
	for _, proof := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "blanket"}[proof], func(t *testing.T) {
			store, e := Open(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native"}
			root := "/synthetic"
			reg, e := store.RegisterOrMerge(key, func(id string) archive.SessionRegistration {
				var admission *archive.CodexAdmissionProof
				if proof {
					admission = &archive.CodexAdmissionProof{Generation: "generation", Revision: "revision", Cwd: root}
				}
				r := archive.SessionRegistration{CodexAdmission: admission, ArchiveSessionID: id, NativeSessionID: key.NativeID, ProjectRoot: root, ProjectID: archive.ProjectID(root), Harness: archive.Harness{Name: "codex"}, Origin: archive.SessionOriginHook, SessionStartedAt: time.Now()}
				return r
			})
			if e != nil {
				t.Fatal(e)
			}
			_, e = store.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error {
				if proof {
					r.CodexAdmission.Generation = "changed"
				} else {
					r.CodexAdmission = &archive.CodexAdmissionProof{Generation: "generation", Revision: "revision", Cwd: root}
				}
				return nil
			})
			if e == nil {
				t.Fatal("proof mutation accepted")
			}
			replacement := reg
			if proof {
				replacement.CodexAdmission = nil
			} else {
				replacement.CodexAdmission = &archive.CodexAdmissionProof{Generation: "generation", Revision: "revision", Cwd: root}
			}
			if e = store.SaveRegistration(replacement); e == nil {
				t.Fatal("replacement changed immutable proof")
			}
			after, _, e := store.LoadRegistration(reg.ArchiveSessionID)
			if e != nil || (after.CodexAdmission != nil) != proof {
				t.Fatalf("proof changed %#v %v", after, e)
			}
			if proof && after.CodexAdmission.Generation != "generation" {
				t.Fatal("proof overwritten")
			}
		})
	}
}
