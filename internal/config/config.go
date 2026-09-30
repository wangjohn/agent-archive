// Package config is the single local, durable record of how this machine's
// agent-archive is set up: which storage destination it publishes to, which
// projects are included, and whether collection is currently paused. It is
// the one file `agent-archive setup` writes and every other command reads.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// SchemaVersion is bumped only when Config's on-disk shape changes
// incompatibly.
const SchemaVersion = 1

// SkillEvidence controls how much filesystem skill data a source bundle carries.
type SkillEvidence string

const (
	// SkillEvidenceNone omits filesystem skill inventory and snapshots.
	SkillEvidenceNone SkillEvidence = "none"
	// SkillEvidenceMetadata includes names and hashes, without skill bodies.
	SkillEvidenceMetadata SkillEvidence = "metadata"
	// SkillEvidenceBody includes filtered snapshots as well as metadata.
	SkillEvidenceBody SkillEvidence = "body"
)

// EffectiveSkillEvidence preserves the behavior of configs saved before this
// setting existed. Fresh setup persists metadata explicitly.
func (c Config) EffectiveSkillEvidence() SkillEvidence {
	if c.SkillEvidence == "" {
		return SkillEvidenceBody
	}
	return c.SkillEvidence
}

// ValidSkillEvidence reports whether mode is one of the supported policies.
func ValidSkillEvidence(mode SkillEvidence) bool {
	switch mode {
	case SkillEvidenceNone, SkillEvidenceMetadata, SkillEvidenceBody:
		return true
	}
	return false
}

// Config is this machine's complete archive configuration. It contains no
// secrets: R2 secrets live in the credential store (the Keychain on macOS, a
// private file elsewhere; see credentials.Config.R2CredentialRef)
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
	// DeclinedHarnesses lists apps the user chose to leave out when
	// reconfiguring: declined when setup offered them as found on this
	// computer, or removed from the selection. Setup does not offer them
	// again; an app leaves this list once it is included.
	DeclinedHarnesses []string `json:"declined_harnesses,omitempty"`
	// InstalledExecutable is the executable path setup wrote into the hooks
	// and the LaunchAgent. Status checks the installed hooks against this
	// path rather than whichever path status itself was run through (a
	// symlink, a Homebrew shim, a copied binary); a configuration written
	// before this field existed falls back to the running executable.
	InstalledExecutable string `json:"installed_executable,omitempty"`
	// HookFiles records, per app in Harnesses, the hook configuration file
	// setup installed into, as resolved from the environment setup ran in
	// (CLAUDE_CONFIG_DIR, CODEX_HOME). Status and uninstall read it so they
	// find the files from a shell without those variables. A configuration
	// written before this field existed falls back to the current
	// environment's paths.
	HookFiles map[string]string `json:"hook_files,omitempty"`
	// RequireSkillUse opts out of the spec's default (capture sessions with
	// no detected skill use too, to preserve comparison evidence). The zero
	// value (false) matches that default, so a config that predates this
	// field, or one built without setting it, behaves correctly rather than
	// silently declining everything.
	RequireSkillUse bool `json:"require_skill_use"`
	// SkillEvidence controls filesystem skill inventory and snapshot uploads.
	// An absent field is a legacy body policy, not a new-install default.
	SkillEvidence SkillEvidence `json:"skill_evidence,omitempty"`
	// NoSkills records that the person opted out of the agent skills setup
	// installs into their coding agents (setup --no-skills). While it is set,
	// setup installs none and removes the ones it wrote earlier; setup
	// --skills clears it. The zero value installs them, so a configuration
	// from before this field behaves as it did.
	NoSkills bool `json:"no_skills,omitempty"`
	// RetentionDays is whole-session retention, proposed as 90 by setup.
	// Enforcing it is the Retention slice's job, not this package's.
	RetentionDays int `json:"retention_days"`
	// Handoff holds preferences for `agent-archive handoff`. Setup never
	// asks for them; they are edited by hand and carried through
	// reconfiguration like the rest of this file.
	Handoff HandoffConfig `json:"handoff,omitzero"`
}

// HandoffConfig is what `agent-archive handoff` launches with by default.
// Agents are named as harnesses are: claude, codex, cursor.
type HandoffConfig struct {
	// Args are arguments given to an agent before any after `--` on the
	// command line, keyed by the destination agent.
	Args map[string][]string `json:"args,omitempty"`
	// DefaultTo is the destination offered first, keyed by the harness of
	// the session being handed off.
	DefaultTo map[string]string `json:"default_to,omitempty"`
}

// handoffAgents are the agents a handoff can come from or go to.
var handoffAgents = []string{"claude", "codex", "cursor"}

// validate rejects names handoff would not recognize, so a typo in a
// hand-edited file is reported rather than silently ignored.
func (h HandoffConfig) validate() error {
	for agent, args := range h.Args {
		if !slices.Contains(handoffAgents, agent) {
			return fmt.Errorf("handoff.args: unknown agent %q; use claude, codex, or cursor", agent)
		}
		for _, arg := range args {
			// exec cannot pass a NUL byte, and an empty word is almost
			// always a quoting mistake.
			if arg == "" || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("handoff.args.%s: every argument must be non-empty text without a NUL byte", agent)
			}
		}
	}
	for source, dest := range h.DefaultTo {
		if !slices.Contains(handoffAgents, source) {
			return fmt.Errorf("handoff.default_to: unknown harness %q; use claude, codex, or cursor", source)
		}
		if !slices.Contains(handoffAgents, dest) {
			return fmt.Errorf("handoff.default_to.%s: unknown agent %q; use claude, codex, or cursor", source, dest)
		}
	}
	return nil
}

func path(home string) string { return filepath.Join(home, "config.json") }

// Load reads this machine's configuration. found is false, with a nil error,
// when setup has never run. An error names the file, and for one that no
// longer decodes, the way out: every command needs it, so nothing else can
// say which file stopped it.
func Load(home string) (cfg Config, found bool, err error) {
	err = local.Read(path(home), &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return Config{}, false, fmt.Errorf("%w: %s (%w). Restore it from a backup, or fix the JSON by hand; moving it aside (keep the copy: it records this Mac's machine ID) and running agent-archive setup configures this Mac again", ErrUnreadable, path(home), err)
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("read %s: %w", path(home), err)
	}
	if !ValidSkillEvidence(cfg.EffectiveSkillEvidence()) {
		return Config{}, false, fmt.Errorf("read %s: unsupported skill_evidence %q; choose none, metadata, or body", path(home), cfg.SkillEvidence)
	}
	if err := cfg.Handoff.validate(); err != nil {
		return Config{}, false, fmt.Errorf("read %s: %w", path(home), err)
	}
	return cfg, true, nil
}

// ErrUnreadable is a configuration file that exists but does not decode.
var ErrUnreadable = errors.New("the settings file cannot be read")

// Save durably writes cfg, replacing any prior configuration atomically.
func Save(home string, cfg Config) error {
	if !ValidSkillEvidence(cfg.EffectiveSkillEvidence()) {
		return fmt.Errorf("unsupported skill_evidence %q; choose none, metadata, or body", cfg.SkillEvidence)
	}
	if err := cfg.Handoff.validate(); err != nil {
		return err
	}
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

// DestinationID identifies a storage destination by its provider, endpoint,
// bucket, and prefix. It never covers credentials or their references. It
// lives here rather than in package archive, which imports no other internal
// package: taking a credentials.Config would pull the AWS SDK and cgo into
// archive.
//
// Registrations and batch files store this value, and it decides which bucket
// owns a session. It must never change, not the provider or endpoint
// normalisation, the join, nor the prefix trimming, without a migration of
// every stored ID: otherwise every registration silently belongs to no
// destination. TestDestinationIDIsPinned holds it fixed.
func DestinationID(c credentials.Config) string {
	// The provider is compared as storage compares it, case- and
	// space-insensitively; setup always writes it lowercase.
	provider := strings.ToLower(strings.TrimSpace(c.Provider))
	endpoint := ""
	if provider == credentials.ProviderR2 {
		endpoint, _ = credentials.R2Endpoint(c.R2Endpoint, c.R2AccountID)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{provider, endpoint, c.Bucket, strings.Trim(c.Prefix, "/")}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// DestinationID is the ID of the storage destination configured now.
func (c Config) DestinationID() string { return DestinationID(c.Storage) }

// AcceptSession prevents excluded apps/projects and previous destinations from
// continuing to publish or delete sessions after reconfiguration. The
// destination check is InCurrentDestination. Project activation compares the
// registration's admission, never its start: an imported session began long
// before the project was activated, and is admitted by the import itself.
func (c Config) AcceptSession(r archive.SessionRegistration) bool {
	if !c.InCurrentDestination(r) {
		return false
	}
	if !c.acceptsHarness(r) {
		return false
	}
	admitted := r.Admitted()
	for _, p := range c.Archive.Projects {
		if p.Included && p.Root == r.ProjectRoot {
			return p.ActivatedAt.IsZero() || !admitted.Before(p.ActivatedAt)
		}
	}
	return len(c.Archive.Projects) == 0 // older programmatic configurations
}

// InCurrentDestination reports whether a registration published to the
// storage destination configured now, rather than to one it replaced. A
// registration that records its destination's ID is compared by that ID, so
// one admitted into a bucket belongs to it again if the configuration
// switches back. A registration without one (written before the ID existed)
// falls back to time: it belongs here if it was admitted at or after
// DestinationSince. Like AcceptSession it compares the admission, not the
// start: an import published here even though it started before this
// destination was configured.
func (c Config) InCurrentDestination(r archive.SessionRegistration) bool {
	if r.DestinationID != "" {
		return r.DestinationID == c.DestinationID()
	}
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
