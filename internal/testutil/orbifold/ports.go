// Package orbifold supplies a test-only integration with independent native vocabulary.
package orbifold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// ID is the test-only integration catalog identity.
const ID agentmeta.ID = "orbifold"

// Kind identifies the synthetic manifest source locator.
const Kind archive.SourceKind = "orbifold/manifest"

// Frame identifies the independent synthetic native record framing.
const Frame agentapi.NativeRecordKind = "orbifold-pulse"

// Format identifies retained Orbifold pulse bundles.
const Format = "orbifold-pulse-set"

// NativePulse is intentionally unrelated to existing native schemas.
type NativePulse struct {
	PulseKind      string  `json:"pulseKind"`
	Speaker        Speaker `json:"speaker"`
	Words          string  `json:"words"`
	NativeIdentity string  `json:"nativeIdentity"`
	Clock          string  `json:"clock"`
	Landing        string  `json:"landing,omitempty"`
	Credential     string  `json:"credential,omitempty"`
}

// Ports owns a mutable synthetic source; passes freeze its multiple shard values.
type Ports struct {
	Shards         [][]byte
	ShardPaths     []string
	Generation     int
	Mutation       agentapi.Mutation
	TransientReads int
	Reads          int
	Filters        int
	Parses         int
	ParserVersion  string
}

// Name returns the synthetic integration identity.
func (p *Ports) Name() string { return string(ID) }

// Version returns the synthetic privacy filter version.
func (p *Ports) Version() string { return "orbifold-filter-1" }

// Describe validates a synthetic locator and declares its mutation semantics.
func (p *Ports) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	if ref.Kind != Kind && ref.Kind != archive.SourceKindFile || ref.Kind == Kind && ref.Key == "" {
		return agentapi.SourceSemantics{}, errors.New("unsupported orbifold locator")
	}
	return agentapi.SourceSemantics{Mutation: p.Mutation, Provider: string(Kind)}, nil
}

// OpenPass opens a caller-scoped synthetic source observation pass.
func (p *Ports) OpenPass(ctx context.Context, _ agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &pass{owner: p}, nil
}

// OpenAdmissionPass uses the same bounded in-memory snapshot as ordinary reads.
func (p *Ports) OpenAdmissionPass(ctx context.Context, env agentapi.SourceEnvironment, _ agentapi.SourceRef) (agentapi.SourcePass, error) {
	return p.OpenPass(ctx, env)
}

type pass struct {
	owner  *Ports
	closed bool
	live   []*snapshot
}

func (p *pass) Signature(ctx context.Context, ref agentapi.SourceRef) (agentapi.SourceObservation, error) {
	if p.closed {
		return agentapi.SourceObservation{}, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return agentapi.SourceObservation{}, err
	}
	if _, err := p.owner.Describe(ref); err != nil {
		return agentapi.SourceObservation{}, err
	}
	var size int64
	for _, raw := range p.owner.Shards {
		size += int64(len(raw))
	}
	return agentapi.SourceObservation{Present: true, Empty: len(p.owner.Shards) == 0, Size: size, Activity: time.Date(2026, 9, 1, 0, 0, p.owner.Generation, 0, time.UTC), Signature: agentapi.SourceSignature{Version: 1, Provider: string(Kind), Token: strconv.Itoa(p.owner.Generation)}}, nil
}

func (p *pass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	observed, err := p.Signature(ctx, ref)
	if err != nil {
		return nil, err
	}
	p.owner.Reads++
	if p.owner.TransientReads > 0 {
		p.owner.TransientReads--
		return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("synthetic retry"))
	}
	if limits.RawBytes > 0 && observed.Size > limits.RawBytes {
		return nil, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
	}
	shards := p.owner.Shards
	if len(p.owner.ShardPaths) > 0 {
		shards, err = ReadManifestShards(p.owner.ShardPaths, func(path string) (io.ReadCloser, error) { return os.Open(path) })
		if err != nil {
			return nil, err
		}
	}
	s := &snapshot{observed: observed}
	for _, raw := range shards {
		if limits.RecordBytes > 0 && int64(len(raw)) > limits.RecordBytes {
			return nil, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		s.shards = append(s.shards, bytes.Clone(raw))
	}
	p.live = append(p.live, s)
	return s, nil
}

func (p *pass) Close() error {
	if !p.closed {
		p.closed = true
		for _, s := range p.live {
			_ = s.Close()
		}
	}
	return nil
}

type snapshot struct {
	observed agentapi.SourceObservation
	shards   [][]byte
	closed   bool
}

func (s *snapshot) Observation() agentapi.SourceObservation { return s.observed }

func (s *snapshot) Input() agentapi.NativeInput {
	return agentapi.NativeInput{Records: &records{owner: s}}
}

func (s *snapshot) Close() error { s.closed = true; s.shards = nil; return nil }

type records struct {
	owner *snapshot
	i     int
}

func (r *records) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	if r.owner.closed {
		return agentapi.NativeRecord{}, false, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return agentapi.NativeRecord{}, false, err
	}
	if r.i == len(r.owner.shards) {
		return agentapi.NativeRecord{}, false, nil
	}
	i := r.i
	r.i++
	return agentapi.NativeRecord{Kind: Frame, Key: strconv.Itoa(i), Raw: r.owner.shards[i]}, true, nil
}

// Filter retains independent native pulse fields while removing synthetic credentials.
func (p *Ports) Filter(ctx context.Context, in agentapi.NativeInput, _ agentapi.FilterContext) (archive.FilteredTranscript, error) {
	p.Filters++
	if in.Records == nil || in.File != nil {
		return archive.FilteredTranscript{}, archive.ErrUnsafeSourceFormat
	}
	out := archive.FilteredTranscript{Format: Format, NativeStartComplete: true}
	for {
		record, ok, err := in.Records.Next(ctx)
		if err != nil {
			return out, err
		}
		if !ok {
			break
		}
		if record.Kind != Frame || record.Missing {
			return out, archive.ErrUnsafeSourceFormat
		}
		var pulse NativePulse
		if err := json.Unmarshal(record.Raw, &pulse); err != nil {
			return out, err
		}
		if pulse.PulseKind != "exchange" || (pulse.Speaker != SpeakerPilot && pulse.Speaker != SpeakerOracle) {
			return out, archive.ErrUnsafeSourceFormat
		}
		pulse.Credential = ""
		pulse.Words, _ = archive.RedactSensitive(pulse.Words)
		raw, err := json.Marshal(pulse)
		if err != nil {
			return out, err
		}
		out.Records = append(out.Records, raw)
		out.SessionIDs = append(out.SessionIDs, pulse.NativeIdentity)
		at, err := time.Parse(time.RFC3339, pulse.Clock)
		if err != nil {
			return out, err
		}
		if out.NativeStartAt.IsZero() || at.Before(out.NativeStartAt) {
			out.NativeStartAt = at
		}
		if at.After(out.NativeEndAt) {
			out.NativeEndAt = at
		}
	}
	out.FirstEventAt = out.NativeStartAt
	return out, nil
}

// Refilter applies the current filter to retained evidence without reopening the source.
func (p *Ports) Refilter(ctx context.Context, b archive.SourceBundle, at time.Time) (archive.FilteredTranscript, error) {
	s := &snapshot{}
	for _, raw := range b.NativeRecords {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return archive.FilteredTranscript{}, err
		}
		s.shards = append(s.shards, encoded)
	}
	return p.Filter(ctx, s.Input(), agentapi.FilterContext{StartedAt: at})
}

// EvidenceExtends checks whether the retained pulse sequence preserves the previous prefix.
func (p *Ports) EvidenceExtends(old, next archive.SourceBundle) bool {
	if len(old.NativeRecords) > len(next.NativeRecords) {
		return false
	}
	for i, raw := range old.NativeRecords {
		if !reflect.DeepEqual(raw, next.NativeRecords[i]) {
			return false
		}
	}
	return true
}

// Parser remains independent from the filter's version and source ownership.
type Parser struct{ Owner *Ports }

// Version returns the independently configured synthetic analysis version.
func (p Parser) Version() string {
	if p.Owner.ParserVersion == "" {
		return "orbifold-parser-1"
	}
	return p.Owner.ParserVersion
}

// Parse normalizes retained pulses and preserves unavailable and unknown metrics.
func (p Parser) Parse(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	p.Owner.Parses++
	if err := ctx.Err(); err != nil {
		return archive.Analysis{}, err
	}
	if b.Capture.SourceFormat != Format {
		return archive.Analysis{}, archive.ErrUnsafeSourceFormat
	}
	a := archive.Analysis{Observability: archive.Observability{StructuredCounts: archive.Availability{State: archive.AvailabilityAvailable}, Compactions: archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}, ToolErrors: archive.Availability{State: archive.AvailabilityUnknown, Reason: archive.AvailabilityReasonNotRecorded}, ToolResults: archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}}}
	for i, raw := range b.NativeRecords {
		var pulse NativePulse
		encoded, err := json.Marshal(raw)
		if err != nil {
			return a, err
		}
		if err := json.Unmarshal(encoded, &pulse); err != nil {
			return a, err
		}
		role, kind := "user", archive.TurnKindHumanPrompt
		if pulse.Speaker == SpeakerOracle {
			role, kind = "assistant", archive.TurnKindAssistant
		}
		at, _ := time.Parse(time.RFC3339, pulse.Clock)
		a.View.Turns = append(a.View.Turns, archive.NormalizedTurn{RecordIndex: i, Role: role, Kind: kind, Text: pulse.Words, ID: pulse.NativeIdentity, Timestamp: pulse.Clock})
		if pulse.NativeIdentity != b.NativeSessionID {
			a.Facts.IdentityConflict = true
		}
		if at.After(a.View.LatestRecordAt) {
			a.View.LatestRecordAt = at
		}
	}
	return a, nil
}

// Decode translates synthetic lifecycle signals into shared lifecycle events.
func (p *Ports) Decode(ctx context.Context, in agentapi.HookInput) ([]agentapi.LifecycleEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, _ := in.Payload["opaque"].(string)
	project, _ := in.Payload["checkout"].(string)
	key, _ := in.Payload["manifest"].(string)
	signal, _ := in.Payload["pulse"].(string)
	e := agentapi.LifecycleEvent{Session: agentapi.NativeSession{Agent: ID, NativeID: id}, ProjectRoot: project, Source: agentapi.SourceRef{Path: key}, NativeEvent: signal, Reason: signal}
	switch Signal(signal) {
	case SignalBirth:
		e.Kind = agentapi.EventStart
		e.NewOnly = true
		e.Start = agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: agentapi.FreshnessExplicitStart}
	case SignalResume:
		e.Kind = agentapi.EventStart
		e.NewOnly = true
		e.Start = agentapi.StartEvidence{Kind: agentapi.FreshContinuation, Reason: agentapi.FreshnessContinuation}
	case SignalReply:
		e.Kind = agentapi.EventResponse
	case SignalRest:
		e.Kind = agentapi.EventStop
	default:
		return nil, nil
	}
	return []agentapi.LifecycleEvent{e}, nil
}

// ImportPolicy requires native start evidence for synthetic historical import.
func (p *Ports) ImportPolicy(agentapi.SourceRef) agentapi.ImportPolicy {
	return agentapi.ImportPolicy{Start: agentapi.ImportNativeStart}
}

// InspectImport reports retained pulse identity and native start observations.
func (p *Ports) InspectImport(ctx context.Context, in agentapi.ImportInspectionRequest) (agentapi.ImportInspection, error) {
	if err := ctx.Err(); err != nil {
		return agentapi.ImportInspection{}, err
	}
	out := agentapi.ImportInspection{Conversation: len(in.Filtered.Records) > 0, StartedAt: in.Filtered.NativeStartAt}
	for _, id := range in.Filtered.SessionIDs {
		out.IdentityMismatch = out.IdentityMismatch || id != in.Session.NativeID
	}
	return out, nil
}

// Speaker is the independent synthetic native speaker vocabulary.
type Speaker string

// SpeakerPilot and SpeakerOracle distinguish the two native pulse speakers.
const (
	SpeakerPilot  Speaker = "pilot"
	SpeakerOracle Speaker = "oracle"
)

// Signal is the independent synthetic native lifecycle vocabulary.
type Signal string

// SignalBirth, SignalResume, SignalReply and SignalRest describe synthetic lifecycle transitions.
const (
	SignalBirth  Signal = "birth"
	SignalResume Signal = "resume"
	SignalReply  Signal = "reply"
	SignalRest   Signal = "rest"
)
