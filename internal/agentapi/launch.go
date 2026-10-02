// Package agentapi defines integration ports consumed by shared orchestration.
package agentapi

// LaunchRequest supplies native argv inputs; implementations perform no I/O.
type LaunchRequest struct {
	ProjectDir  string
	Prompt      string
	HandoffPath string
	ExtraArgs   []string
}

// Executables describes ordered executable candidates and installation guidance.
type Executables struct {
	Names   []string
	Install string
}

// Launcher interprets native argv rules; the caller owns process and environment.
type Launcher interface {
	Executables() Executables
	Args(LaunchRequest) ([]string, error)
}
