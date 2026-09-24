package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aws/smithy-go"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type verificationEvidence struct {
	ConfigurationID string    `json:"configuration_id"`
	PublishedAt     time.Time `json:"published_at"`
	VerifiedAt      time.Time `json:"verified_at"`
	SourceSHA256    string    `json:"source_sha256"`
	// Outcome is the most recent attempt's result for this publication:
	// verificationOutcomeVerified, verificationOutcomeFailed (the remote was
	// unreachable or the object is missing; worth retrying) or
	// verificationOutcomeMismatch (the remote no longer identifies what this
	// machine published; permanent for this publication, cleared only by a
	// republish or a configuration change).
	Outcome verificationOutcome `json:"outcome,omitempty"`
	// Attempts counts read-backs tried for this publication under this
	// configuration; NextRetryAt is when the next one may run, following
	// verificationBackoff. Both reset when the publication or configuration
	// changes.
	Attempts    int       `json:"attempts,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	NextRetryAt time.Time `json:"next_retry_at,omitempty"`
}

// verificationOutcome is the result of one read-back attempt.
type verificationOutcome string

const (
	verificationOutcomeVerified verificationOutcome = "verified"
	verificationOutcomeFailed   verificationOutcome = "failed"
	verificationOutcomeMismatch verificationOutcome = "mismatch"
)

const (
	// maxVerificationsPerPass bounds the read-back downloads (each up to the
	// reader's compressed limit) one pass may perform, so a configuration
	// change that invalidates every record does not re-download every
	// published source on the next tick. Oldest publications go first.
	maxVerificationsPerPass = 5
)

// verificationBackoff spaces retries of a failed read-back; the last entry
// repeats once attempts exceed the schedule.
var verificationBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 6 * time.Hour, 24 * time.Hour}

// errVerificationMismatch marks a read-back that can only be fixed by a
// republish: the remote metadata does not describe this machine's last
// publication. It is distinct from a transient failure so status can say so.
var errVerificationMismatch = errors.New("read-back mismatch")

func verificationRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(verificationBackoff) {
		attempts = len(verificationBackoff)
	}
	return verificationBackoff[attempts-1]
}

// verificationSummary reports what one verifyPublications pass did.
type verificationSummary struct {
	Attempted  int
	Verified   int
	Failed     int
	Mismatched int
	// Deferred counts publications still unverified that were not attempted
	// this pass, because their backoff has not elapsed or the per-pass cap
	// was reached.
	Deferred int
}

type storageHealth struct {
	ConfigurationID string `json:"configuration_id"`
	//lint:ignore LV1001 set and compared as literals in collect.go and status.go; typing it needs collect.go to change with it
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checked_at"`
	Context   string    `json:"context"`
}

func configurationID(cfg config.Config) string {
	// These fields contain references, never credentials. Pausing, retention,
	// and unrelated historical settings do not invalidate capture verification.
	//lint:ignore musttag hash input for the configuration ID: its key names are the Go field names, and changing them would invalidate stored verification evidence
	data, _ := json.Marshal(struct {
		Storage            any
		Harnesses          []string
		Machine            string
		Since              time.Time
		CapabilityContract int
	}{cfg.Storage, cfg.Harnesses, cfg.MachineID, cfg.DestinationSince, 1})
	return storage.SHA256Hex(data)
}

func sessionVerificationConfigurationID(cfg config.Config, reg archive.SessionRegistration) string {
	var activation time.Time
	for _, project := range cfg.Archive.Projects {
		if project.Included && project.Root == reg.ProjectRoot {
			activation = project.ActivatedAt
			break
		}
	}
	//lint:ignore musttag hash input for the configuration ID: its key names are the Go field names, and changing them would invalidate stored verification evidence
	data, _ := json.Marshal(struct {
		Storage            any
		Machine            string
		DestinationSince   time.Time
		Harness            string
		ProjectRoot        string
		ProjectActivatedAt time.Time
		CapabilityContract int
	}{cfg.Storage, cfg.MachineID, cfg.DestinationSince, reg.Harness.Name, reg.ProjectRoot, activation, 1})
	return storage.SHA256Hex(data)
}

func verificationPath(home, id string) string {
	return filepath.Join(home, "sessions", id, "verification.json")
}

func readVerification(home, id string) (verificationEvidence, error) {
	var v verificationEvidence
	err := local.Read(verificationPath(home, id), &v)
	if os.IsNotExist(err) {
		return v, nil
	}
	return v, err
}

func recordStorageHealth(home string, cfg config.Config, env Env, background bool, state string) error {
	contextName := "manual_sync"
	if background {
		contextName = "background_collector"
	}
	return local.Write(filepath.Join(home, "storage-health.json"), storageHealth{configurationID(cfg), state, env.now().UTC(), contextName})
}

// Storage access evidence: the background collector re-probes once its last
// verified record is older than storageHealthRefreshAfter, and status calls a
// verified record stale after storageHealthStaleAfter. The refresh interval is
// shorter than the staleness threshold so a healthy, unpaused install is never
// reported stale between ticks.
const (
	storageHealthRefreshAfter = 4 * time.Minute
	storageHealthStaleAfter   = 10 * time.Minute
)

// isStorageError reports whether err came from the object store itself (an
// AWS API or operation error, or a storage sentinel) rather than from
// reading or filtering a local transcript.
func isStorageError(err error) bool {
	var api smithy.APIError
	var op *smithy.OperationError
	return errors.As(err, &api) || errors.As(err, &op) || errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrChecksumMismatch)
}

func storageFailureState(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "ExpiredToken", "ExpiredTokenException", "RequestExpired":
			return "credentials_expired"
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "Unauthorized":
			return "authentication_failed"
		}
	}
	return "storage_unavailable"
}

// Verify only new publications or evidence invalidated by configuration changes.
// Status reads this local result; it never downloads conversation content.
//
// A failed read-back is recorded with a retry time and never surfaces as a
// pass error: the publication itself succeeded, so `sync` still exits 0 and
// Status.LastError stays free for genuine collection failures. Status reports
// the pending or failed verification from the record instead.
//
// Local state that cannot be read fails only its own session's read-back:
// an unreadable registration is skipped (the collector pass has already
// reported it), and a session whose published state or verification record
// cannot be read is left out and returned as an error once every other
// session has been handled.
func verifyPublications(home string, cfg config.Config, env Env, store *collector.LocalStore, remote storage.ObjectStore) (verificationSummary, error) {
	return verifyPublicationsWithin(context.Background(), home, cfg, env, store, remote)
}

// verifyPublicationsWithin is verifyPublications within ctx: a collector
// pass passes its own deadline, so read-back gets what is left of the pass's
// budget rather than time of its own on top. Read-backs ctx leaves no time
// for are deferred to the next pass, like those over the per-pass cap.
func verifyPublicationsWithin(ctx context.Context, home string, cfg config.Config, env Env, store *collector.LocalStore, remote storage.ObjectStore) (verificationSummary, error) {
	var summary verificationSummary
	regs, _, err := store.ScanRegistrations()
	if err != nil {
		return summary, err
	}
	now := env.now().UTC()
	type candidate struct {
		reg    archive.SessionRegistration
		bundle archive.SourceBundle
		at     time.Time
		prior  verificationEvidence
		// cfgID is the per-session verification configuration hash the
		// record is keyed to (see sessionVerificationConfigurationID).
		cfgID string
	}
	var due []candidate
	var localErrs []error
	for _, reg := range regs {
		if !cfg.AcceptSession(reg) {
			continue
		}
		bundle, at, _, err := store.LoadLastPublished(reg.ArchiveSessionID)
		if err != nil {
			localErrs = append(localErrs, err)
			continue
		}
		if at.IsZero() {
			continue
		}
		prior, err := readVerification(home, reg.ArchiveSessionID)
		if err != nil {
			localErrs = append(localErrs, err)
			continue
		}
		verificationConfigurationID := sessionVerificationConfigurationID(cfg, reg)
		if prior.ConfigurationID == verificationConfigurationID && prior.PublishedAt.Equal(at) {
			if !prior.VerifiedAt.IsZero() {
				continue
			}
			if now.Before(prior.NextRetryAt) {
				summary.Deferred++
				continue
			}
		} else {
			// A new publication or configuration starts its own attempt count.
			prior = verificationEvidence{}
		}
		due = append(due, candidate{reg, bundle, at, prior, verificationConfigurationID})
	}
	// Imports are read back too, but only hook-captured publications prove an
	// app's capture works (status never promotes an app on an import), so
	// they go first: a large import must not hold them behind the cap.
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].reg.Imported() != due[j].reg.Imported() {
			return !due[i].reg.Imported()
		}
		return due[i].at.Before(due[j].at)
	})
	if len(due) > maxVerificationsPerPass {
		summary.Deferred += len(due) - maxVerificationsPerPass
		due = due[:maxVerificationsPerPass]
	}
	for _, c := range due {
		if ctx.Err() != nil {
			summary.Deferred++
			continue
		}
		summary.Attempted++
		sha, err := verifyPublication(ctx, cfg, store, remote, c.reg, c.bundle)
		record := verificationEvidence{ConfigurationID: c.cfgID, PublishedAt: c.at, SourceSHA256: sha, Attempts: c.prior.Attempts + 1}
		switch {
		case err == nil:
			summary.Verified++
			record.Outcome = verificationOutcomeVerified
			record.VerifiedAt = now
		case errors.Is(err, errVerificationMismatch):
			summary.Mismatched++
			record.Outcome = verificationOutcomeMismatch
		default:
			summary.Failed++
			record.Outcome = verificationOutcomeFailed
		}
		if err != nil {
			record.LastError = err.Error()
			record.NextRetryAt = now.Add(verificationRetryDelay(record.Attempts))
		}
		if err := local.Write(verificationPath(home, c.reg.ArchiveSessionID), record); err != nil {
			localErrs = append(localErrs, err)
		}
	}
	return summary, errors.Join(localErrs...)
}

// verificationTimeout bounds one session's read-back (within the caller's own
// deadline): a metadata read and a
// source download of at most the reader's compressed limit. A variable only
// so a test can shorten it.
var verificationTimeout = 5 * time.Minute

// verifyPublication reads one session's remote metadata and source back and
// returns the verified source SHA-256. A mismatch wraps errVerificationMismatch;
// any other error is treated as transient.
//
// The source the metadata must name is the one recorded when it was uploaded
// (collector.LocalStore.LoadLastPublishedSource). Only state from a version
// that recorded none falls back to rebuilding the digest from the cached
// bundle, which a later source schema or compressor can no longer reproduce.
func verifyPublication(ctx context.Context, cfg config.Config, store *collector.LocalStore, remote storage.ObjectStore, reg archive.SessionRegistration, bundle archive.SourceBundle) (string, error) {
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return "", err
	}
	expected, recorded, err := store.LoadLastPublishedSource(reg.ArchiveSessionID)
	if err != nil {
		return "", err
	}
	if !recorded {
		rebuilt, err := archive.BuildCompressedSource(bundle)
		if err != nil {
			return "", err
		}
		expected.SHA256 = rebuilt.SHA256
	}
	ctx, cancel := context.WithTimeout(ctx, verificationTimeout)
	defer cancel()
	metadata, err := reader.ReadMetadata(ctx, remote, key)
	if err != nil {
		return "", err
	}
	if metadata.SourceBundle.SHA256 != expected.SHA256 || (recorded && metadata.SourceBundle.Key != expected.Key) {
		return "", fmt.Errorf("%w: remote metadata does not identify the last local publication", errVerificationMismatch)
	}
	if metadata.MachineID != cfg.MachineID {
		return "", fmt.Errorf("%w: metadata ownership does not match this machine", errVerificationMismatch)
	}
	if _, err := reader.LoadSource(ctx, remote, metadata, reader.Limits{}); err != nil {
		return "", err
	}
	return metadata.SourceBundle.SHA256, nil
}
