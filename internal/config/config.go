// Package config is the single local, durable record of how this machine's
// agent-archive is set up: which storage destination it publishes to, which
// projects are included, and whether collection is currently paused. It is
// the one file `agent-archive setup` writes and every other command reads.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// SchemaVersion is bumped only when Config's on-disk shape changes
// incompatibly.
const SchemaVersion = 1

// Config is this machine's complete archive configuration. It contains no
// secrets: R2 secrets live in Keychain (see credentials.Config.R2CredentialRef)
// and S3 credentials are resolved through the named AWS profile.
type Config struct {
	BucketPrivacy         *storage.PrivacyReport `json:"bucket_privacy,omitempty"`
	RetiredCredentialRefs []string               `json:"retired_credential_refs,omitempty"`
	StorageVerifiedAt     time.Time              `json:"storage_verified_at,omitempty"`
	DestinationSince      time.Time              `json:"destination_since,omitempty"`
	PreviousDestinations  []credentials.Config   `json:"previous_destinations,omitempty"`

	SchemaVersion int                `json:"schema_version"`
	MachineID     string             `json:"machine_id"`
	Storage       credentials.Config `json:"storage"`
	Archive       archive.Config     `json:"archive"`
	// Paused persistently suspends collection, uploads, and remote cleanup
	// without deleting data or existing configuration.
	Paused bool `json:"paused"`
	// Harnesses lists which applications setup installed hooks for
	// (values match archive.Harness.Name: "codex", "claude", "cursor").
	Harnesses []string `json:"harnesses,omitempty"`
	// ImportedHarnesses lists apps whose sessions were imported by
	// `agent-archive backfill` without their hooks installed. It admits those
	// apps' imported sessions only; hook registrations still need Harnesses.
	// Backfill adds to it and setup carries it over from the committed
	// configuration, dropping an app once its hooks are installed.
	ImportedHarnesses []string `json:"imported_harnesses,omitempty"`
	// InstalledExecutable is the executable path setup wrote into the hooks
	// and the LaunchAgent. Status checks the installed hooks against this
	// path rather than whichever path status itself was run through (a
	// symlink, a Homebrew shim, a copied binary); a configuration written
	// before this field existed falls back to the running executable.
	InstalledExecutable string `json:"installed_executable,omitempty"`
	// RequireSkillUse opts out of the spec's default (capture sessions with
	// no detected skill use too, to preserve comparison evidence). The zero
	// value (false) matches that default, so a config that predates this
	// field, or one built without setting it, behaves correctly rather than
	// silently declining everything.
	RequireSkillUse bool `json:"require_skill_use"`
	// RetentionDays is whole-session retention, proposed as 90 by setup.
	// Enforcing it is the Retention slice's job, not this package's.
	RetentionDays int `json:"retention_days"`
}

func path(home string) string { return filepath.Join(home, "config.json") }

// Load reads this machine's configuration. found is false, with a nil error,
// when setup has never run.
func Load(home string) (cfg Config, found bool, err error) {
	err = local.Read(path(home), &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	return cfg, true, nil
}

// Save durably writes cfg, replacing any prior configuration atomically.
func Save(home string, cfg Config) error {
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = SchemaVersion
	}
	return local.Write(path(home), cfg)
}

// SetPaused updates only the Paused flag, preserving the rest of an existing
// configuration. It fails if setup has not run yet: pausing before there is
// anything to pause is not a meaningful state.
func SetPaused(home string, paused bool) (Config, error) {
	cfg, found, err := Load(home)
	if err != nil {
		return Config{}, err
	}
	if !found {
		return Config{}, errors.New("not set up yet; run `agent-archive setup` first")
	}
	cfg.Paused = paused
	if err := Save(home, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// AcceptSession prevents excluded apps/projects and previous destinations from
// continuing to publish or delete sessions after reconfiguration. Both time
// boundaries compare the registration's admission, never its start: an
// imported session began long before the project was activated, and is
// admitted by the import itself.
func (c Config) AcceptSession(r archive.SessionRegistration) bool {
	admitted := r.Admitted()
	if !c.DestinationSince.IsZero() && admitted.Before(c.DestinationSince) {
		return false
	}
	if !c.acceptsHarness(r) {
		return false
	}
	for _, p := range c.Archive.Projects {
		if p.Included && p.Root == r.ProjectRoot {
			return p.ActivatedAt.IsZero() || !admitted.Before(p.ActivatedAt)
		}
	}
	return len(c.Archive.Projects) == 0 // older programmatic configurations
}

// InCurrentDestination reports whether a registration published to the
// storage destination configured now, rather than to one it replaced. Like
// AcceptSession it compares the admission: an import published here even
// though it started before this destination was configured.
func (c Config) InCurrentDestination(r archive.SessionRegistration) bool {
	return c.DestinationSince.IsZero() || !r.Admitted().Before(c.DestinationSince)
}

// acceptsHarness reports whether the registration's app may publish. Hook
// registrations need the app's hooks (Harnesses); an import may also come
// from an app that was imported without hooks (ImportedHarnesses). An empty
// Harnesses list is an older programmatic configuration and admits every app.
func (c Config) acceptsHarness(r archive.SessionRegistration) bool {
	if len(c.Harnesses) == 0 || slices.Contains(c.Harnesses, r.Harness.Name) {
		return true
	}
	return r.Imported() && slices.Contains(c.ImportedHarnesses, r.Harness.Name)
}
