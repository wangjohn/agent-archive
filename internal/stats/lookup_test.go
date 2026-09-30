package stats

// Lookup returns the price of a model id as the archive records it (any
// case, with or without a date or context suffix), and whether the table
// prices it. Only the tests look prices up by id; Compute prices through the
// table's index.
func (t PriceTable) Lookup(model string) (ModelPrice, bool) {
	id := NormalizeModel(model)
	for _, entry := range t.Models {
		if NormalizeModel(entry.ID) == id {
			return entry, true
		}
	}
	return ModelPrice{}, false
}
