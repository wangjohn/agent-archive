// Package machines publishes informational machine records. Records are bucket
// claims, never evidence of exclusive ownership or authorization to revoke keys.
package machines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/revocation"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// SchemaVersion identifies the supported informational record format.
const SchemaVersion = 1

// MaxRecordBytes bounds allocations from untrusted record bodies.
const MaxRecordBytes = 16 << 10

// Timeout is the shared listing or publication budget.
const Timeout = 5 * time.Second

const maxContinuationBytes = 16 << 10

// CredentialBinding records claimed provenance without a credential secret.
type CredentialBinding struct {
	Kind        config.MachineAssignmentKind `json:"kind"`
	AccessKeyID string                       `json:"access_key_id,omitempty"`
	RecipientID string                       `json:"recipient_id,omitempty"`
	IssuerID    string                       `json:"issuer_id,omitempty"`
	SlotID      string                       `json:"slot_id,omitempty"`
	SharedWith  string                       `json:"shared_with,omitempty"`
}

// Record describes one machine, without paths or session content.
type Record struct {
	CredentialHistoryPartial bool                `json:"credential_history_partial,omitempty"`
	UnusedSpares             []CredentialBinding `json:"unused_spares,omitempty"`
	RetiredCredentials       []CredentialBinding `json:"retired_credentials,omitempty"`
	SchemaVersion            int                 `json:"schema_version"`
	MachineID                string              `json:"machine_id"`
	Name                     string              `json:"name"`
	Platform                 string              `json:"platform"`
	AgentArchiveVersion      string              `json:"agent_archive_version"`
	PairedAt                 *time.Time          `json:"paired_at,omitempty"`
	PairedFrom               string              `json:"paired_from,omitempty"`
	PairingID                string              `json:"pairing_id,omitempty"`
	Credential               CredentialBinding   `json:"credential"`
	HeartbeatAt              time.Time           `json:"heartbeat_at"`
}

// Build constructs a record only from local committed settings. Stale
// destination assignments are ignored; manual R2 keys always remain unknown.
func Build(cfg config.Config, platform, version, accessKeyID string, now time.Time) (Record, error) {
	r := Record{SchemaVersion: SchemaVersion, MachineID: cfg.MachineID, Name: cfg.MachineName, Platform: platform, AgentArchiveVersion: version, HeartbeatAt: now.UTC()}
	if r.Name == "" && config.ValidMachineID(cfg.MachineID) {
		r.Name = "unnamed-" + cfg.MachineID[:4]
	}
	r.Credential.Kind = map[string]config.MachineAssignmentKind{
		credentials.ProviderS3: config.MachineAssignmentAWSProfile,
		credentials.ProviderR2: config.MachineAssignmentR2Unknown,
	}[cfg.Storage.Provider]
	if r.Credential.Kind == "" {
		return r, errors.New("storage provider is not configured")
	}
	if cfg.Storage.Provider == credentials.ProviderR2 {
		r.Credential.AccessKeyID = accessKeyID
	}

	if a := cfg.MachineAssignment; a != nil && a.DestinationID == cfg.DestinationID() {
		if err := cfg.ValidateMachine(); err != nil {
			return r, err
		}
		if (cfg.Storage.Provider == credentials.ProviderS3) != (a.Kind == config.MachineAssignmentAWSProfile) {
			return r, errors.New("credential assignment does not match storage provider")
		}
		r.Credential = CredentialBinding{Kind: a.Kind, AccessKeyID: a.AccessKeyID, RecipientID: a.RecipientID, IssuerID: a.IssuerID, SlotID: a.SlotID, SharedWith: a.SharedWith}
		r.PairedAt, r.PairedFrom, r.PairingID = a.PairedAt, a.PairedFrom, a.PairingID
	}
	for _, a := range cfg.RetiredMachineAssignments {
		if a.DestinationID == cfg.DestinationID() {
			if len(r.RetiredCredentials) >= 16 {
				r.CredentialHistoryPartial = true
				continue
			}
			r.RetiredCredentials = append(r.RetiredCredentials, CredentialBinding{Kind: a.Kind, AccessKeyID: a.AccessKeyID, RecipientID: a.RecipientID, IssuerID: a.IssuerID, SlotID: a.SlotID})
		}
	}
	return r, validate(r)
}

func validate(r Record) error {
	if r.SchemaVersion != SchemaVersion {
		return errors.New("unsupported schema version")
	}
	if !config.ValidMachineID(r.MachineID) || !config.ValidMachineName(r.Name) {
		return errors.New("invalid machine identity or name")
	}
	if !config.SafeMachineText(r.Platform, 80) || !config.SafeMachineText(r.AgentArchiveVersion, 128) || r.Platform == "" || r.AgentArchiveVersion == "" {
		return errors.New("invalid platform or version")
	}
	a := config.MachineAssignment{DestinationID: strings.Repeat("0", 64), Kind: r.Credential.Kind, AccessKeyID: r.Credential.AccessKeyID, RecipientID: r.Credential.RecipientID, IssuerID: r.Credential.IssuerID, SlotID: r.Credential.SlotID, SharedWith: r.Credential.SharedWith, PairedFrom: r.PairedFrom, PairingID: r.PairingID, PairedAt: r.PairedAt}
	if err := (config.Config{MachineAssignment: &a}).ValidateMachine(); err != nil {
		return errors.New("unsupported or invalid credential claim")
	}
	if len(r.UnusedSpares) > 5 || len(r.RetiredCredentials) > 16 {
		return errors.New("too many credential claims")
	}
	seen := map[string]bool{}
	for _, b := range append(append([]CredentialBinding{}, r.UnusedSpares...), r.RetiredCredentials...) {
		if b.Kind != config.MachineAssignmentR2Own || !config.ValidMachineID(b.AccessKeyID) || !config.ValidMachineID(b.RecipientID) || !config.ValidMachineID(b.IssuerID) || !config.ValidMachineID(b.SlotID) || seen[b.AccessKeyID] {
			return errors.New("invalid credential history claim")
		}
		seen[b.AccessKeyID] = true
	}
	for _, spare := range r.UnusedSpares {
		if spare.IssuerID != r.MachineID {
			return errors.New("unused spare issuer mismatch")
		}
	}
	if r.HeartbeatAt.IsZero() {
		return errors.New("missing heartbeat time")
	}
	return nil
}

// Fingerprint identifies record content independently of its daily heartbeat.
func Fingerprint(r Record) string {
	r.HeartbeatAt = time.Time{}
	b, _ := json.Marshal(r)
	return storage.SHA256Hex(b)
}

// Publish replaces the local machine's record under a bounded context.
func Publish(ctx context.Context, store storage.ObjectStore, r Record) error {
	if err := validate(r); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > MaxRecordBytes {
		return storage.ErrObjectTooLarge
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	return store.Put(ctx, "machines/"+r.MachineID+".json", b)
}

// Unreadable identifies an omitted object with a bounded, control-safe reason.
type Unreadable struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// ListResult preserves successful records when listing is incomplete.
type ListResult struct {
	Revocations      []revocation.Journal `json:"revocations,omitempty"`
	SchemaVersion    int                  `json:"schema_version"`
	Records          []Record             `json:"records"`
	Unreadable       []Unreadable         `json:"unreadable,omitempty"`
	Partial          bool                 `json:"partial"`
	ProviderVerified bool                 `json:"provider_verified"`
}

func diagnostic(key, reason string) Unreadable {
	if len(key) > 160 {
		key = key[:160]
	}
	// JSON quoting prevents control characters or invalid bytes reaching a terminal.
	return Unreadable{Key: strconv.QuoteToASCII(key), Reason: reason}
}

// List reads machine records and informational progress with one five-second deadline, at most
// 1000 pages or examined keys and four concurrent allocation-bounded fetches. Stores
// lacking the required extensions are refused rather than read unboundedly.
func List(ctx context.Context, store storage.ObjectStore) ListResult {
	result := ListResult{SchemaVersion: SchemaVersion, Records: []Record{}}
	pages, ok := store.(storage.PageLister)
	getter, limited := store.(storage.LimitedGetter)
	if !ok || !limited {
		return ListResult{SchemaVersion: SchemaVersion, Records: []Record{}, Partial: true, Unreadable: []Unreadable{diagnostic("machines/", "store does not support bounded listing and reads")}}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	token := ""
	seen := map[string]bool{}
	examined := 0
	seenKeys := map[string]bool{}
	for pageCount := 0; ; pageCount++ {
		if pageCount >= 1000 {
			result.Partial = true
			result.Unreadable = append(result.Unreadable, diagnostic("machines/", "listing page cap reached"))
			break
		}
		page, err := pages.ListPage(ctx, "machines/", token, int32(min(100, 1000-examined)))
		if err != nil {
			result.Partial = true
			result.Unreadable = append(result.Unreadable, diagnostic("machines/", "listing failed or timed out"))
			break
		}
		objects := page.Objects
		if len(objects) > 1000-examined {
			objects = objects[:1000-examined]
			result.Partial = true
		}
		examined += len(objects)

		got := make([]fetchedMachineObject, len(objects))
		var wg sync.WaitGroup
		slots := make(chan struct{}, 4)
		for i, obj := range objects {
			if seenKeys[obj.Key] {
				got[i] = badMachineObject(obj.Key, "duplicate listed key")
				continue
			}
			seenKeys[obj.Key] = true
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				got[i] = badMachineObject(obj.Key, "read timed out")
				continue
			}
			wg.Go(func() { defer func() { <-slots }(); got[i] = fetchMachineObject(ctx, getter, obj) })
		}
		wg.Wait()
		for _, f := range got {
			if f.problem != nil {
				result.Unreadable = append(result.Unreadable, *f.problem)
			} else if f.operation != nil {
				result.Revocations = append(result.Revocations, *f.operation)
			} else {
				result.Records = append(result.Records, f.record)
			}
		}
		if ctx.Err() != nil {
			result.Partial = true
			break
		}
		if page.Next == "" {
			break
		}
		if len(page.Next) > maxContinuationBytes {
			result.Partial = true
			result.Unreadable = append(result.Unreadable, diagnostic("machines/", "continuation token exceeds 16 KiB"))
			break
		}
		nextFingerprint := storage.SHA256Hex([]byte(page.Next))
		if examined >= 1000 || seen[nextFingerprint] {
			result.Partial = true
			result.Unreadable = append(result.Unreadable, diagnostic("machines/", "listing cap reached or continuation repeated"))
			break
		}
		seen[nextFingerprint] = true
		token = page.Next
	}
	if len(result.Unreadable) > 0 {
		result.Partial = true
	}
	sort.Slice(result.Records, func(i, j int) bool {
		a, b := result.Records[i], result.Records[j]
		if a.Name == b.Name {
			return a.MachineID < b.MachineID
		}
		return a.Name < b.Name
	})
	return result
}

// Select resolves a bucket label only when unique; IDs disambiguate duplicates.
func Select(records []Record, query string) (Record, error) {
	var matches []Record
	for _, r := range records {
		if r.MachineID == query {
			return r, nil
		}
		if r.Name == query {
			matches = append(matches, r)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Record{}, fmt.Errorf("machine name is ambiguous; use its full machine ID")
	}
	return Record{}, errors.New("machine not found; list machines and use its full machine ID")
}

type fetchedMachineObject struct {
	record    Record
	operation *revocation.Journal
	problem   *Unreadable
}

func badMachineObject(key, reason string) fetchedMachineObject {
	d := diagnostic(key, reason)
	return fetchedMachineObject{problem: &d}
}
func fetchMachineObject(ctx context.Context, getter storage.LimitedGetter, obj storage.Object) fetchedMachineObject {
	const progressPrefix = "machines/revocations/"
	prefix := "machines/"
	limit := int64(MaxRecordBytes)
	progress := strings.HasPrefix(obj.Key, progressPrefix)
	if progress {
		prefix = progressPrefix
		limit = 65536
	}
	id := strings.TrimSuffix(strings.TrimPrefix(obj.Key, prefix), ".json")
	if obj.Key != prefix+id+".json" || !config.ValidMachineID(id) {
		return badMachineObject(obj.Key, "invalid record or progress path")
	}
	if obj.Size > limit {
		return badMachineObject(obj.Key, "record or progress exceeds body limit")
	}
	raw, err := getter.GetLimited(ctx, obj.Key, limit)
	if err != nil {
		return badMachineObject(obj.Key, "record or progress unreadable or oversized")
	}
	if progress {
		var j revocation.Journal
		if json.Unmarshal(raw, &j) != nil || j.OperationID != id || j.Validate() != nil {
			return badMachineObject(obj.Key, "invalid or unreadable progress claim")
		}
		return fetchedMachineObject{operation: &j}
	}
	var r Record
	if json.Unmarshal(raw, &r) != nil || validate(r) != nil || r.MachineID != id {
		return badMachineObject(obj.Key, "invalid, unsupported, or mismatched record")
	}
	return fetchedMachineObject{record: r}
}
