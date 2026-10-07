package state

import "encoding/json"

// Future publication fields cannot become a legacy ready transaction simply
// because a configuration is missing or damaged. Reject them in the existing
// decode, without another body read or a second selecting schema.
type unsupportedPublicationField struct{}

func (*unsupportedPublicationField) UnmarshalJSON([]byte) error { return ErrDurableStorageRecovery }

type unsupportedPendingFields struct {
	Commit         unsupportedPublicationField `json:"commit"`
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
