package capture

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"os"
	"path/filepath"
	"testing"
)

func TestFuturePublishedSubagentOwnerGrantsNoPublishedLink(t *testing.T) {
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "native-parent", ProjectID: "project", ProjectRoot: "/synthetic/project", Harness: archive.Harness{Name: "claude"}}
	child := parent
	child.ArchiveSessionID = "child"
	child.NativeSessionID = "native-child"
	child.ParentSessionID = "parent"
	child.ParentNativeSessionID = "native-parent"
	child.SubagentID = "agent"
	if err = local.Write(filepath.Join(home, "registrations", "child.json"), child); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "published", "child.json"), []byte(`{"commit":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, status, owned, err := existingSubagentOwner(store, config.Config{}, parent, "child", "agent", archive.LinkedSessionPending)
	if !errors.Is(err, state.ErrDurableStorageRecovery) || owned || status == archive.LinkedSessionPublished {
		t.Fatalf("refused owner granted link: %s %v %v", status, owned, err)
	}
}
