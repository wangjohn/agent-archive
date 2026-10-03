package agentapi

import (
	"context"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// Mutation declares the shared rewrite policy.
type Mutation uint8

const (
	// AppendOnly blocks a source which no longer extends retained evidence.
	AppendOnly Mutation = iota
	// ReplaceableSnapshot allows replacement with a cumulative provenance gap.
	ReplaceableSnapshot
)

// SourceSemantics describes observable consistency and rewrite behavior.
type SourceSemantics struct {
	Mutation Mutation
	Provider string
}

// SourceEnvironment supplies narrow read dependencies and local database location.
type SourceEnvironment struct {
	Files    transcriptio.Opener
	Policy   transcriptio.OpenPolicy
	Database string
}

// ReadLimits bounds raw values and each native record before filtering.
type ReadLimits struct {
	RawBytes         int64
	RecordBytes      int64
	SubagentMetadata bool
}

// SourceSignature is a bounded, provider-qualified equality token.
type SourceSignature struct {
	Version  uint8  `json:"version"`
	Provider string `json:"provider"`
	Token    string `json:"token"`
}

// SourceObservation separates presence, activity and emptiness from equality.
type SourceObservation struct {
	Signature SourceSignature
	Present   bool
	Empty     bool
	Activity  time.Time
	Size      int64
}

// SourceProvider owns native reads but never shared publication policy.
type SourceProvider interface {
	Describe(SourceRef) (SourceSemantics, error)
	OpenPass(context.Context, SourceEnvironment) (SourcePass, error)
}

// SourcePass is serial, lazy, single-owner, and terminal after Close.
type SourcePass interface {
	Signature(context.Context, SourceRef) (SourceObservation, error)
	Read(context.Context, SourceRef, ReadLimits) (SourceSnapshot, error)
	Close() error
}

// SourceSnapshot owns immutable borrowed input valid only during its pass.
type SourceSnapshot interface {
	Observation() SourceObservation
	Input() NativeInput
	Close() error
}

// FileInput exposes verified bounded reads, coverage, and post-read verification.
type FileInput = transcriptio.Input

// NativeRecordKind identifies a provider-owned record frame.
type NativeRecordKind string

// ComposerRecord and BubbleRecord are the Cursor provider's actual ordered frames.
const (
	ComposerRecord NativeRecordKind = "composer"
	BubbleRecord   NativeRecordKind = "bubble"
)

// NativeRecord borrows one immutable ordered native value until snapshot close.
type NativeRecord struct {
	Kind    NativeRecordKind
	Key     string
	Raw     []byte
	Missing bool
}

// RecordInput yields composer first and bubbles in header order, with no export envelope.
type RecordInput interface {
	Next(context.Context) (NativeRecord, bool, error)
}

// NativeInput is an explicit union; exactly one File or Records is populated.
type NativeInput struct {
	File         FileInput
	Records      RecordInput
	SubagentMeta []byte
}

// FilterContext supplies existing native filtering observations without path access.
type FilterContext struct {
	StartedAt time.Time
	Limits    ReadLimits
}

// TranscriptFilter interprets bounded native input. Adapter methods are temporary retained-bundle compatibility.
type TranscriptFilter interface {
	archive.Adapter
	Filter(context.Context, NativeInput, FilterContext) (archive.FilteredTranscript, error)
	Refilter(context.Context, archive.SourceBundle, time.Time) (archive.FilteredTranscript, error)
}

// SourcesLookup is the narrow source/filter binding injected into shared readers.
type SourcesLookup interface {
	LookupSources(string) (SourceProvider, TranscriptFilter, bool)
}

// SourceSweeper performs content-free cleanup of abandoned provider resources.
type SourceSweeper interface{ SweepSources() }

// ActivityProvider batches cheap ordering observations without snapshotting or filtering.
type ActivityProvider interface {
	Activities(context.Context, SourceEnvironment, []SourceRef) (map[SourceRef]time.Time, error)
}
