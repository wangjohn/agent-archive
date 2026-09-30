package statshtml

import (
	"slices"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

// publicModels is the built-in price table: the only authority on which model
// names are public. A person's own price file (--prices) can add a model of
// their own to the run, but that must not make its name shareable.
var publicModels = sync.OnceValue(stats.DefaultPriceTable)

// modelNamer decides how the model names of one page are shown. A model id can
// name a client (a fine-tune "ft:gpt-4o:acme::abc", a deployment called
// "acme-prod"), so unless real names were asked for, a page shows a model's
// name only when the built-in price table lists it. Any other model is a
// stand-in ("model A", "model B") in the order of the run's own ranking, one
// stand-in per model throughout the page.
type modelNamer struct {
	reveal bool
	stand  *namer
	// rows maps a row's label to the text shown for it, and ids maps each
	// model id of a row to that row's label.
	rows map[string]string
	ids  map[string]string
	// hidden is whether a row of the run was replaced by a stand-in.
	hidden bool
}

// newModelNamer names every row of the run, best-ranked first, so the letters
// follow the ranking and are fixed before any section asks.
func newModelNamer(reveal bool, rows []stats.ModelRow) *modelNamer {
	m := &modelNamer{
		reveal: reveal, stand: newNamer(false, "model"),
		rows: map[string]string{}, ids: map[string]string{},
	}
	for _, r := range rows {
		for _, id := range r.Models {
			m.ids[stats.NormalizeModel(id)] = r.Label
		}
		if reveal || publicRow(r) {
			m.rows[r.Label] = clean(r.Label)
			continue
		}
		m.rows[r.Label] = m.stand.name(r.Label)
		m.hidden = true
	}
	return m
}

// publicRow is whether a row's label and every model id under it are ones the
// built-in price table lists, the label being the family it groups them under.
// A label that came from a person's own price file, or from an id the table
// does not list, is not public. The archive's own placeholders for a model it
// could not name say nothing about anyone's work.
func publicRow(r stats.ModelRow) bool {
	if len(r.Models) == 0 {
		return isPlaceholderModel(r.Label)
	}
	table := publicModels()
	for _, id := range r.Models {
		if isPlaceholderModel(id) && id == r.Label {
			continue
		}
		family, ok := table.Family(id)
		if !ok || family != r.Label {
			return false
		}
	}
	return true
}

func isPlaceholderModel(id string) bool {
	return slices.Contains([]string{archive.UnknownModel, archive.OtherModels}, id)
}

// label is the text shown for a row's label (a cost-by-model row, the
// favorite model). A label no row carries is never shown as it is.
func (m *modelNamer) label(label string) string {
	if text, ok := m.rows[label]; ok {
		return text
	}
	if m.reveal {
		return clean(label)
	}
	return m.stand.name(label)
}

// id is the text shown for a model id the caller named (the --model filter):
// the id itself when the built-in table lists it, else the stand-in of the row
// it belongs to, so it reads the same as in the table, else a stand-in of its
// own.
func (m *modelNamer) id(id string) string {
	if _, ok := publicModels().Family(id); ok || m.reveal {
		return clean(id)
	}
	if row, ok := m.ids[stats.NormalizeModel(id)]; ok {
		return m.label(row)
	}
	return m.stand.name(stats.NormalizeModel(id))
}
