package backfill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Batch is one confirmed import, kept as imports/<id>.json under the archive
// home. It is local bookkeeping and is never uploaded. It holds no native ID
// and no path: projects are named by project ID and sessions by archive
// session ID. Registrations point back to it through ImportBatch.
type Batch struct {
	// ID is <local date>-<n>, n counting that day's imports from 1.
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	// CompletedAt is set once every session of the confirmed plan has been
	// registered or skipped. An import without it was interrupted; the next
	// run with the same filters continues it.
	CompletedAt *time.Time   `json:"completed_at,omitempty"`
	Filters     BatchFilters `json:"filters"`
	// DestinationID names the bucket the import was confirmed for.
	DestinationID string `json:"destination_id"`
	// ProjectsAdded are the project IDs the import added to the
	// configuration; projects that were already included are not listed.
	ProjectsAdded []string `json:"projects_added"`
	// AppsAdded are the apps the import added to ImportedHarnesses.
	AppsAdded []string `json:"apps_added"`
	// Sessions are the archive session IDs the import registered.
	Sessions []string `json:"sessions"`
	// Subagents are the archive session IDs given to the imported sessions'
	// subagent transcripts. Each registers when the collector validates it.
	Subagents []string `json:"subagents"`
}

// BatchFilters are the filters and --include-* flags a batch was run with.
// A --project directory is recorded by its project ID.
type BatchFilters struct {
	Harnesses      []string `json:"harnesses"`
	ProjectIDs     []string `json:"project_ids"`
	Since          string   `json:"since,omitempty"`
	Until          string   `json:"until,omitempty"`
	IncludeHome    bool     `json:"include_home"`
	IncludeTemp    bool     `json:"include_temp"`
	IncludeRemoved bool     `json:"include_removed"`
}

// BatchFilters records the plan's filters without paths. A --project
// directory is named by the project ID of its resolved path, as the plan
// compared it, so the same folder typed two ways continues the same batch.
func (p Plan) BatchFilters() BatchFilters {
	f := p.Filters
	out := BatchFilters{
		Harnesses: []string{}, ProjectIDs: []string{},
		Since: f.Since, Until: f.Until,
		IncludeHome: f.IncludeHome, IncludeTemp: f.IncludeTemp, IncludeRemoved: f.IncludeRemoved,
	}
	for _, h := range f.Harnesses {
		out.Harnesses = addUnique(out.Harnesses, canonicalHarness(h))
	}
	sort.Strings(out.Harnesses)
	for _, dir := range p.projectFilter {
		out.ProjectIDs = addUnique(out.ProjectIDs, archive.ProjectID(dir))
	}
	sort.Strings(out.ProjectIDs)
	return out
}

// Reconcile rebuilds the batch's sessions from the local store, which is the
// source of truth: every registration carrying the batch's ID, and every
// imported subagent candidate of those sessions. A crash inside a
// registration hold registers sessions the batch file never heard of.
func (b *Batch) Reconcile(store *collector.LocalStore) error {
	regs, err := store.LoadRegistrations()
	if err != nil {
		return err
	}
	parents := map[string]bool{}
	for _, reg := range regs {
		if reg.ImportBatch != b.ID {
			continue
		}
		if reg.ParentSessionID != "" {
			b.Subagents = addUnique(b.Subagents, reg.ArchiveSessionID)
		} else {
			b.Sessions = addUnique(b.Sessions, reg.ArchiveSessionID)
			parents[reg.ArchiveSessionID] = true
		}
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if parents[c.ParentArchiveSessionID] && c.Origin == archive.SessionOriginImport {
			b.Subagents = addUnique(b.Subagents, c.ArchiveSessionID)
		}
	}
	return nil
}

// Matches reports whether a run with these filters and destination
// continues the batch.
func (b Batch) Matches(filters BatchFilters, destinationID string) bool {
	return b.Filters.equal(filters) && b.DestinationID == destinationID
}

func (f BatchFilters) equal(o BatchFilters) bool {
	return slices.Equal(f.Harnesses, o.Harnesses) && slices.Equal(f.ProjectIDs, o.ProjectIDs) &&
		f.Since == o.Since && f.Until == o.Until &&
		f.IncludeHome == o.IncludeHome && f.IncludeTemp == o.IncludeTemp && f.IncludeRemoved == o.IncludeRemoved
}

// DestinationID identifies a storage destination by its provider, endpoint,
// bucket, and prefix. It never covers credentials or their references.
func DestinationID(c credentials.Config) string {
	endpoint := ""
	if c.Provider == credentials.ProviderR2 {
		endpoint, _ = credentials.R2Endpoint(c.R2Endpoint, c.R2AccountID)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{c.Provider, endpoint, c.Bucket, strings.Trim(c.Prefix, "/")}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func batchDir(home string) string { return filepath.Join(home, "imports") }

func batchPath(home, id string) string { return filepath.Join(batchDir(home), id+".json") }

// LoadBatches returns every readable import batch, oldest first. A batch
// file that cannot be read is left out and named in err, which is returned
// alongside the batches that could be read: a caller that only reports on
// imports can go on, one that must see every batch treats err as fatal.
func LoadBatches(home string) ([]Batch, error) {
	entries, err := os.ReadDir(batchDir(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list imports: %w", err)
	}
	var out []Batch
	var unreadable []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var b Batch
		if err := local.Read(filepath.Join(batchDir(home), e.Name()), &b); err != nil {
			unreadable = append(unreadable, fmt.Errorf("read import %q: %w", strings.TrimSuffix(e.Name(), ".json"), err))
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, errors.Join(unreadable...)
}

// SaveBatch durably writes b, replacing any earlier version.
func SaveBatch(home string, b Batch) error {
	if b.ID == "" || strings.ContainsAny(b.ID, `/\`) {
		return errors.New("import ID is required")
	}
	if err := local.Write(batchPath(home, b.ID), b); err != nil {
		return fmt.Errorf("save import %s: %w", b.ID, err)
	}
	return nil
}

// OpenBatch returns the batch a confirmed run records into: the latest one,
// if it was interrupted and ran with the same filters and destination, or a
// new one named for now's local date. An interrupted import is continued so
// that its sessions and the projects it added stay one import, which undo
// can reverse as a whole.
func OpenBatch(home string, filters BatchFilters, destinationID string, now time.Time) (Batch, error) {
	batches, err := LoadBatches(home)
	if err != nil {
		return Batch{}, err
	}
	if n := len(batches); n > 0 {
		last := batches[n-1]
		if last.CompletedAt == nil && last.Matches(filters, destinationID) {
			return last, nil
		}
	}
	day := now.Format(dateLayout)
	next := 1
	for _, b := range batches {
		if rest, ok := strings.CutPrefix(b.ID, day+"-"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n >= next {
				next = n + 1
			}
		}
	}
	return Batch{
		ID: fmt.Sprintf("%s-%d", day, next), StartedAt: now.UTC(), Filters: filters, DestinationID: destinationID,
		ProjectsAdded: []string{}, AppsAdded: []string{}, Sessions: []string{}, Subagents: []string{},
	}, nil
}

// AddChanges records the projects and apps a run added to the configuration.
func (b *Batch) AddChanges(projectIDs, apps []string) {
	b.ProjectsAdded = addUnique(b.ProjectsAdded, projectIDs...)
	b.AppsAdded = addUnique(b.AppsAdded, apps...)
}

// AddSessions records sessions and subagents a run registered.
func (b *Batch) AddSessions(sessions, subagents []string) {
	b.Sessions = addUnique(b.Sessions, sessions...)
	b.Subagents = addUnique(b.Subagents, subagents...)
}

// addUnique appends the values not already in list.
func addUnique(list []string, values ...string) []string {
	for _, v := range values {
		if !slices.Contains(list, v) {
			list = append(list, v)
		}
	}
	return list
}
