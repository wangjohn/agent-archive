package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

func labelScope(reg archive.SessionRegistration, env agentapi.LabelEnvironment) string {
	b, _ := json.Marshal(struct {
		ID, Path, Root, Destination string
		Env                         agentapi.LabelEnvironment
	}{reg.NativeSessionID, reg.TranscriptPath, reg.DiscoveryRoot, reg.DestinationID, env})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (p *pass) observeLabels() {
	if p.opts.Labels == nil {
		return
	}
	provider, ok := p.opts.Labels.LookupLabels("codex")
	if !ok {
		return
	}
	cache, err := p.local.LoadLabels()
	if err != nil {
		addError(p.result.Errors, "session-labels", err)
		cache = state.LabelCache{Version: 1, Entries: map[string]state.LabelEntry{}}
	}
	eligible := map[string]archive.SessionRegistration{}
	ids := []string{}
	for _, reg := range p.registrations {
		if reg.Harness.Name != "codex" || reg.CaptureFrozen || reg.Imported() || p.unreadable[reg.ArchiveSessionID] || (p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg)) {
			continue
		}
		if p.local.GenerationCaptureAllowed(reg) != nil {
			continue
		}
		eligible[reg.ArchiveSessionID] = reg
		ids = append(ids, reg.ArchiveSessionID)
	}
	sort.Strings(ids)
	p.opts.labels = map[string]state.LabelEntry{}
	p.labelStates = map[string]*state.Published{}
	for id, entry := range cache.Entries {
		reg, ok := eligible[id]
		if !ok || entry.Scope != labelScope(reg, p.opts.LabelEnvironment) {
			delete(cache.Entries, id)
			continue
		}
		if entry.Label.State != "" {
			p.opts.labels[id] = entry
		}
	}
	ctx, cancel := context.WithTimeout(p.ctx, 2*time.Second)
	defer cancel()
	requests := p.prepareLabelRequests(ctx, provider, &cache, ids, eligible)
	results := provider.LookupLabels(ctx, p.opts.LabelEnvironment, requests)
	for _, request := range requests {
		id := request.Registration.ArchiveSessionID
		entry := cache.Entries[id]
		label, ok := archive.FilterSessionLabel(results[id])
		if ok && label.NativeID == request.Registration.NativeSessionID {
			// An index miss is weaker than a previously canonical database observation.
			if !(label.Source == "index" && label.State == "confirmed_absent" && entry.Label.Source == "database") {
				if entry.Label.Fingerprint() != label.Fingerprint() {
					entry.Label, entry.ObservedAt = label, p.now
				}
			}
			entry.Failures = 0
			entry.NextAt = p.now.Add(time.Minute)
		} else {
			if entry.Failures < 6 {
				entry.Failures++
			}
			entry.NextAt = p.now.Add(time.Minute * time.Duration(1<<(entry.Failures-1)))
		}
		cache.Entries[id] = entry
		if entry.Label.State != "" {
			p.opts.labels[id] = entry
		}
	}
	// The cache is a bounded set of observations, never a native inventory.
	if len(cache.Entries) > state.MaxLabelCacheEntries {
		keys := make([]string, 0, len(cache.Entries))
		for id := range cache.Entries {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		for _, id := range keys[:len(keys)-state.MaxLabelCacheEntries] {
			delete(cache.Entries, id)
		}
	}
	if err := p.local.SaveLabels(cache); err != nil {
		addError(p.result.Errors, "session-labels", err)
	}
}

func (p *pass) prepareLabelRequests(ctx context.Context, provider agentapi.LabelProvider, cache *state.LabelCache, ids []string, eligible map[string]archive.SessionRegistration) []agentapi.LabelRequest {
	requests := []agentapi.LabelRequest{}
	start := sort.SearchStrings(ids, cache.Cursor)
	if start < len(ids) && ids[start] == cache.Cursor {
		start++
	}
	for n := 0; n < len(ids) && len(requests) < 64 && ctx.Err() == nil; n++ {
		id := ids[(start+n)%len(ids)]
		reg := eligible[id]
		entry := cache.Entries[id]
		cache.Cursor = id
		if entry.NextAt.After(p.now) {
			continue
		}
		checksum, stamp, err := p.local.LabelRevision(id)
		if err != nil {
			continue
		}
		contract := archive.SessionLabelContract + "/" + archive.FilterVersion + "/" + p.opts.parserVersionFor("codex")
		validContext := entry.Context.Contract == contract && entry.Context.NativeID == reg.NativeSessionID && ((checksum != "" && checksum == entry.SourceChecksum) || (checksum == "" && stamp == entry.SourceStamp))
		if !validContext {
			published, n, err := p.local.LoadLabelPublication(id, (16<<20)-p.labelBytes)
			p.labelBytes += n
			if err != nil {
				continue
			}
			p.labelStates[id] = published
			bundle, _, found := published.LastPublished()
			if !found {
				continue
			}
			if bundle.History != nil || bundle.SchemaVersion == archive.HistorySourceSchemaVersion {
				delete(cache.Entries, id)
				delete(p.opts.labels, id)
				continue
			}
			source, known := published.LastPublishedSource()
			if !known {
				continue
			}
			if builder, ok := provider.(agentapi.LabelContextProvider); ok {
				entry.Context = builder.LabelContext(bundle)
			} else {
				entry.Context = agentapi.LabelContext{NativeID: bundle.NativeSessionID, Ordinary: bundle.History == nil && bundle.SchemaVersion == archive.SourceSchemaVersion, Producer: bundle.Capture.Harness.Version}
			}
			entry.Context.Contract = contract
			if entry.Context.Producer != "0.159.2" {
				entry.Context.Producer = ""
			}
			entry.SourceChecksum = source.SHA256
			entry.SourceStamp = stamp
			if entry.Label.State == "" {
				if label, at, ok := archive.CurrentSessionLabel(bundle); ok {
					entry.Label, entry.ObservedAt = label, at
				}
			}
		}
		requests = append(requests, agentapi.LabelRequest{Registration: reg, Context: entry.Context})
		entry.Scope = labelScope(reg, p.opts.LabelEnvironment)
		cache.Entries[id] = entry
	}
	return requests
}

func (s *sessionScan) applyLabels(evidence []archive.SupplementalEvidence) []archive.SupplementalEvidence {
	entry, ok := s.opts.labels[s.id()]
	if !ok {
		return evidence
	}
	return archive.MergeSupplementalEvidence(evidence, []archive.SupplementalEvidence{entry.Label.Evidence(entry.ObservedAt)})
}

func labelFingerprint(bundle archive.SourceBundle) string {
	label, _, ok := archive.CurrentSessionLabel(bundle)
	if !ok {
		return ""
	}
	return label.Fingerprint()
}

// refreshLabels publishes changed external evidence from the retained source.
// It does not read a transcript or renew conversation activity/retention time.
func (s *sessionScan) refreshLabels() (sessionOutcome, bool, error) {
	entry, ok := s.opts.labels[s.id()]
	if !ok || s.req.Token != "" {
		return outcomeSkipped, false, nil
	}
	key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	if err != nil {
		return outcomeSkipped, false, err
	}
	last, found := s.lastPublication(key)
	if !found || last.bundle.History != nil || !sourceEvidenceWithinPolicy(last.bundle.SupplementalEvidence, s.opts.skillEvidence()) || labelFingerprint(last.bundle) == entry.Label.Fingerprint() {
		return outcomeSkipped, false, nil
	}
	reader, hasReader := newSourceReader(s.reg, s.opts)
	if hasReader {
		_, err := reader.Signature(s.ctx)
		if err == nil && last.bundle.Capture.FilterVersion != archive.FilterVersion {
			// Refilter available native input normally before refreshing its
			// names; retained missing input keeps its original filter provenance.
			return outcomeSkipped, false, nil
		}
		if err == nil && !s.sourceSettled(reader) {
			return outcomeSkipped, false, nil
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// Only known missing native input permits retained-source refresh.
			if !errors.Is(err, os.ErrNotExist) {
				return outcomeSkipped, false, nil
			}
		} else if err != nil {
			return outcomeSkipped, false, err
		}
	}
	// The narrow summary is only a lookup fast path. Before source-backed
	// publication, verify all retained bytes against the committed reference.
	compressed, err := archive.BuildCompressedSource(last.bundle)
	if err != nil || compressed.SHA256 != last.metadata.SourceBundle.SHA256 || len(compressed.Bytes) != last.metadata.SourceBundle.CompressedBytes {
		return outcomeSkipped, true, errors.New("retained source cannot be verified for session name refresh")
	}
	candidate := last.bundle
	candidate.SupplementalEvidence = s.applyLabels(candidate.SupplementalEvidence)
	if err := archive.ValidateSessionLabels(candidate); err != nil {
		return outcomeSkipped, false, err
	}
	observed := missingSource(s.reg)
	if hasReader {
		if current, err := reader.Signature(s.ctx); err == nil {
			observed = &current
		}
	}
	outcome, err := s.publish(sourceRead{observed: *observed}, candidate)
	return outcome, true, err
}

func (s *sessionScan) publishedLabel() string {
	bundle, _, _ := s.published.LastPublished()
	return labelFingerprint(bundle)
}
