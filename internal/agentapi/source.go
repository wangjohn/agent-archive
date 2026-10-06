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
	// LegacyUnboundRegistration is set only for an already admitted ordinary
	// registration without a binding or discovery origin. It grants no creation
	// permission; the provider retains its narrow legacy compatibility checks.
	LegacyUnboundRegistration bool
	ReadBudget                *NativeReadBudget
	CodexRollouts             CodexRolloutLookup
	Files                     transcriptio.Opener
	Policy                    transcriptio.OpenPolicy
	Database                  string
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
	Ordinal uint64
	History *archive.SourceHistory
	Kind    NativeRecordKind
	Key     string
	Raw     []byte
	Missing bool
}

// RecordInput yields provider-declared ordered borrowed frames without an export envelope.
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
	// Filename is a physical locator observation only; it grants no path access.
	Filename  string
	StartedAt time.Time
	Limits    ReadLimits
}

// TranscriptFilter owns native filtering and retained evidence interpretation.
type TranscriptFilter interface {
	archive.Adapter
	RetainedComparator
	Filter(context.Context, NativeInput, FilterContext) (archive.FilteredTranscript, error)
	Refilter(context.Context, archive.SourceBundle, time.Time) (archive.FilteredTranscript, error)
}

// LocalIdentityResolver owns native filename conventions and identity preference.
// filename is an observation only; implementations must not open it or inspect
// raw ownership SessionIDs. The returned identity must come from safe facts.
type LocalIdentityResolver interface {
	LocalIdentity(archive.FilteredTranscript, string) archive.NativeSessionIdentity
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

// CodexRegisteredRolloutLookup observes a legacy registered ID that does not
// have modern native UUID layout. The implementation checks existing ownership;
// this port never creates or authorizes a registration.
type CodexRegisteredRolloutLookup interface {
	RegisteredThread(context.Context, string) (CodexRolloutSet, error)
}
