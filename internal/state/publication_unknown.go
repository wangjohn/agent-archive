package state

import (
	"bytes"
	"encoding/json"
)

// Future publication fields cannot become a legacy ready transaction simply
// because a configuration is missing or damaged. Reject them in the existing
// decode, without another body read or a second selecting schema.
type unsupportedPublicationField struct{}

func (*unsupportedPublicationField) UnmarshalJSON([]byte) error { return ErrDurableStorageRecovery }

type unsupportedPendingFields struct {
	Sources        unsupportedPublicationField `json:"sources"`
	JournalVersion unsupportedPublicationField `json:"journal_version"`
	Phase          unsupportedPublicationField `json:"phase"`
	Preparation    unsupportedPublicationField `json:"preparation"`
	Progress       unsupportedPublicationField `json:"progress"`
	Cleanup        unsupportedPublicationField `json:"cleanup"`
	AdmissionStage unsupportedPublicationField `json:"admission_stage"`
}

type unsupportedPublishedFields struct {
	PublicationVersion unsupportedPublicationField `json:"publication_version"`
	Commit             unsupportedPublicationField `json:"commit"`
	Sources            unsupportedPublicationField `json:"sources"`
	PredecessorUnknown unsupportedPublicationField `json:"predecessor_unknown"`
}

// UnmarshalJSON refuses foreign publication authority before legacy fallback.
func (p *PendingPublication) UnmarshalJSON(data []byte) error {
	type pendingJSON PendingPublication
	wire := struct {
		*pendingJSON
		unsupportedPendingFields
	}{pendingJSON: (*pendingJSON)(p)}
	return json.Unmarshal(data, &wire)
}

func (p *publishedState) UnmarshalJSON(data []byte) error {
	type publishedJSON publishedState
	wire := struct {
		*publishedJSON
		unsupportedPublishedFields
	}{publishedJSON: (*publishedJSON)(p)}
	return json.Unmarshal(data, &wire)
}

// UnmarshalJSON refuses foreign or incomplete commit descriptors before a
// protected catalog transaction could fall back to ordinary publication.
func (p *CatalogPublication) UnmarshalJSON(data []byte) error {
	type wire CatalogPublication
	var next wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return ErrDurableStorageRecovery
	}
	if next.Protocol != 9 || next.ID == "" || len(next.ID) > 128 || len(next.ExpectedRevision) > 128 {
		return ErrDurableStorageRecovery
	}
	*p = CatalogPublication(next)
	return nil
}
