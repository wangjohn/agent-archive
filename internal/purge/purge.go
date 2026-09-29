// Package purge builds and applies conservative plans for unreferenced archive
// sources. The bucket's current metadata, not local collector state, decides
// whether a source is live.
package purge

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

const Lifetime = 5 * time.Minute
const maxSourceRead = 32 << 20

var sourceName = regexp.MustCompile(`^source\.[0-9a-f]{64}\.jsonl\.gz$`)

type Candidate struct {
	Key           string `json:"key"`
	Size          int64  `json:"size"`
	ETag          string `json:"etag"`
	FilterVersion string `json:"filter_version,omitempty"`
}

type CurrentOldSession struct {
	MetadataKey   string `json:"metadata_key"`
	SourceKey     string `json:"source_key"`
	FilterVersion string `json:"filter_version"`
}

type Plan struct {
	Version            int                 `json:"version"`
	DestinationID      string              `json:"destination_id"`
	Bucket             string              `json:"bucket"`
	Prefix             string              `json:"prefix"`
	Mode               string              `json:"mode"`
	BeforeFilter       string              `json:"before_filter,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	ExpiresAt          time.Time           `json:"expires_at"`
	Candidates         []Candidate         `json:"candidates"`
	CurrentOldSessions []CurrentOldSession `json:"current_old_sessions"`
	Digest             string              `json:"digest"`
}

// Inventory reads every current metadata sidecar before it proposes a source.
// An ambiguous or unreadable sidecar makes the entire plan fail closed.
func Inventory(ctx context.Context, store storage.ObjectStore, destinationID, bucket, prefix, mode, before string, now time.Time) (Plan, error) {
	if mode != "unreferenced" && mode != "old-filter" {
		return Plan{}, fmt.Errorf("unsupported purge mode %q", mode)
	}
	if mode == "old-filter" && !numericBefore(before) {
		return Plan{}, fmt.Errorf("old-filter mode requires a numeric --before-filter version")
	}
	if mode == "unreferenced" {
		before = archive.FilterVersion
	}
	objects, err := store.List(ctx, "sessions")
	if err != nil {
		return Plan{}, fmt.Errorf("list sessions: %w", err)
	}
	current := make(map[string]bool)
	listedSources := make(map[string]bool)
	for _, object := range objects {
		if validSourceKey(object.Key) {
			listedSources[object.Key] = true
		}
	}
	var old []CurrentOldSession
	for _, object := range objects {
		if strings.HasPrefix(object.Key, "sessions/") && strings.HasSuffix(object.Key, "/metadata.json") && !validMetadataKey(object.Key) {
			return Plan{}, fmt.Errorf("unexpected metadata key %q", object.Key)
		}
		if !validMetadataKey(object.Key) {
			continue
		}
		meta, err := reader.ReadMetadata(ctx, store, object.Key)
		if err != nil {
			return Plan{}, fmt.Errorf("inspect %q: %w", object.Key, err)
		}
		if path.Dir(meta.SourceBundle.Key) != path.Dir(object.Key) || !validSourceKey(meta.SourceBundle.Key) {
			return Plan{}, fmt.Errorf("metadata %q has an invalid source reference", object.Key)
		}
		current[meta.SourceBundle.Key] = true
		if !listedSources[meta.SourceBundle.Key] {
			return Plan{}, fmt.Errorf("metadata %q references a missing source", object.Key)
		}
		if numericOlder(meta.FilterVersion, before) {
			old = append(old, CurrentOldSession{object.Key, meta.SourceBundle.Key, meta.FilterVersion})
		}
	}
	var candidates []Candidate
	for _, object := range objects {
		if !validSourceKey(object.Key) || current[object.Key] {
			continue
		}
		candidate := Candidate{Key: object.Key, Size: object.Size, ETag: object.ETag}
		if mode == "old-filter" {
			version, err := sourceFilterVersion(ctx, store, object)
			if err != nil {
				return Plan{}, fmt.Errorf("inspect %q: %w", object.Key, err)
			}
			candidate.FilterVersion = version
			if !numericOlder(version, before) {
				continue
			}
		}
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Key < candidates[j].Key })
	sort.Slice(old, func(i, j int) bool { return old[i].MetadataKey < old[j].MetadataKey })
	plan := Plan{Version: 1, DestinationID: destinationID, Bucket: bucket, Prefix: prefix, Mode: mode, BeforeFilter: before, CreatedAt: now.UTC(), ExpiresAt: now.Add(Lifetime).UTC(), Candidates: candidates, CurrentOldSessions: old}
	plan.Digest, err = digest(plan)
	return plan, err
}

func validMetadataKey(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 4 && parts[0] == "sessions" && validHarness(parts[1]) && validSession(parts[2]) && parts[3] == "metadata.json"
}

func validSourceKey(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 4 && parts[0] == "sessions" && validHarness(parts[1]) && validSession(parts[2]) && sourceName.MatchString(parts[3])
}

func validHarness(value string) bool {
	return value == "claude" || value == "codex" || value == "cursor"
}
func validSession(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func numericBefore(version string) bool { n, err := strconv.Atoi(version); return err == nil && n > 0 }
func numericOlder(version, before string) bool {
	if !numericBefore(before) {
		return false
	}
	a, errA := strconv.Atoi(version)
	b, _ := strconv.Atoi(before)
	return errA == nil && a > 0 && a < b
}

func sourceFilterVersion(ctx context.Context, store storage.ObjectStore, object storage.Object) (string, error) {
	if object.Size > maxSourceRead {
		return "", fmt.Errorf("source exceeds %d-byte inspection limit", maxSourceRead)
	}
	data, err := store.Get(ctx, object.Key)
	if err != nil {
		return "", err
	}
	if len(data) > maxSourceRead {
		return "", fmt.Errorf("source exceeds %d-byte inspection limit", maxSourceRead)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer reader.Close()
	line, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err != nil {
		return "", err
	}
	first, _, ok := bytes.Cut(line, []byte{'\n'})
	if !ok {
		return "", errors.New("source header exceeds inspection limit")
	}
	var header archive.SourceHeader
	if err := json.Unmarshal(first, &header); err != nil {
		return "", err
	}
	if header.Kind != archive.SourceLineHeader || header.Capture.FilterVersion == "" {
		return "", errors.New("source has no supported filter header")
	}
	return header.Capture.FilterVersion, nil
}

func digest(plan Plan) (string, error) {
	plan.Digest = ""
	data, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (p Plan) Validate(destinationID string, now time.Time) error {
	if p.Version != 1 || p.DestinationID != destinationID {
		return errors.New("purge plan destination changed or version unsupported")
	}
	if now.Before(p.CreatedAt) || !now.Before(p.ExpiresAt) {
		return errors.New("purge plan expired; create a new plan")
	}
	got, err := digest(p)
	if err != nil {
		return err
	}
	if got != p.Digest {
		return errors.New("purge plan digest mismatch")
	}
	return nil
}

type Report struct {
	PlanDigest string   `json:"plan_digest"`
	Deleted    []string `json:"deleted"`
	Remaining  []string `json:"remaining"`
	Error      string   `json:"error,omitempty"`
}

// Apply deletes one candidate at a time after a fresh full metadata scan.
// The caller durably saves the report after each successful delete. All Macs
// writing to this prefix must be paused; S3 offers no atomic conditional
// delete against another writer's metadata update.
func Apply(ctx context.Context, store storage.ObjectStore, plan Plan, report *Report, save func(Report) error) error {
	if report.PlanDigest != plan.Digest {
		return errors.New("report does not match plan")
	}
	remaining := make(map[string]bool, len(plan.Candidates))
	for _, c := range plan.Candidates {
		remaining[c.Key] = true
	}
	for _, key := range report.Deleted {
		if !remaining[key] {
			return errors.New("report contains an unknown or repeated deleted key")
		}
		delete(remaining, key)
	}
	for _, key := range report.Remaining {
		if !remaining[key] {
			return errors.New("report contains an unknown or repeated remaining key")
		}
		delete(remaining, key)
	}
	if len(remaining) != 0 {
		return errors.New("report omits planned keys")
	}
	for len(report.Remaining) > 0 {
		key := report.Remaining[0]
		candidate, ok := findCandidate(plan.Candidates, key)
		if !ok || !validSourceKey(key) {
			return fmt.Errorf("invalid planned key %q", key)
		}
		objects, err := store.List(ctx, "sessions")
		if err != nil {
			return fmt.Errorf("relist before %q: %w", key, err)
		}
		var listed *storage.Object
		for i := range objects {
			if objects[i].Key == key {
				listed = &objects[i]
			}
			if !validMetadataKey(objects[i].Key) {
				continue
			}
			meta, err := reader.ReadMetadata(ctx, store, objects[i].Key)
			if err != nil {
				return fmt.Errorf("recheck %q: %w", objects[i].Key, err)
			}
			if meta.SourceBundle.Key == key {
				return fmt.Errorf("%q is now a current source", key)
			}
		}
		if listed != nil && (listed.Size != candidate.Size || listed.ETag != candidate.ETag) {
			return fmt.Errorf("%q changed since plan", key)
		}
		if listed != nil {
			if err := store.Delete(ctx, key); err != nil {
				return fmt.Errorf("delete %q: %w", key, err)
			}
		}
		report.Deleted = append(report.Deleted, key)
		report.Remaining = report.Remaining[1:]
		report.Error = ""
		if err := save(*report); err != nil {
			return fmt.Errorf("save deletion report: %w", err)
		}
	}
	return nil
}

func findCandidate(candidates []Candidate, key string) (Candidate, bool) {
	for _, item := range candidates {
		if item.Key == key {
			return item, true
		}
	}
	return Candidate{}, false
}
