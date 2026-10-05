package agentapi

import (
	"github.com/wangjohn/agent-archive/internal/filechange"
	"io/fs"
)

// HookLocations supplies observed native environment locations without host calls.
type HookLocations struct {
	UserHome         string
	WorkingDirectory string
	Environment      map[string]string
}

// HookOwner identifies the installation whose handlers may be changed.
type HookOwner struct {
	Executable      string
	DataHome        string
	DefaultDataHome string
	Locations       map[string]string
}

// HookFile is a bounded caller observation, including symlink and permission facts.
type HookFile struct {
	Path      string
	Bytes     []byte
	Present   bool
	ReadError error
	Mode      fs.FileMode
	Regular   bool
}

// HookAction selects installation or removal.
type HookAction uint8

// The following values define the supported typed observations.
const (
	HookInstall HookAction = iota + 1
	HookRemove
)

// HookPlanRequest contains every observation needed by a pure configurator.
type HookPlanRequest struct {
	Action HookAction
	File   HookFile
	Owner  HookOwner
}

// HookInspectionRequest is separate from mutation planning.
type HookInspectionRequest struct {
	File  HookFile
	Owner HookOwner
}

// HookState distinguishes missing, managed, foreign and unreadable configuration.
type HookState uint8

// The following values define the supported typed observations.
const (
	HookAbsent HookState = iota
	HookOwned
	HookForeign
	HookUnreadable
)

// HookOtherOwner is safe local ownership detail used by setup and status.
type HookOtherOwner struct {
	DataHome string
	Default  bool
	Command  string
}

// HookInspection describes settings without performing reads or writes.
type HookInspection struct {
	// Owned reports any handlers of this installation, independently of foreign
	// ownership and whether its required event handlers are complete.
	Owned              bool
	State              HookState
	Installed          bool
	Reason             string
	Others             []HookOtherOwner
	OwnershipLocations []string
}

// HookConfigurator owns native paths, layout, event names and ownership rules.
// Every method is pure; the caller supplies existing bytes and resolved identities.
type HookConfigurator interface {
	Location(HookLocations) string
	EnvironmentKeys() []string
	Plan(HookPlanRequest) ([]filechange.Change, error)
	Inspect(HookInspectionRequest) (HookInspection, error)
}

// HooksLookup supplies only hook configuration ports to shared setup orchestration.
type HooksLookup interface {
	LookupHooks(string) (HookConfigurator, bool)
	HookAgents() []string
}
