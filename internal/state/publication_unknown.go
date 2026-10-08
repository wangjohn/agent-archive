package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
	"reflect"
	"strings"
)

// Closed protocols never let future authority fall through to a legacy record.
func closedPublicationDecode(data []byte, dst any, fields ...map[string]bool) error {
	if err := uniquePublicationJSON(json.NewDecoder(bytes.NewReader(data)), 0, reflect.TypeOf(dst), fields...); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ErrDurableStorageRecovery
	}
	return nil
}

func (p *PendingPublication) UnmarshalJSON(data []byte) error {
	type pendingJSON PendingPublication
	var decoded pendingJSON
	fields := make(map[string]bool)
	if err := closedPublicationDecode(data, &decoded, fields); err != nil {
		return err
	}
	*p = PendingPublication(decoded)
	if p.JournalVersion == 2 {
		if fields["source_bytes"] {
			return ErrDurableStorageRecovery
		}
		if len(p.Sources) > 0 && p.Sources[0].Payload.Kind == PublicationInline {
			p.SourceBytes = p.Sources[0].Payload.Inline
		}
		if err := p.validatePublicationEnvelope(); err != nil {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
	} else {
		for _, field := range []string{"journal_version", "phase", "preparation", "progress", "cleanup"} {
			if fields[field] {
				return ErrDurableStorageRecovery
			}
		}
		if p.JournalVersion != 0 || p.Phase != "" || p.Preparation != nil || p.Progress != nil || p.Cleanup != nil || p.Commit != nil && p.Commit.Version != 1 {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

func (p *publishedState) UnmarshalJSON(data []byte) error {
	type publishedJSON publishedState
	var decoded publishedJSON
	fields := make(map[string]bool)
	if err := closedPublicationDecode(data, &decoded, fields); err != nil {
		return err
	}
	*p = publishedState(decoded)
	if p.PublicationVersion == 2 {
		return p.validateSelectingPublished()
	}
	for _, field := range []string{"publication_version", "settled_privacy", "preparation", "payloads", "cleanup", "privacy_receipts"} {
		if fields[field] {
			return ErrDurableStorageRecovery
		}
	}
	// Claimed authority cannot disappear through null/empty JSON values. A
	// complete legacy Commit1 is the only supported legacy selecting variant.
	if fields["commit"] || fields["sources"] || fields["predecessor_unknown"] {
		if p.Commit == nil || p.Commit.Version != 1 || len(p.Sources) == 0 || p.PredecessorUnknown {
			return ErrDurableStorageRecovery
		}
		bundle, _, found := p.resolveLastPublished()
		if !found || len(p.MetadataBytes) == 0 {
			return ErrDurableStorageRecovery
		}
		active := p.Sources[0]
		var metadata archive.Metadata
		if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
		key, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
		if err != nil {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
		sources := make([]PublicationSource, len(p.Sources))
		for i, ref := range p.Sources {
			sources[i].Reference = ref
		}
		pending := PendingPublication{MetadataKey: key, Commit: p.Commit, Sources: sources, Bundle: bundle, SourceKey: active.Key, SourceSHA256: active.SHA256, SourceSize: active.CompressedBytes, MetadataOnly: true, MetadataBytes: p.MetadataBytes}
		if err := pending.validateReadyPublication(); err != nil {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
	}
	if p.PublicationVersion != 0 || p.SettledPrivacy != nil || p.Preparation != nil || len(p.Payloads) != 0 || p.Cleanup != nil || p.Commit != nil && p.Commit.Version != 1 {
		return ErrDurableStorageRecovery
	}
	return nil
}

func (p publishedState) validateSelectingPublished() error {
	derived := p.summary()
	if p.Summary == nil || !reflect.DeepEqual(*p.Summary, derived) {
		return ErrDurableStorageRecovery
	}
	if p.PublicationVersion != 2 || p.Commit == nil || p.Commit.Version != 2 || p.Commit.PayloadSetSHA256 != payloadSetSHA(p.Payloads) || p.Commit.MetadataSHA256 != publicationSHA256(p.MetadataBytes) || p.Commit.PrivacySHA256 != privacyReceiptsSHA(p.PrivacyReceipts) {
		return ErrDurableStorageRecovery
	}
	if (p.Preparation == nil) == (p.SettledPrivacy == nil) {
		return ErrDurableStorageRecovery
	}
	if p.SettledPrivacy != nil {
		if err := p.SettledPrivacy.validate(p); err != nil {
			return err
		}
	} else {
		if p.Commit.SettledPrivacySHA256 != "" || p.Commit.PreparationSHA256 != p.Preparation.SHA256 || p.Preparation.SHA256 != preparationSHA(*p.Preparation) {
			return ErrDurableStorageRecovery
		}
		if err := p.Preparation.validate(); err != nil {
			return err
		}
		if p.Preparation.Migration != nil && p.Preparation.Migration.NextMetadataSHA256 != publicationSHA256(p.MetadataBytes) {
			return ErrDurableStorageRecovery
		}
		if p.Commit.Purpose == PublicationPrivacyRewrite {
			if err := validatePrivacyCorrespondence(*p.Preparation, p.Payloads, p.PrivacyReceipts, p.MetadataBytes, privacyPreviousBody(*p.Preparation)); err != nil {
				return err
			}
		} else if len(p.PrivacyReceipts) > 0 {
			return ErrDurableStorageRecovery
		}
	}
	// The selecting bundle survives cache/block candidates in LastPublished.
	bundle, _, found := p.resolveLastPublished()
	if !found {
		return ErrDurableStorageRecovery
	}
	if len(p.Payloads) == 0 {
		return ErrDurableStorageRecovery
	}
	current := p.Payloads[0].Reference
	commit := *p.Commit
	commit.SettledPrivacySHA256 = ""

	// Published validation uses private Commit2+payload witnesses without a mutable preparation cursor.
	var metadata struct {
		SessionID string `json:"session_id"`
		Harness   struct {
			Name string `json:"name"`
		} `json:"harness"`
	}
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return err
	}
	key, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil {
		return err
	}
	pending := PendingPublication{MetadataKey: key, JournalVersion: 2, Commit: &commit, Sources: p.Payloads, Bundle: bundle, SourceKey: current.Key, SourceSHA256: current.SHA256, SourceSize: current.CompressedBytes, MetadataOnly: true, MetadataBytes: p.MetadataBytes, SourceBytes: p.Payloads[0].Payload.Inline}
	if err := pending.validateReadyPublication(); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

// uniquePublicationJSON rejects duplicate authority keys before typed decoding.
func uniquePublicationJSON(d *json.Decoder, depth int, destination reflect.Type, topFields ...map[string]bool) error {
	for destination != nil && destination.Kind() == reflect.Pointer {
		destination = destination.Elem()
	}
	if depth > 128 {
		return ErrDurableStorageRecovery
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			var child reflect.Type
			// Go's typed JSON decoder accepts case aliases for struct fields.
			// Canonicalize only those fields; arbitrary native map keys remain exact.
			name, child = publicationJSONField(destination, name)
			if depth == 0 && len(topFields) > 0 {
				name = strings.ToLower(name)
			}
			if !ok || keys[name] {
				return ErrDurableStorageRecovery
			}
			keys[name] = true
			if depth == 0 && len(topFields) > 0 {
				topFields[0][name] = true
			}
			if err = uniquePublicationJSON(d, depth+1, child); err != nil {
				return err
			}
		}
	case '[':
		var child reflect.Type
		if destination != nil && (destination.Kind() == reflect.Slice || destination.Kind() == reflect.Array) {
			child = destination.Elem()
		}
		for d.More() {
			if err = uniquePublicationJSON(d, depth+1, child); err != nil {
				return err
			}
		}
	default:
		return ErrDurableStorageRecovery
	}
	_, err = d.Token()
	return err
}

func publicationJSONField(destination reflect.Type, name string) (string, reflect.Type) {
	var child reflect.Type
	if destination != nil && destination.Kind() == reflect.Struct {
		for i := range destination.NumField() {
			field := destination.Field(i)
			if field.PkgPath != "" {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			if tag == "" {
				tag = field.Name
			}
			if strings.EqualFold(name, tag) {
				return tag, field.Type
			}
		}
	} else if destination != nil && destination.Kind() == reflect.Map {
		child = destination.Elem()
	}
	return name, child
}
