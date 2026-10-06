package discovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// SourceDescriptor keeps a stable source identity separate from its private
// locator. Neither an adapter locator nor its fingerprint authorizes capture.
type SourceDescriptor struct {
	Kind      archive.SourceKind
	StableKey string
	Locator   string
	Root      string
	Priority  int
}

// Candidate contains source facts only; shared policy chooses the project,
// destination and authorization generation and performs all registration.
type Candidate struct {
	Agent              string
	NativeSessionID    string
	Source             SourceDescriptor
	StartedAt          time.Time
	FirstTaskAt        time.Time
	StartEvidence      string
	WorkingDirectory   string
	HarnessVersion     string
	ProducerOriginator string
	ProducerSource     string
	FormatProfile      sourcefacts.CodexProfile
	Execution          string
	NativeChild        bool
	RootNativeID       string
	OwnStart           *uint64
	ParentNativeID     string
	ForkNativeID       string
}

// Outcome is a content-free source classification, independent of policy.
type Outcome string

const (
	outcomeUsable      Outcome = "native_format"
	outcomeIncomplete  Outcome = "incomplete_metadata"
	outcomeUnavailable Outcome = "source_unavailable"
	outcomeChanged     Outcome = "source_changed"
)

// Observation is one bounded metadata probe and its typed outcome.
type Observation struct {
	// Identity is validated native lookup evidence, independent of Candidate admission.
	Identity        *codexmeta.CodexIdentity
	NativeCreatedAt time.Time
	Candidate       Candidate
	Outcome         Outcome
	Bytes           int64
}

// Fingerprint is a retry/scheduling hint, never native start evidence.
type Fingerprint struct {
	Size  int64
	Mtime int64
}

// SourceEntry is a source or child directory in a bounded enumeration batch.
type SourceEntry struct {
	Source      SourceDescriptor
	Fingerprint Fingerprint
	Directory   string
}

// SourceBatch carries a durable enumeration cookie and at most 256 entries.
// A cookie is a coverage hint, never freshness or completeness evidence.
type SourceBatch struct {
	Entries      []SourceEntry
	Continuation int64
	Complete     bool
}

// SourceAdapter supplies bounded enumeration and native evidence only. It
// cannot receive configuration, select destinations/projects or write state.
type SourceAdapter interface {
	Agent() string
	InitialDirectories() []string
	Enumerate(context.Context, string, string, int64) (SourceBatch, error)
	Describe(string, string, string) SourceEntry
	Inspect(context.Context, SourceDescriptor) Observation
	Supported(Candidate) bool
	PriorityDirectories(time.Time) []string
}

// The compile-time registry ships only Codex. Format support stays bounded;
// Claude and Cursor retain their existing hooks and gain no discovery path.
func registeredAdapters() []SourceAdapter { return []SourceAdapter{codexAdapter{}} }

func findAdapter(adapters []SourceAdapter, agent string) SourceAdapter {
	for _, a := range adapters {
		if a.Agent() == agent {
			return a
		}
	}
	return nil
}

type codexAdapter struct {
	supported func(sourcefacts.CodexMeta) bool
}

func (codexAdapter) Agent() string { return "codex" }

func (codexAdapter) InitialDirectories() []string { return []string{"sessions", "archived_sessions"} }

func (a codexAdapter) Enumerate(ctx context.Context, root, path string, cookie int64) (SourceBatch, error) {
	if err := ctx.Err(); err != nil {
		return SourceBatch{}, err
	}
	names, next, complete, err := readBatch(directory{Root: root, Path: path, Offset: cookie})
	if err != nil {
		return SourceBatch{}, err
	}
	b := SourceBatch{Continuation: next, Complete: complete}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return SourceBatch{}, err
		}
		b.Entries = append(b.Entries, a.Describe(root, path, name))
	}
	return b, nil
}

func (codexAdapter) Describe(root, path, name string) SourceEntry {
	loc := filepath.Join(root, path, name)
	info, err := os.Lstat(loc)
	if err != nil {
		return SourceEntry{}
	}
	if info.IsDir() {
		return SourceEntry{Directory: filepath.Join(path, name)}
	}
	if !info.Mode().IsRegular() || !strings.HasPrefix(name, "rollout-") || sourcefacts.RolloutID(name) == "" {
		return SourceEntry{}
	}
	priority := 0
	if local.PathWithin(loc, filepath.Join(root, "archived_sessions")) {
		priority = 1
	}
	return SourceEntry{Source: SourceDescriptor{Priority: priority, Kind: archive.SourceKindFile, StableKey: sourcefacts.RolloutID(name), Locator: loc, Root: root}, Fingerprint: Fingerprint{Size: info.Size(), Mtime: info.ModTime().UnixNano()}}
}

func (a codexAdapter) Inspect(ctx context.Context, source SourceDescriptor) Observation {
	h := sourcefacts.ReadHeader(ctx, source.Root, source.Locator)
	o := Observation{Outcome: Outcome(h.Outcome), Bytes: h.Bytes, Identity: h.Identity, NativeCreatedAt: h.NativeCreatedAt}
	if o.Outcome != outcomeUsable {
		return o
	}
	o.Candidate = candidateFromHeader(h, source)
	return o
}

func candidateFromHeader(h sourcefacts.Header, source SourceDescriptor) Candidate {
	var producerSource string
	if json.Unmarshal(h.Meta.Source, &producerSource) != nil {
		producerSource = string(h.Meta.Source)
	}
	c := Candidate{Agent: "codex", NativeSessionID: h.Meta.ID, Source: source, StartedAt: h.Started, FirstTaskAt: h.FirstTaskAt, StartEvidence: "native_start", WorkingDirectory: h.Meta.Cwd, HarnessVersion: h.Meta.Version, ProducerOriginator: h.Meta.Originator, ProducerSource: producerSource, FormatProfile: h.Profile, Execution: "native"}
	if h.Identity != nil {
		c.NativeChild = h.Identity.Child
		c.RootNativeID = h.Identity.RootID
		c.ParentNativeID = h.Identity.ParentID
		c.ForkNativeID = h.Identity.ForkID
		c.OwnStart = h.Identity.SubagentOrdinal
	}
	return c
}

func (codexAdapter) PriorityDirectories(now time.Time) []string {
	var paths []string
	for _, delta := range []int{0, -1, 1} {
		paths = append(paths, filepath.Join("sessions", now.AddDate(0, 0, delta).Format("2006/01/02")))
	}
	return paths
}

func (a codexAdapter) Supported(c Candidate) bool {
	if c.FormatProfile != sourcefacts.CodexLegacyJSONL && c.FormatProfile != sourcefacts.CodexPaginatedJSONL {
		return false
	}
	supported := a.supported
	if supported == nil {
		supported = sourcefacts.SupportedCodexProducer
	}
	source, _ := json.Marshal(c.ProducerSource)
	if strings.HasPrefix(c.ProducerSource, "{") {
		source = json.RawMessage(c.ProducerSource)
	}
	return supported(sourcefacts.CodexMeta{Version: c.HarnessVersion, Originator: c.ProducerOriginator, Source: source})
}
